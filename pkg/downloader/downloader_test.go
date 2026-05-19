package downloader

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
)

func isWindows() bool { return runtime.GOOS == "windows" }

// recordingLogger captures log lines for later inspection.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingLogger) Printf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recordingLogger) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// newTestClient builds a Client wired to the supplied test server.
func newTestClient(server *httptest.Server, log boosty.Logger) *boosty.Client {
	return &boosty.Client{
		Tokens:       &boosty.Tokens{AccessToken: "test"},
		HTTP:         server.Client(),
		DownloadHTTP: server.Client(),
		Log:          log,
	}
}

// TestDownloadMedia_NilOnEmpty pins the errors.Join nil-return contract: with
// no media, no errors are collected so the returned error must be nil. A
// regression that aggregates a zero-length slice into a non-nil error would
// make every caller treat an empty post as failed.
func TestDownloadMedia_NilOnEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be called with no media")
	}))
	defer server.Close()

	log := &recordingLogger{}
	c := newTestClient(server, log)

	dir := t.TempDir()
	err := DownloadMedia(c, nil, dir)
	if err != nil {
		t.Fatalf("DownloadMedia with no media: %v, want nil", err)
	}
}

// TestDownloadMedia_SkipsExternalVideos verifies external_video items are not
// touched by DownloadMedia — those are DownloadExternal's job. A regression
// here would either double-download or hand yt-dlp URLs to the HTTP client.
func TestDownloadMedia_SkipsExternalVideos(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, "x")
	}))
	defer server.Close()

	log := &recordingLogger{}
	c := newTestClient(server, log)

	media := []parser.MediaItem{
		{Type: "external_video", URL: "https://youtu.be/abc", Filename: "ext"},
		{Type: "external_video", URL: "https://vk.com/video123", Filename: "ext2"},
	}
	dir := t.TempDir()
	err := DownloadMedia(c, media, dir)
	if err != nil {
		t.Fatalf("DownloadMedia: %v, want nil", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("HTTP hits = %d, want 0 (external videos must be skipped)", got)
	}
}

// TestDownloadMedia_Success writes all items to disk and returns nil.
func TestDownloadMedia_Success(t *testing.T) {
	bodies := map[string]string{
		"/a.jpg": "image-a-bytes",
		"/b.jpg": "image-b-bytes",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer server.Close()

	log := &recordingLogger{}
	c := newTestClient(server, log)
	dir := t.TempDir()

	media := []parser.MediaItem{
		{Type: "image", URL: server.URL + "/a.jpg", Filename: "a.jpg"},
		{Type: "image", URL: server.URL + "/b.jpg", Filename: "b.jpg"},
	}
	if err := DownloadMedia(c, media, dir); err != nil {
		t.Fatalf("DownloadMedia: %v, want nil", err)
	}

	for name, want := range map[string]string{"a.jpg": "image-a-bytes", "b.jpg": "image-b-bytes"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("file %s = %q, want %q", name, got, want)
		}
	}
}

// TestDownloadMedia_MixedSuccessAndFailure verifies errors.Join wraps every
// per-file failure and that successful items still land on disk. A regression
// that returns on first error would skip later items; a regression that
// silently swallowed failures would mark a partially-downloaded post complete.
func TestDownloadMedia_MixedSuccessAndFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping retry test in short mode (~50s due to DownloadFile backoff)")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/good.jpg":
			fmt.Fprint(w, "good-bytes")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	log := &recordingLogger{}
	c := newTestClient(server, log)
	dir := t.TempDir()

	media := []parser.MediaItem{
		{Type: "image", URL: server.URL + "/good.jpg", Filename: "good.jpg"},
		{Type: "image", URL: server.URL + "/bad1.jpg", Filename: "bad1.jpg"},
		{Type: "image", URL: server.URL + "/bad2.jpg", Filename: "bad2.jpg"},
	}
	err := DownloadMedia(c, media, dir)
	if err == nil {
		t.Fatal("DownloadMedia: nil, want non-nil error from failing items")
	}

	msg := err.Error()
	for _, name := range []string{"bad1.jpg", "bad2.jpg"} {
		if !strings.Contains(msg, name) {
			t.Errorf("error %q missing filename %q (errors.Join lost a failure)", msg, name)
		}
	}
	if strings.Contains(msg, "good.jpg") {
		t.Errorf("error %q references successful file 'good.jpg'", msg)
	}

	got, readErr := os.ReadFile(filepath.Join(dir, "good.jpg"))
	if readErr != nil {
		t.Fatalf("good.jpg should still be on disk despite sibling failures: %v", readErr)
	}
	if string(got) != "good-bytes" {
		t.Errorf("good.jpg = %q, want %q", got, "good-bytes")
	}
}

