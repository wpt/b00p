package boosty

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rewriteTransport reroutes every request to the test server regardless of
// host. BaseURL is a const, so requests built against it (token refresh at
// api.boosty.to/oauth/token/) cannot otherwise be pointed at a fixture.
// Mirrors the syncer test harness's apiRewriteTransport.
type rewriteTransport struct{ target *url.URL }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = rt.target.Scheme
	clone.URL.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func rewriteClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	return &http.Client{Transport: rewriteTransport{target: u}}
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

// A 200 whose body dies mid-read (conn reset, stalled stream) is the same
// transient network flake as a pre-header failure and must keep the retry
// schedule — a single json.Decoder call would conflate it with malformed
// JSON and fail the whole run on one dropped connection.
func TestGetJSON_MidBodyCutRetries(t *testing.T) {
	saved := RetryDelays
	RetryDelays = []time.Duration{time.Millisecond}
	defer func() { RetryDelays = saved }()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			// Promise more bytes than we send, then kill the connection:
			// the client sees a mid-body unexpected EOF.
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			hj := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer server.Close()

	c := &Client{
		Tokens: &Tokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		HTTP:   server.Client(),
		Log:    discardLogger{},
	}

	var result map[string]string
	if err := c.GetJSON(server.URL+"/test", &result); err != nil {
		t.Fatalf("GetJSON should recover from a mid-body cut: %v", err)
	}
	if result["status"] != "ok" {
		t.Errorf("status = %q, want 'ok'", result["status"])
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// Malformed JSON in a complete 200 body is deterministic — the same bytes
// re-download on every attempt — so it fails fast, and the error names the
// URL (a bare "invalid character ..." would not say which endpoint died).
func TestGetJSON_MalformedJSONFailsFast(t *testing.T) {
	saved := RetryDelays
	RetryDelays = []time.Duration{time.Millisecond}
	defer func() { RetryDelays = saved }()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		fmt.Fprint(w, "<html>not json</html>")
	}))
	defer server.Close()

	c := &Client{
		Tokens: &Tokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		HTTP:   server.Client(),
		Log:    discardLogger{},
	}

	var result map[string]string
	err := c.GetJSON(server.URL+"/test", &result)
	if err == nil {
		t.Fatal("GetJSON should fail on malformed JSON")
	}
	if !strings.Contains(err.Error(), server.URL+"/test") {
		t.Errorf("error %q does not name the URL", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (deterministic verdict, no retries)", got)
	}
}

