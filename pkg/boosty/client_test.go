package boosty

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(handler http.Handler) *Client {
	server := httptest.NewServer(handler)
	return &Client{
		Tokens: &Tokens{
			AccessToken: "test-token",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		},
		HTTP: server.Client(),
		Log:  discardLogger{},
	}
}

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
		Tokens: &Tokens{AccessToken: "test"},
		HTTP:   server.Client(),
		Log:    discardLogger{},
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
		Tokens: &Tokens{AccessToken: "test"},
		HTTP:   server.Client(),
		Log:    discardLogger{},
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
		Tokens: &Tokens{AccessToken: "test"},
		HTTP:   server.Client(),
		Log:    discardLogger{},
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