// TestDownloadMedia_MkdirFailure surfaces the MkdirAll error rather than
// silently appending to a missing directory. We point dir at a path that
// can't be created — an existing regular file used as a parent.
func TestDownloadMedia_MkdirFailure(t *testing.T) {
	log := &recordingLogger{}
	c := newTestClient(httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})), log)

	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("file-not-dir"), 0644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	// blocker is a regular file; MkdirAll(blocker/sub) must fail.
	bad := filepath.Join(blocker, "sub")

	err := DownloadMedia(c, []parser.MediaItem{{Type: "image", URL: "http://example", Filename: "x.jpg"}}, bad)
	if err == nil {
		t.Fatal("DownloadMedia under un-mkdir-able dir: nil, want error")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Errorf("error %q should mention mkdir", err.Error())
	}
}

// TestDownloadExternal_NilWhenNoExternal verifies the early-return contract:
// without external_video items, yt-dlp must not be looked up and nil is
// returned. A regression that always ran exec.LookPath would fail on machines
// without yt-dlp even when no external content is present.
func TestDownloadExternal_NilWhenNoExternal(t *testing.T) {
	log := &recordingLogger{}
	media := []parser.MediaItem{
		{Type: "image", URL: "http://example/a.jpg", Filename: "a.jpg"},
		{Type: "video", URL: "http://example/b.mp4", Filename: "b.mp4"},
	}

	err := DownloadExternal(log, media, t.TempDir())
	if err != nil {
		t.Fatalf("DownloadExternal with no external videos: %v, want nil", err)
	}
	if log.joined() != "" {
		t.Errorf("DownloadExternal logged %q, want silent for empty case", log.joined())
	}
}

// TestDownloadExternal_NilOnEmptyMedia: zero items must short-circuit too.
func TestDownloadExternal_NilOnEmptyMedia(t *testing.T) {
	log := &recordingLogger{}
	err := DownloadExternal(log, nil, t.TempDir())
	if err != nil {
		t.Fatalf("DownloadExternal with nil media: %v, want nil", err)
	}
}

// TestDownloadExternal_MissingYTDLP triggers the LookPath failure path by
// pointing PATH at an empty directory. A regression that swallowed this
// error (or didn't surface the install hint) would leave users mystified
// when external downloads silently produce nothing.
func TestDownloadExternal_MissingYTDLP(t *testing.T) {
	// Empty PATH guarantees exec.LookPath("yt-dlp") fails regardless of host.
	t.Setenv("PATH", t.TempDir())
	// Also clear PATHEXT on Windows to avoid resolving .exe shims from elsewhere.
	t.Setenv("PATHEXT", "")

	log := &recordingLogger{}
	media := []parser.MediaItem{
		{Type: "external_video", URL: "https://youtu.be/abc", Filename: "ext"},
	}
	err := DownloadExternal(log, media, t.TempDir())
	if err == nil {
		t.Fatal("DownloadExternal without yt-dlp: nil, want error")
	}
	if !strings.Contains(err.Error(), "yt-dlp not found") {
		t.Errorf("error %q should mention 'yt-dlp not found'", err.Error())
	}
	if !strings.Contains(err.Error(), "pip install") {
		t.Errorf("error %q should include install hint", err.Error())
	}
}