func TestGetJSON_RetryOnNetworkError(t *testing.T) {
	saved := RetryDelays
	RetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { RetryDelays = saved }()

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
	if err := c.downloadOnce(DownloadRequest{URL: server.URL + "/file", Path: path}); err != nil {
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

func TestDownloadOnce_MidStreamFailureKeepsPartialTmp(t *testing.T) {
	// Mid-stream connection drop should leave the bytes-we-got in .tmp so the
	// next retry can resume via Range: bytes=N- instead of restarting from
	// byte 0. The final path stays absent (rename only happens on success).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial"))
		// Flush so the partial bytes reach the client before we hijack and
		// kill the connection — otherwise the response writer's buffer may
		// drop them on hijack and io.Copy sees zero bytes.
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
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
	err := c.downloadOnce(DownloadRequest{URL: server.URL + "/file", Path: path})
	if err == nil {
		t.Fatal("downloadOnce should fail on mid-stream connection drop")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be absent (rename only on success), stat err = %v", path, err)
	}
	info, statErr := os.Stat(path + ".tmp")
	if statErr != nil {
		t.Fatalf("expected partial %s.tmp to remain for resume, stat err = %v", path, statErr)
	}
	if info.Size() == 0 {
		t.Errorf("expected partial %s.tmp to hold the bytes we received, got 0", path)
	}
	// The sidecar must exist alongside the partial and pin the object key
	// (the URL, absent an explicit Key) and the advertised total — without
	// it the next attempt treats the tmp as orphaned and restarts from byte
	// 0, silently disabling resume.
	sidecar, scErr := os.ReadFile(path + ".tmp.url")
	if scErr != nil {
		t.Fatalf("expected resume sidecar %s.tmp.url next to the partial: %v", path, scErr)
	}
	if sc := parseResumeSidecar(sidecar); sc.Key != server.URL+"/file" || sc.Total != 1000000 {
		t.Errorf("sidecar = %+v, want key %q and total 1000000", sc, server.URL+"/file")
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
	err := c.downloadOnce(DownloadRequest{URL: server.URL + "/file", Path: path})
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

	if err := c.downloadOnce(DownloadRequest{URL: server.URL + "/file", Path: path}); err != nil {
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

// --- Range-resume protocol ---
//
// The .tmp + .tmp.url pair implements crash-safe resume: bytes already on
// disk are reused ONLY when the sidecar pins the same URL (otherwise head-of-
// URL-A + tail-of-URL-B would concatenate into silent corruption that
// --check-media cannot detect), the server must answer 206 starting at the
// exact requested offset, and 200 / 416 reset cleanly. These tests pin every
// branch of that protocol.

func TestDownloadOnce_ResumesVia206(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=4-" {
			t.Errorf("Range = %q, want bytes=4- (resume from the 4-byte partial)", got)
		}
		w.Header().Set("Content-Range", "bytes 4-7/8")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "tail")
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
	dlURL := server.URL + "/file"
	if err := os.WriteFile(path+".tmp", []byte("head"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp.url", []byte(dlURL), 0644); err != nil {
		t.Fatal(err)
	}

	if err := c.downloadOnce(DownloadRequest{URL: dlURL, Path: path}); err != nil {
		t.Fatalf("downloadOnce: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "headtail" {
		t.Errorf("file = %q, want 'headtail' (head from tmp + tail from 206)", data)
	}
	for _, leftover := range []string{path + ".tmp", path + ".tmp.url"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("expected %s to be gone after success, stat err = %v", leftover, err)
		}
	}
}

// The sidecar is keyed on DownloadRequest.Key, not the URL: a signed okcdn
// URL changes on every refresh, so a partial from a crashed run must still
// resume when the next run downloads the same object through a new URL.
func TestDownloadOnce_ResumesAcrossURLChangeWithSameKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=4-" {
			t.Errorf("Range = %q, want bytes=4- (same key must resume)", got)
		}
		w.Header().Set("Content-Range", "bytes 4-7/8")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "tail")
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
	if err := os.WriteFile(path+".tmp", []byte("head"), 0644); err != nil {
		t.Fatal(err)
	}
	// Recorded against a URL that no longer exists; key and total still match.
	if err := os.WriteFile(path+".tmp.url", resumeSidecar{Key: "video-42", Total: 8}.encode(), 0644); err != nil {
		t.Fatal(err)
	}

	err := c.downloadOnce(DownloadRequest{URL: server.URL + "/refreshed?sig=new", Path: path, Key: "video-42"})
	if err != nil {
		t.Fatalf("downloadOnce: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "headtail" {
		t.Errorf("file = %q, want 'headtail'", data)
	}
}

// Same key but a different advertised total: the object was re-encoded
// between runs, so the partial must be dropped and the retry restarts at 0.
func TestDownloadOnce_TotalMismatchDropsPair(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 4-7/8")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "tail")
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
	if err := os.WriteFile(path+".tmp", []byte("head"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp.url", resumeSidecar{Key: "video-42", Total: 999}.encode(), 0644); err != nil {
		t.Fatal(err)
	}

	err := c.downloadOnce(DownloadRequest{URL: server.URL + "/file", Path: path, Key: "video-42"})
	if err == nil {
		t.Fatal("downloadOnce should fail when the advertised total differs from the recorded one")
	}
	if errors.Is(err, errNonRetriable) {
		t.Errorf("error %v must stay retriable (the pair was dropped, the retry restarts from 0)", err)
	}
	for _, leftover := range []string{path + ".tmp", path + ".tmp.url"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("expected %s to be dropped, stat err = %v", leftover, err)
		}
	}
}

// A 206 may end before the object does (some CDNs cap ranges). Its
// Content-Length matches the short range, so net/http reports a clean EOF and
// only the Content-Range total shows the file is incomplete; a chunked 206
// has no Content-Length at all, and "bytes N-M/*" names no total, so only
// the sidecar's does. The prefix must stay as tmp + sidecar, and later
// attempts resume from the new offset until the object is complete.
func TestDownload_Short206KeepsPrefixAndResumes(t *testing.T) {
	const object = "0123456789abcdefghij"
	for _, tc := range []struct {
		name                  string
		chunked, unknownTotal bool
	}{
		{name: "content-length"},
		{name: "chunked", chunked: true},
		{name: "unknown total", unknownTotal: true},
	} {
		chunked := tc.chunked
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var ranges []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rng := r.Header.Get("Range")
				mu.Lock()
				ranges = append(ranges, rng)
				mu.Unlock()
				start, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
				if err != nil {
					t.Errorf("Range = %q, want bytes=N-", rng)
					return
				}
				end := min(start+4, len(object))
				total := strconv.Itoa(len(object))
				if tc.unknownTotal {
					total = "*"
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%s", start, end-1, total))
				if !chunked {
					w.Header().Set("Content-Length", strconv.Itoa(end-start))
				}
				w.WriteHeader(http.StatusPartialContent)
				if chunked {
					w.(http.Flusher).Flush()
				}
				fmt.Fprint(w, object[start:end])
			}))
			defer server.Close()

			c := &Client{
				Tokens:       &Tokens{AccessToken: "test"},
				HTTP:         server.Client(),
				DownloadHTTP: server.Client(),
				Log:          discardLogger{},
			}
			path := filepath.Join(t.TempDir(), "video.mp4")
			if err := os.WriteFile(path+".tmp", []byte(object[:4]), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".tmp.url", resumeSidecar{Key: "video-42", Total: 20}.encode(), 0644); err != nil {
				t.Fatal(err)
			}
			req := DownloadRequest{URL: server.URL + "/file", Path: path, Key: "video-42"}

			err := c.downloadOnce(req)
			if err == nil {
				t.Fatal("downloadOnce accepted a 206 that ended at byte 8 of 20")
			}
			if errors.Is(err, errNonRetriable) {
				t.Errorf("error %v must stay retriable so the next attempt resumes", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("truncated file renamed into place, stat err = %v", err)
			}
			if data, _ := os.ReadFile(path + ".tmp"); string(data) != object[:8] {
				t.Errorf("tmp = %q, want %q kept for resume", data, object[:8])
			}
			data, _ := os.ReadFile(path + ".tmp.url")
			if sc := parseResumeSidecar(data); sc != (resumeSidecar{Key: "video-42", Total: 20}) {
				t.Errorf("sidecar = %+v, want key video-42 and the Content-Range total 20", sc)
			}

			saved := RetryDelays
			RetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
			defer func() { RetryDelays = saved }()
			if err := c.Download(req); err != nil {
				t.Fatalf("Download: %v", err)
			}
			if data, _ := os.ReadFile(path); string(data) != object {
				t.Errorf("file = %q, want %q", data, object)
			}
			mu.Lock()
			defer mu.Unlock()
			if want := []string{"bytes=4-", "bytes=8-", "bytes=12-", "bytes=16-"}; !slices.Equal(ranges, want) {
				t.Errorf("Range headers = %q, want %q (each attempt resumes from the last offset)", ranges, want)
			}
		})
	}
}

