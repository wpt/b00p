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
	"strings"
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
	err := c.downloadOnce(server.URL+"/file", path)
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
	// The sidecar must exist alongside the partial and pin the URL — without
	// it the next attempt treats the tmp as orphaned and restarts from byte 0,
	// silently disabling resume.
	sidecar, scErr := os.ReadFile(path + ".tmp.url")
	if scErr != nil {
		t.Fatalf("expected resume sidecar %s.tmp.url next to the partial: %v", path, scErr)
	}
	if string(sidecar) != server.URL+"/file" {
		t.Errorf("sidecar = %q, want the download URL %q", sidecar, server.URL+"/file")
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

	if err := c.downloadOnce(dlURL, path); err != nil {
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

	if err := c.downloadOnce(server.URL+"/file", path); err != nil {
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

	if err := c.downloadOnce(dlURL, path); err == nil {
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

	if err := c.downloadOnce(dlURL, path); err == nil {
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

	if err := c.downloadOnce(dlURL, path); err != nil {
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

func TestGetJSON_RefreshSaveFailureFailsClosed(t *testing.T) {
	saved := RetryDelays
	RetryDelays = []time.Duration{time.Millisecond}
	defer func() { RetryDelays = saved }()

	var apiCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh",
			"refresh_token": "fresh-refresh",
			"expires_in":    3600,
		})
	})
	mux.HandleFunc("/v1/test", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&apiCalls, 1)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	tok := &Tokens{
		AccessToken:  "stale",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	}
	c := &Client{
		Tokens:   tok,
		AuthPath: filepath.Join(t.TempDir(), "missing-parent", "auth.json"),
		HTTP:     rewriteClient(t, server),
		Log:      discardLogger{},
	}

	var out map[string]string
	err := c.GetJSON(BaseURL+"/v1/test", &out)
	if err == nil {
		t.Fatal("GetJSON should fail when refreshed tokens cannot be saved")
	}
	if !errors.Is(err, ErrTokenSaveFailed) {
		t.Fatalf("err = %v, want ErrTokenSaveFailed in chain", err)
	}
	if got := atomic.LoadInt32(&apiCalls); got != 0 {
		t.Errorf("API calls with unpersisted token = %d, want 0", got)
	}
	if tok.AccessToken != "stale" || tok.RefreshToken != "old-refresh" {
		t.Errorf("tokens mutated after save failure: %+v", tok)
	}
}

// Tokens.Refresh classifies the server verdict: 4xx (dead/revoked refresh
// token) wraps ErrRefreshRejected; 429 and 5xx stay plain (transient). The
// user-visible message is identical either way.
func TestRefresh_StatusClassification(t *testing.T) {
	cases := []struct {
		status   int
		rejected bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusTooManyRequests, false},
		{http.StatusBadGateway, false},
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
			if !strings.Contains(err.Error(), "Get new tokens from browser cookies and update auth.json") {
				t.Errorf("err = %q, must keep the actionable instruction", err)
			}
		})
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