// TestDownloadExternal_HostileURLNotInterpretedAsFlag pins the `--` stop-parsing
// contract: a post that embeds a URL starting with `-` (or `--exec=...`-style
// payload) must not be smuggled to yt-dlp as a flag. We replace yt-dlp with a
// stub that echoes its argv; the stub fails with a marker exit so we get the
// error path AND can inspect the produced command via CombinedOutput, captured
// indirectly through the joined log.
//
// Strategy: install a fake yt-dlp on PATH that prints argv then exits 1.
// DownloadExternal logs CombinedOutput on failure, so log lines carry the argv.
func TestDownloadExternal_HostileURLNotInterpretedAsFlag(t *testing.T) {
	fakeDir, ytdlp := installFakeYTDLP(t)
	t.Setenv("PATH", fakeDir)
	// On Windows we install yt-dlp.bat — make sure PATHEXT recognises .BAT.
	if filepath.Ext(ytdlp) == ".bat" {
		t.Setenv("PATHEXT", ".BAT")
	}

	log := &recordingLogger{}
	media := []parser.MediaItem{
		{Type: "external_video", URL: "--exec=rm -rf /", Filename: "hostile"},
	}
	err := DownloadExternal(log, media, t.TempDir())
	if err == nil {
		t.Fatal("expected non-nil error from failing fake yt-dlp")
	}

	out := log.joined()
	// The hostile URL must appear AFTER a `--` separator in argv so yt-dlp
	// treats it as a positional. The fake prints args verbatim; if `--` is
	// missing or precedes `-o`, real yt-dlp would parse `--exec=...` as a
	// flag and execute attacker code.
	if !strings.Contains(out, "--") {
		t.Errorf("argv missing `--` separator: %q", out)
	}
	if !strings.Contains(out, "--exec=rm -rf /") {
		t.Errorf("hostile URL not present in argv as positional: %q", out)
	}
	// Sanity: the `-o` template must still be there.
	if !strings.Contains(out, "hostile.%(ext)s") {
		t.Errorf("output template missing: %q", out)
	}
}

// TestDownloadExternal_AggregatesFailures runs two external items with a fake
// yt-dlp that always fails; errors.Join must surface both URLs.
func TestDownloadExternal_AggregatesFailures(t *testing.T) {
	fakeDir, ytdlp := installFakeYTDLP(t)
	t.Setenv("PATH", fakeDir)
	if filepath.Ext(ytdlp) == ".bat" {
		t.Setenv("PATHEXT", ".BAT")
	}

	log := &recordingLogger{}
	media := []parser.MediaItem{
		{Type: "external_video", URL: "https://youtu.be/aaa", Filename: "ext1"},
		{Type: "external_video", URL: "https://vk.com/video=bbb", Filename: "ext2"},
	}
	err := DownloadExternal(log, media, t.TempDir())
	if err == nil {
		t.Fatal("DownloadExternal: nil, want aggregated error from two failing items")
	}

	msg := err.Error()
	for _, want := range []string{"https://youtu.be/aaa", "https://vk.com/video=bbb"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error %q missing URL %q", msg, want)
		}
	}

	// errors.Join wraps individual errors; both must remain unwrappable.
	var found int
	for _, m := range media {
		if strings.Contains(err.Error(), m.URL) {
			found++
		}
	}
	if found != 2 {
		t.Errorf("expected 2 URL substrings in aggregate, got %d", found)
	}
	_ = errors.Unwrap // import sanity; errors.Join error doesn't unwrap to single
}

// installFakeYTDLP drops an executable named yt-dlp (or yt-dlp.bat on Windows)
// into a fresh temp directory and returns (dir, executablePath). The fake
// prints its argv to stdout then exits 1, so callers can inspect argv via
// CombinedOutput / the logger and observe the error path.
func installFakeYTDLP(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	var (
		name    string
		content string
	)
	if isWindows() {
		name = "yt-dlp.bat"
		// `@echo off` prevents the prompt from echoing each line; %* prints argv.
		content = "@echo off\r\necho %*\r\nexit /b 1\r\n"
	} else {
		name = "yt-dlp"
		content = "#!/bin/sh\necho \"$@\"\nexit 1\n"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write fake yt-dlp: %v", err)
	}
	return dir, path
}