// A 206 that delivers more bytes than the object's total leaves a tmp that
// is no prefix of the object: drop the pair so the retry restarts from 0.
func TestDownloadOnce_206PastTotalDropsPair(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 4-11/8")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "tailtail")
	}))
	defer server.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}
	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path+".tmp", []byte("head"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp.url", resumeSidecar{Key: "video-42", Total: 8}.encode(), 0644); err != nil {
		t.Fatal(err)
	}

	err := c.downloadOnce(DownloadRequest{URL: server.URL + "/file", Path: path, Key: "video-42"})
	if err == nil || errors.Is(err, errNonRetriable) {
		t.Fatalf("err = %v, want a retriable error for 12 bytes of an 8-byte object", err)
	}
	for _, leftover := range []string{path, path + ".tmp", path + ".tmp.url"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("expected %s to be absent, stat err = %v", leftover, err)
		}
	}
}

// A rename that keeps failing after a complete download used to delete the
// tmp and burn the retry schedule re-downloading the whole object into the
// same failure. The finished tmp must survive with its sidecar, the error
// must stop the schedule, and the next attempt must rename it without a GET.
// A non-empty directory at the target fails the rename on every OS. A
// chunked response advertises no size, so the sidecar must learn it from the
// bytes that landed.
func TestDownload_FailedRenameKeepsTmpForNextAttempt(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprintf("chunked=%v", chunked), func(t *testing.T) {
			testFailedRenameKeepsTmp(t, chunked)
		})
	}
}

func testFailedRenameKeepsTmp(t *testing.T, chunked bool) {
	saved, savedWindow := RetryDelays, lockRetryWindow
	RetryDelays, lockRetryWindow = []time.Duration{time.Millisecond, time.Millisecond}, 0
	defer func() { RetryDelays, lockRetryWindow = saved, savedWindow }()

	const object = "complete video bytes"
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if chunked {
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, object)
	}))
	defer server.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}
	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.MkdirAll(filepath.Join(path, "blocker"), 0755); err != nil {
		t.Fatal(err)
	}
	req := DownloadRequest{URL: server.URL + "/file", Path: path, Key: "video-42", Replace: true}

	err := c.Download(req)
	if !errors.Is(err, errNonRetriable) {
		t.Fatalf("err = %v, want a non-retriable rename failure", err)
	}
	if n := gets.Load(); n != 1 {
		t.Errorf("GETs = %d, want 1 (a blocked rename must not re-download)", n)
	}
	if data, _ := os.ReadFile(path + ".tmp"); string(data) != object {
		t.Errorf("tmp = %q, want the complete download kept", data)
	}
	data, _ := os.ReadFile(path + ".tmp.url")
	if sc := parseResumeSidecar(data); sc != (resumeSidecar{Key: "video-42", Total: int64(len(object))}) {
		t.Errorf("sidecar = %+v, want key video-42 and total %d", sc, len(object))
	}

	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := c.Download(req); err != nil {
		t.Fatalf("Download after the blocker is gone: %v", err)
	}
	if n := gets.Load(); n != 1 {
		t.Errorf("GETs = %d, want still 1 (the complete tmp is renamed, not re-downloaded)", n)
	}
	if data, _ := os.ReadFile(path); string(data) != object {
		t.Errorf("file = %q, want %q", data, object)
	}
	for _, leftover := range []string{path + ".tmp", path + ".tmp.url"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("expected %s to be absent, stat err = %v", leftover, err)
		}
	}
}

