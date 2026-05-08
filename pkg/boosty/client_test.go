package boosty

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetJSON_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing auth header")
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()

	c := &Client{
		Tokens: &Tokens{AccessToken: "test-token", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		HTTP:   server.Client(),
		Log:    discardLogger{},
	}

	var result map[string]string
	err := c.GetJSON(server.URL+"/test", &result)
	if err != nil {
		t.Fatalf("GetJSON error: %v", err)
	}
	if result["status"] != "ok" {
		t.Errorf("status = %q, want 'ok'", result["status"])
	}
}

func TestGetJSON_RetryOnNetworkError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping retry test in short mode (takes ~20s due to backoff delays)")
	}

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n <= 2 {
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()

	c := &Client{
		Tokens: &Tokens{AccessToken: "test-token", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		HTTP:   server.Client(),
		Log:    discardLogger{},
	}

	var result map[string]string
	err := c.GetJSON(server.URL+"/test", &result)
	if err != nil {
		t.Fatalf("GetJSON should succeed after retries: %v", err)
	}
	if atomic.LoadInt32(&attempts) < 3 {
		t.Errorf("expected at least 3 attempts, got %d", attempts)
	}
}

func TestGetJSON_ReturnsErrorOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not_found"}`)
	}))
	defer server.Close()

	c := &Client{
		Tokens: &Tokens{AccessToken: "test-token", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		HTTP:   server.Client(),
		Log:    discardLogger{},
	}

	var result map[string]string
	err := c.GetJSON(server.URL+"/test", &result)
	if err == nil {
		t.Fatal("GetJSON should return error on 404")
	}
}

func TestDownloadFile_Success(t *testing.T) {
	content := "hello world file content"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, content)
	}))
	defer server.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	err := c.DownloadFile(server.URL+"/file", path)
	if err != nil {
		t.Fatalf("DownloadFile error: %v", err)
	}

	data, _ := os.ReadFile(path)
	if string(data) != content {
		t.Errorf("file content = %q, want %q", string(data), content)
	}
}

func TestDownloadFile_SkipsExistingFile(t *testing.T) {
	var called int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		fmt.Fprint(w, "new content")
	}))
	defer server.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	os.WriteFile(path, []byte("old content"), 0644)

	err := c.DownloadFile(server.URL+"/file", path)
	if err != nil {
		t.Fatalf("DownloadFile error: %v", err)
	}

	if atomic.LoadInt32(&called) != 0 {
		t.Error("should not have made HTTP request for existing file")
	}

	data, _ := os.ReadFile(path)
	if string(data) != "old content" {
		t.Errorf("file should not be overwritten, got %q", string(data))
	}
}

func TestDownloadFile_RedownloadsZeroByteFile(t *testing.T) {
	content := "fresh content"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, content)
	}))
	defer server.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	os.WriteFile(path, []byte{}, 0644)

	err := c.DownloadFile(server.URL+"/file", path)
	if err != nil {
		t.Fatalf("DownloadFile error: %v", err)
	}

	data, _ := os.ReadFile(path)
	if string(data) != content {
		t.Errorf("file content = %q, want %q", string(data), content)
	}
}

// downloadOnce must write to <path>.tmp and rename on success so a crash mid-copy
// cannot poison the final path. These tests pin that contract: a regression that
// removes the rename or skips the .tmp cleanup will fail here.

func TestDownloadOnce_SuccessLeavesNoTmp(t *testing.T) {
	content := "successful download payload"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, content)
	}))
	defer server.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "video.mp4")
	if err := c.downloadOnce(server.URL+"/file", path); err != nil {
		t.Fatalf("downloadOnce error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
	if string(data) != content {
		t.Errorf("file content = %q, want %q", string(data), content)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected %s.tmp to be absent after success, stat err = %v", path, err)
	}
}

func TestDownloadOnce_MidStreamFailureLeavesNoFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Advertise a content length far larger than what is actually written,
		// then hijack and close the connection. io.Copy sees ErrUnexpectedEOF
		// and downloadOnce takes the copy-error branch.
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial"))
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("server does not support hijacking")
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer server.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "video.mp4")
	err := c.downloadOnce(server.URL+"/file", path)
	if err == nil {
		t.Fatal("downloadOnce should fail on mid-stream connection drop")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be absent (no half-written file), stat err = %v", path, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected %s.tmp to be cleaned up after copy error, stat err = %v", path, err)
	}
}

func TestDownloadOnce_HTTPErrorLeavesNoTmp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "video.mp4")
	err := c.downloadOnce(server.URL+"/file", path)
	if err == nil {
		t.Fatal("downloadOnce should fail on 403")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be absent, stat err = %v", path, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected %s.tmp to be absent (created lazily after status check), stat err = %v", path, err)
	}
}

// TestDownloadOnce_OverwritesStaleTmp ensures a leftover .tmp from a previous
// crashed run is overwritten rather than appended to, so the bytes on disk
// after a successful retry match the server response exactly.
func TestDownloadOnce_OverwritesStaleTmp(t *testing.T) {
	content := "fresh payload"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, content)
	}))
	defer server.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(path+".tmp", []byte("stale partial bytes from prior crash"), 0644); err != nil {
		t.Fatalf("seed stale .tmp: %v", err)
	}

	if err := c.downloadOnce(server.URL+"/file", path); err != nil {
		t.Fatalf("downloadOnce error: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != content {
		t.Errorf("file content = %q, want %q (stale bytes from .tmp leaked into final path)", string(data), content)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected %s.tmp to be absent after success, stat err = %v", path, err)
	}
}

// CommentsURL must include reply_limit so the server inlines replies. Without
// the param the server returns replies.data=[] regardless of replyCount, which
// silently dropped every reply body before this fix.
func TestCommentsURL_IncludesReplyLimit(t *testing.T) {
	got := CommentsURL("someblog", "post-id-123", 50, 0)
	if !strings.Contains(got, "reply_limit=100") {
		t.Errorf("CommentsURL = %q, missing reply_limit=100", got)
	}
	if !strings.Contains(got, "limit=50") {
		t.Errorf("CommentsURL = %q, missing limit=50", got)
	}
	if !strings.Contains(got, "offset=0") {
		t.Errorf("CommentsURL = %q, missing offset=0", got)
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{500, "500 B"},
		{1024 * 1024, "1.0 MB"},
		{1536 * 1024, "1.5 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{int64(2.5 * 1024 * 1024 * 1024), "2.5 GB"},
	}
	for _, tt := range tests {
		got := FormatSize(tt.bytes)
		if got != tt.want {
			t.Errorf("FormatSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}