// On Windows a player or antivirus holding the target open blocks the rename
// only until it lets go; the rename must wait that out within one attempt.
// Go opens files without FILE_SHARE_DELETE, so os.Open stands in for it.
func TestDownload_RenameRetriesWhileTargetLockedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("an open handle blocks a rename only on Windows")
	}
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		fmt.Fprint(w, "new video")
	}))
	defer server.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}
	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path, []byte("old video"), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(200*time.Millisecond, func() { f.Close() })

	if err := c.Download(DownloadRequest{URL: server.URL + "/file", Path: path, Key: "v", Replace: true}); err != nil {
		t.Fatalf("Download while the target is open: %v", err)
	}
	if n := gets.Load(); n != 1 {
		t.Errorf("GETs = %d, want 1", n)
	}
	if data, _ := os.ReadFile(path); string(data) != "new video" {
		t.Errorf("file = %q, want the replacement", data)
	}
}

// Replace downloads over an existing non-empty file; the old bytes survive
// until the new ones are renamed into place, so a failed re-download never
// empties the slot.
func TestDownload_ReplaceOverwritesExistingAndKeepsOldOnFailure(t *testing.T) {
	saved := RetryDelays
	RetryDelays = []time.Duration{time.Millisecond}
	defer func() { RetryDelays = saved }()

	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
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
	path := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(path, []byte("old content"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := c.Download(DownloadRequest{URL: server.URL + "/file", Path: path, Replace: true}); err != nil {
		t.Fatalf("Download(replace): %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "new content" {
		t.Errorf("file = %q, want the replacement", data)
	}

	fail.Store(true)
	if err := c.Download(DownloadRequest{URL: server.URL + "/file", Path: path, Replace: true}); err == nil {
		t.Fatal("Download(replace) against a 404 should fail")
	}
	if data, _ := os.ReadFile(path); string(data) != "new content" {
		t.Errorf("file = %q after failed replace, want the previous bytes untouched", data)
	}
}

func TestDownloadOnce_SidecarURLMismatchRestartsFresh(t *testing.T) {
	content := "fresh full payload"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "" {
			t.Errorf("Range = %q sent despite sidecar URL mismatch — stale bytes would be resumed against a different signed URL", got)
		}
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
	if err := os.WriteFile(path+".tmp", []byte("stale-bytes-from-old-signed-url"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp.url", []byte("https://old.signed.example/file?sig=expired"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := c.downloadOnce(DownloadRequest{URL: server.URL + "/file", Path: path}); err != nil {
		t.Fatalf("downloadOnce: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != content {
		t.Errorf("file = %q, want %q (no stale prefix may survive a URL change)", data, content)
	}
}

func TestDownloadOnce_206WrongContentRangeDropsPair(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Misbehaving CDN: we ask bytes=4-, it answers a wider range. Appending
		// its body would duplicate bytes 0..3 in the middle of the file.
		w.Header().Set("Content-Range", "bytes 0-7/8")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "headtail")
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
	dlURL := server.URL + "/file"
	if err := os.WriteFile(path+".tmp", []byte("head"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp.url", []byte(dlURL), 0644); err != nil {
		t.Fatal(err)
	}

	if err := c.downloadOnce(DownloadRequest{URL: dlURL, Path: path}); err == nil {
		t.Fatal("downloadOnce should fail on a Content-Range that does not start at the requested offset")
	}
	for _, leftover := range []string{path + ".tmp", path + ".tmp.url"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("expected %s to be dropped (untrustworthy alignment), stat err = %v", leftover, err)
		}
	}
}

func TestDownloadOnce_416DropsPair(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
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
	dlURL := server.URL + "/file"
	if err := os.WriteFile(path+".tmp", []byte("partial-larger-than-resource"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp.url", []byte(dlURL), 0644); err != nil {
		t.Fatal(err)
	}

	if err := c.downloadOnce(DownloadRequest{URL: dlURL, Path: path}); err == nil {
		t.Fatal("downloadOnce should fail on 416")
	}
	for _, leftover := range []string{path + ".tmp", path + ".tmp.url"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("expected %s to be reset on 416, stat err = %v", leftover, err)
		}
	}
}

func TestDownloadOnce_200AfterRangeTruncatesAndRestarts(t *testing.T) {
	content := "entirely fresh body"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server ignores the Range header and replies 200 with the full
		// resource — the partial must be truncated, not appended to.
		w.WriteHeader(http.StatusOK)
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
	dlURL := server.URL + "/file"
	if err := os.WriteFile(path+".tmp", []byte("head"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp.url", []byte(dlURL), 0644); err != nil {
		t.Fatal(err)
	}

	if err := c.downloadOnce(DownloadRequest{URL: dlURL, Path: path}); err != nil {
		t.Fatalf("downloadOnce: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != content {
		t.Errorf("file = %q, want %q (200 after Range must restart from byte 0)", data, content)
	}
}

// --- Retry classification ---

// Deterministic 4xx download failures (expired signed URL → 400/403/410,
// deleted media → 404) must fail fast: the same URL yields the same verdict,
// so each retry only wastes the backoff schedule (~50s per file at CLI
// defaults).
func TestDownloadFile_4xxFailsFastWithoutRetries(t *testing.T) {
	saved := RetryDelays
	RetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { RetryDelays = saved }()

	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}

	err := c.DownloadFile(server.URL+"/file", filepath.Join(t.TempDir(), "v.mp4"))
	if err == nil {
		t.Fatal("DownloadFile should fail on 403")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("HTTP calls = %d, want exactly 1 (4xx is non-retriable)", got)
	}
}

func TestDownloadFile_5xxKeepsRetrying(t *testing.T) {
	saved := RetryDelays
	RetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { RetryDelays = saved }()

	content := "recovered payload"
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, content)
	}))
	defer server.Close()

	c := &Client{
		Tokens:       &Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          discardLogger{},
	}

	path := filepath.Join(t.TempDir(), "v.mp4")
	if err := c.DownloadFile(server.URL+"/file", path); err != nil {
		t.Fatalf("DownloadFile should recover from transient 5xx: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != content {
		t.Errorf("file = %q, want %q", data, content)
	}
}

// A dead refresh token (server answers 4xx) is permanent: the refresh body
// is deterministic, so retrying cannot succeed. GetJSON must fail fast with
// ErrRefreshRejected instead of burning the whole backoff schedule — observed
// live as ~50s of futile "retry N/3" before the actionable message surfaced.
func TestGetJSON_RefreshRejected4xxFailsFast(t *testing.T) {
	saved := RetryDelays
	RetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { RetryDelays = saved }()

	var refreshCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshCalls, 1)
		w.WriteHeader(http.StatusBadRequest)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := &Client{
		Tokens: &Tokens{
			AccessToken:  "stale",
			RefreshToken: "dead",
			ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(), // forces the proactive refresh
		},
		HTTP: rewriteClient(t, server),
		Log:  discardLogger{},
	}

	var out map[string]string
	err := c.GetJSON(BaseURL+"/v1/test", &out)
	if err == nil {
		t.Fatal("GetJSON should fail on a rejected refresh")
	}
	if !errors.Is(err, ErrRefreshRejected) {
		t.Errorf("err = %v, want ErrRefreshRejected in the chain", err)
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Errorf("refresh POSTs = %d, want exactly 1 (fail fast, no retries)", got)
	}
	if !strings.Contains(err.Error(), "Get new tokens from browser cookies and update auth.json") {
		t.Errorf("err = %q, must keep the actionable instruction", err)
	}
}

// A 5xx during refresh is a transient edge failure and must keep the retry
// schedule — only the deterministic 4xx verdict fails fast.
func TestGetJSON_Refresh5xxStaysRetriable(t *testing.T) {
	saved := RetryDelays
	RetryDelays = []time.Duration{time.Millisecond}
	defer func() { RetryDelays = saved }()

	var refreshCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshCalls, 1)
		w.WriteHeader(http.StatusBadGateway)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := &Client{
		Tokens: &Tokens{
			AccessToken:  "stale",
			RefreshToken: "r",
			ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		},
		HTTP: rewriteClient(t, server),
		Log:  discardLogger{},
	}

	var out map[string]string
	err := c.GetJSON(BaseURL+"/v1/test", &out)
	if err == nil {
		t.Fatal("GetJSON should fail when refresh keeps returning 5xx")
	}
	if errors.Is(err, ErrRefreshRejected) {
		t.Errorf("err = %v; a 5xx refresh failure must NOT be classified as rejected", err)
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 2 {
		t.Errorf("refresh POSTs = %d, want 2 (first attempt + 1 retry)", got)
	}
}

// authServer mocks Boosty's token rotation: it honours one access token and
// one refresh token at a time, and every accepted refresh issues pair N+1
// (aN/rN) and kills pair N, exactly as the real server consumes a refresh
// token. /v1/x answers 200 only for the honoured access token, 401 otherwise.
type authServer struct {
	mu      sync.Mutex
	access  string // honoured access token; "" until the first refresh
	refresh string // the one refresh token the server accepts
	gen     int

	// onRefresh, if set, runs under mu at the start of every refresh POST.
	onRefresh func(s *authServer)
	// onStale, if set, runs (without mu) before an API request with a
	// non-honoured access token gets its 401.
	onStale func()

	refreshPosts atomic.Int32
	apiOK        atomic.Int32
}

// newAuthServer starts a server that accepts refresh token r1 and honours
// no access token yet, so a client holding a0/r1 must refresh to a2/r2.
func newAuthServer(t *testing.T) (*authServer, *http.Client) {
	t.Helper()
	s := &authServer{refresh: "r1", gen: 1}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token/", s.handleRefresh)
	mux.HandleFunc("/v1/x", s.handleAPI)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, rewriteClient(t, srv)
}

// rotate issues the next pair. Caller holds mu.
func (s *authServer) rotate() {
	s.gen++
	s.access, s.refresh = fmt.Sprintf("a%d", s.gen), fmt.Sprintf("r%d", s.gen)
}

func (s *authServer) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.refreshPosts.Add(1)
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onRefresh != nil {
		s.onRefresh(s)
	}
	if body["refresh_token"] != s.refresh {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.rotate()
	json.NewEncoder(w).Encode(map[string]any{
		"access_token":  s.access,
		"refresh_token": s.refresh,
		"expires_in":    3600,
	})
}

func (s *authServer) handleAPI(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ok := s.access != "" && r.Header.Get("Authorization") == "Bearer "+s.access
	s.mu.Unlock()
	if !ok {
		if s.onStale != nil {
			s.onStale()
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	s.apiOK.Add(1)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// The refresh success path, for both triggers: a 401 on a token without
// expiresAt (the documented auth.json setup) and the proactive refresh of an
// expired one. The retry must carry the new token, and auth.json must end up
// holding the rotated pair — the old refresh token is dead server-side.
func TestGetJSON_RefreshRetriesAndPersists(t *testing.T) {
	for name, expiresAt := range map[string]int64{
		"401":     0,
		"expired": time.Now().Add(-time.Hour).UnixMilli(),
	} {
		t.Run(name, func(t *testing.T) {
			srv, hc := newAuthServer(t)
			authPath := filepath.Join(t.TempDir(), "auth.json")
			tok := &Tokens{AccessToken: "a0", RefreshToken: "r1", DeviceID: "d", ExpiresAt: expiresAt}
			if err := tok.SaveTokens(authPath); err != nil {
				t.Fatal(err)
			}
			c := &Client{Tokens: tok, AuthPath: authPath, HTTP: hc, Log: discardLogger{}}

			var out map[string]string
			if err := c.GetJSON(BaseURL+"/v1/x", &out); err != nil {
				t.Fatalf("GetJSON: %v", err)
			}
			if got := srv.refreshPosts.Load(); got != 1 {
				t.Errorf("refresh POSTs = %d, want 1", got)
			}
			if got := srv.apiOK.Load(); got != 1 {
				t.Errorf("API calls with the new token = %d, want 1", got)
			}
			disk, err := LoadTokens(authPath)
			if err != nil {
				t.Fatal(err)
			}
			if disk.AccessToken != "a2" || disk.RefreshToken != "r2" || disk.DeviceID != "d" || disk.ExpiresAt == 0 {
				t.Errorf("auth.json = %+v, want a2/r2/d with expiresAt set", disk)
			}
		})
	}
}

// N workers hit a 401 with the same stale token; exactly one of them may
// refresh. The barrier holds every 401 until all N stale requests have
// arrived, so all N take the refresh path concurrently (the -race CI job's
// coverage of tokensMu).
func TestGetJSON_ConcurrentRefreshDedup(t *testing.T) {
	const n = 6
	srv, hc := newAuthServer(t)
	var stale atomic.Int32
	release := make(chan struct{})
	srv.onStale = func() {
		if stale.Add(1) == n {
			close(release)
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			t.Error("the stale-token requests never all arrived together")
		}
	}
	authPath := filepath.Join(t.TempDir(), "auth.json")
	tok := &Tokens{AccessToken: "a0", RefreshToken: "r1"}
	if err := tok.SaveTokens(authPath); err != nil {
		t.Fatal(err)
	}
	c := &Client{Tokens: tok, AuthPath: authPath, HTTP: hc, Log: discardLogger{}}

	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			var out map[string]string
			errs[i] = c.GetJSON(BaseURL+"/v1/x", &out)
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}
	if got := srv.refreshPosts.Load(); got != 1 {
		t.Errorf("refresh POSTs = %d, want 1", got)
	}
	if got := srv.apiOK.Load(); got != n {
		t.Errorf("API calls with the new token = %d, want %d", got, n)
	}
	if got := stale.Load(); got != n {
		t.Errorf("stale-token requests = %d, want %d", got, n)
	}
}

// A save that fails after a successful refresh must not roll back: the
// server has already consumed the old refresh token. The rotated pair stays
// in memory and lands in auth.json.new, the error names that file, and the
// client never rotates again, so that file stays the valid credential.
func TestGetJSON_RefreshSaveFailureKeepsRotatedTokens(t *testing.T) {
	saved, savedWindow := RetryDelays, lockRetryWindow
	RetryDelays, lockRetryWindow = []time.Duration{time.Millisecond}, 0
	defer func() { RetryDelays, lockRetryWindow = saved, savedWindow }()

	srv, hc := newAuthServer(t)
	// A directory where auth.json should be: the rename onto it fails on
	// every platform, while the sibling fallback file can still be written.
	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.Mkdir(authPath, 0700); err != nil {
		t.Fatal(err)
	}
	tok := &Tokens{AccessToken: "a0", RefreshToken: "r1", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}
	c := &Client{Tokens: tok, AuthPath: authPath, HTTP: hc, Log: discardLogger{}}

	var out map[string]string
	err := c.GetJSON(BaseURL+"/v1/x", &out)
	if !errors.Is(err, ErrTokenSaveFailed) {
		t.Fatalf("err = %v, want ErrTokenSaveFailed in chain", err)
	}
	if !strings.Contains(err.Error(), authPath+".new") {
		t.Errorf("err = %q, must name the fallback file", err)
	}
	if got := srv.apiOK.Load(); got != 0 {
		t.Errorf("API calls after the failed save = %d, want 0", got)
	}
	if tok.AccessToken != "a2" || tok.RefreshToken != "r2" {
		t.Errorf("in-memory tokens = %+v, want the rotated a2/r2", tok)
	}
	if disk, err := LoadTokens(authPath + ".new"); err != nil || disk.RefreshToken != "r2" {
		t.Errorf("fallback file = %+v, %v; want refresh token r2", disk, err)
	}

	// Other workers keep using the live in-memory token...
	if err := c.GetJSON(BaseURL+"/v1/x", &out); err != nil {
		t.Errorf("GetJSON with the rotated token: %v", err)
	}
	// ...but the next refresh fails without a POST: rotating again would
	// leave auth.json.new holding a dead refresh token.
	tok.ExpiresAt = time.Now().Add(-time.Hour).UnixMilli()
	if err := c.GetJSON(BaseURL+"/v1/x", &out); !errors.Is(err, ErrTokenSaveFailed) {
		t.Errorf("err after expiry = %v, want ErrTokenSaveFailed", err)
	}
	if got := srv.refreshPosts.Load(); got != 1 {
		t.Errorf("refresh POSTs = %d, want 1", got)
	}
}

// When the fallback file cannot be written either, the error must say the
// new tokens die with the process instead of pointing at a file.
func TestGetJSON_RefreshSaveAndFallbackFailure(t *testing.T) {
	saved := RetryDelays
	RetryDelays = []time.Duration{time.Millisecond}
	defer func() { RetryDelays = saved }()

	_, hc := newAuthServer(t)
	tok := &Tokens{AccessToken: "a0", RefreshToken: "r1", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}
	c := &Client{
		Tokens:   tok,
		AuthPath: filepath.Join(t.TempDir(), "missing-parent", "auth.json"),
		HTTP:     hc,
		Log:      discardLogger{},
	}

	var out map[string]string
	err := c.GetJSON(BaseURL+"/v1/x", &out)
	if !errors.Is(err, ErrTokenSaveFailed) {
		t.Fatalf("err = %v, want ErrTokenSaveFailed in chain", err)
	}
	if !strings.Contains(err.Error(), "get new tokens from browser cookies") {
		t.Errorf("err = %q, must tell the user the new tokens are lost", err)
	}
	if tok.RefreshToken != "r2" {
		t.Errorf("in-memory refresh token = %q, want the rotated r2", tok.RefreshToken)
	}
}

// Two processes share auth.json. The one that refreshes first rotates r1
// away; the other must adopt the saved pair instead of POSTing its dead r1.
func TestGetJSON_AdoptsTokensRotatedByAnotherProcess(t *testing.T) {
	srv, hc := newAuthServer(t)
	authPath := filepath.Join(t.TempDir(), "auth.json")
	loaded := Tokens{AccessToken: "a0", RefreshToken: "r1", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}
	if err := loaded.SaveTokens(authPath); err != nil {
		t.Fatal(err)
	}
	newProcess := func() *Client {
		tok := loaded
		return &Client{Tokens: &tok, AuthPath: authPath, HTTP: hc, Log: discardLogger{}}
	}
	first, second := newProcess(), newProcess()

	var out map[string]string
	if err := first.GetJSON(BaseURL+"/v1/x", &out); err != nil {
		t.Fatalf("first process: %v", err)
	}
	if err := second.GetJSON(BaseURL+"/v1/x", &out); err != nil {
		t.Fatalf("second process: %v", err)
	}
	if got := srv.refreshPosts.Load(); got != 1 {
		t.Errorf("refresh POSTs = %d, want 1 (the second process adopts)", got)
	}
	if second.Tokens.RefreshToken != "r2" {
		t.Errorf("second process refresh token = %q, want r2", second.Tokens.RefreshToken)
	}
}

// The other process saves its rotated pair after our pre-refresh read but
// before our POST lands: the rejection must trigger one more read of
// auth.json, which recovers instead of failing the run.
func TestGetJSON_AdoptsTokensAfterRejection(t *testing.T) {
	srv, hc := newAuthServer(t)
	authPath := filepath.Join(t.TempDir(), "auth.json")
	tok := &Tokens{AccessToken: "a0", RefreshToken: "r1", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}
	if err := tok.SaveTokens(authPath); err != nil {
		t.Fatal(err)
	}
	var raced atomic.Bool
	srv.onRefresh = func(s *authServer) {
		if raced.Swap(true) {
			return
		}
		s.rotate()
		other := Tokens{AccessToken: s.access, RefreshToken: s.refresh, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
		if err := other.SaveTokens(authPath); err != nil {
			t.Errorf("simulate other process: %v", err)
		}
	}
	c := &Client{Tokens: tok, AuthPath: authPath, HTTP: hc, Log: discardLogger{}}

	var out map[string]string
	if err := c.GetJSON(BaseURL+"/v1/x", &out); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if tok.RefreshToken != "r2" {
		t.Errorf("refresh token = %q, want the adopted r2", tok.RefreshToken)
	}
	if got := srv.refreshPosts.Load(); got != 1 {
		t.Errorf("refresh POSTs = %d, want 1", got)
	}
}

// An auth.json older than the in-memory pair (restored from a backup, or a
// startup rewrite by another b00p that raced our save) holds a consumed
// refresh token; adopting it would kill a working client.
func TestGetJSON_IgnoresOlderTokensOnDisk(t *testing.T) {
	srv, hc := newAuthServer(t)
	srv.mu.Lock()
	srv.rotate() // the server now accepts r2 only
	srv.mu.Unlock()
	authPath := filepath.Join(t.TempDir(), "auth.json")
	backup := Tokens{AccessToken: "a1", RefreshToken: "r1", ExpiresAt: time.Now().Add(-2 * time.Hour).UnixMilli()}
	if err := backup.SaveTokens(authPath); err != nil {
		t.Fatal(err)
	}
	tok := &Tokens{AccessToken: "a2", RefreshToken: "r2", ExpiresAt: time.Now().Add(-time.Minute).UnixMilli()}
	c := &Client{Tokens: tok, AuthPath: authPath, HTTP: hc, Log: discardLogger{}}

	var out map[string]string
	if err := c.GetJSON(BaseURL+"/v1/x", &out); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if disk, err := LoadTokens(authPath); err != nil || disk.RefreshToken != "r3" {
		t.Errorf("auth.json = %+v, %v; want the rotated r3", disk, err)
	}
}

// Once the server rejects a refresh token, later requests must fail without
// POSTing it again; a different refresh token is tried normally.
func TestGetJSON_RejectedRefreshIsNotRetried(t *testing.T) {
	var refreshCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token/", func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := &Client{
		Tokens: &Tokens{
			AccessToken:  "stale",
			RefreshToken: "dead",
			ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		},
		HTTP: rewriteClient(t, server),
		Log:  discardLogger{},
	}

	var out map[string]string
	for i := range 5 {
		if err := c.GetJSON(BaseURL+"/v1/test", &out); !errors.Is(err, ErrRefreshRejected) {
			t.Fatalf("call %d: err = %v, want ErrRefreshRejected", i, err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Errorf("refresh POSTs = %d, want 1", got)
	}

	c.Tokens.RefreshToken = "other"
	c.GetJSON(BaseURL+"/v1/test", &out)
	if got := refreshCalls.Load(); got != 2 {
		t.Errorf("refresh POSTs after new tokens = %d, want 2", got)
	}
}

// Tokens.Refresh classifies the server verdict: 4xx (dead/revoked refresh
// token) wraps ErrRefreshRejected and tells the user to get new tokens; 429
// and 5xx stay plain (transient) and must not, since the tokens are fine.
func TestRefresh_StatusClassification(t *testing.T) {
	const getNewTokens = "Get new tokens from browser cookies and update auth.json"
	cases := []struct {
		status   int
		rejected bool
		wantMsg  string
	}{
		{http.StatusBadRequest, true, getNewTokens},
		{http.StatusUnauthorized, true, getNewTokens},
		{http.StatusForbidden, true, getNewTokens},
		{http.StatusTooManyRequests, false, "try again later"},
		{http.StatusBadGateway, false, "try again later"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			tok := &Tokens{AccessToken: "a", RefreshToken: "r"}
			err := tok.Refresh(rewriteClient(t, server))
			if err == nil {
				t.Fatalf("Refresh should fail on %d", tc.status)
			}
			if got := errors.Is(err, ErrRefreshRejected); got != tc.rejected {
				t.Errorf("errors.Is(err, ErrRefreshRejected) = %v, want %v (err=%v)", got, tc.rejected, err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %q, want it to contain %q", err, tc.wantMsg)
			}
			if !tc.rejected && strings.Contains(err.Error(), getNewTokens) {
				t.Errorf("err = %q: a transient failure must not send the user to re-extract tokens", err)
			}
		})
	}
}

// CommentsURL must include reply_limit so the server inlines replies. Without
// the param the server returns replies.data=[] regardless of replyCount, which
// silently dropped every reply body before this fix.
func TestCommentsURL_IncludesReplyLimit(t *testing.T) {
	got := CommentsURL("someblog", "post-id-123", 50)
	if !strings.Contains(got, "reply_limit=100") {
		t.Errorf("CommentsURL = %q, missing reply_limit=100", got)
	}
	if !strings.Contains(got, "limit=50") {
		t.Errorf("CommentsURL = %q, missing limit=50", got)
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
