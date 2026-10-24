package downloader

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	err := DownloadMedia(c, nil, dir, KeepExisting)
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
		{Type: "external_video", URL: "https://youtu.be/abc"},
		{Type: "external_video", URL: "https://vk.com/video123"},
	}
	dir := t.TempDir()
	err := DownloadMedia(c, media, dir, KeepExisting)
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
	if err := DownloadMedia(c, media, dir, KeepExisting); err != nil {
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
	// A 404 is non-retriable (fails fast on the first attempt), so this
	// shrink is defensive — it keeps the test fast even if the failure mode
	// here ever becomes retriable. The retry classification is exercised in
	// pkg/boosty tests; here we only care about the per-item error wrapping.
	saved := boosty.RetryDelays
	boosty.RetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { boosty.RetryDelays = saved }()

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
	err := DownloadMedia(c, media, dir, KeepExisting)
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

// ReplaceVideos re-downloads native videos over their existing copies and
// leaves other media alone; ReplaceAll refreshes everything. KeepExisting
// (the first-download mode) touches nothing that is already on disk.
func TestDownloadMedia_ReplaceModes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "new-"+strings.TrimPrefix(r.URL.Path, "/"))
	}))
	defer server.Close()
	c := newTestClient(server, &recordingLogger{})

	media := []parser.MediaItem{
		{Type: "image", URL: server.URL + "/img", Filename: "image_001.jpg", ID: "i1"},
		{Type: "video", URL: server.URL + "/vid", Filename: "video_001.mp4", ID: "v1"},
	}
	seed := func(t *testing.T) string {
		dir := t.TempDir()
		for _, m := range media {
			if err := os.WriteFile(filepath.Join(dir, m.Filename), []byte("old"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	read := func(t *testing.T, dir, name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	for _, tc := range []struct {
		mode       Mode
		wantImage  string
		wantVideo  string
		modeString string
	}{
		{KeepExisting, "old", "old", "KeepExisting"},
		{ReplaceVideos, "old", "new-vid", "ReplaceVideos"},
		{ReplaceAll, "new-img", "new-vid", "ReplaceAll"},
	} {
		t.Run(tc.modeString, func(t *testing.T) {
			dir := seed(t)
			if err := DownloadMedia(c, media, dir, tc.mode); err != nil {
				t.Fatalf("DownloadMedia: %v", err)
			}
			if got := read(t, dir, "image_001.jpg"); got != tc.wantImage {
				t.Errorf("image = %q, want %q", got, tc.wantImage)
			}
			if got := read(t, dir, "video_001.mp4"); got != tc.wantVideo {
				t.Errorf("video = %q, want %q", got, tc.wantVideo)
			}
		})
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

	err := DownloadMedia(c, []parser.MediaItem{{Type: "image", URL: "http://example", Filename: "x.jpg"}}, bad, KeepExisting)
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
		{Type: "external_video", URL: "https://youtu.be/abc"},
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

func TestDownloadExternal_CreatesDir(t *testing.T) {
	installFakeYTDLP(t)

	log := &recordingLogger{}
	dir := filepath.Join(t.TempDir(), "missing", "nested")
	err := DownloadExternal(log, []parser.MediaItem{
		{Type: "external_video", URL: "https://youtu.be/abc"},
	}, dir)
	if err == nil {
		t.Fatal("fake yt-dlp should still fail after directory creation")
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("DownloadExternal did not create dir: %v", statErr)
	}
}

// TestDownloadExternal_SeparatorBeforeURL pins the `--` that ends yt-dlp's
// option parsing: the post-supplied URL must be the last argument, directly
// after `--`, or a URL such as `--exec=...` would be parsed as a yt-dlp flag.
func TestDownloadExternal_SeparatorBeforeURL(t *testing.T) {
	calls := installFakeYTDLP(t)

	const videoURL = "https://youtu.be/abc"
	err := DownloadExternal(&recordingLogger{}, []parser.MediaItem{
		{Type: "external_video", URL: videoURL},
	}, t.TempDir())
	if err == nil {
		t.Fatal("expected non-nil error from failing fake yt-dlp")
	}

	want := [][]string{{"--no-playlist", "-o", "external_%(extractor_key)s_%(id)s.%(ext)s", "--", videoURL}}
	if got := calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("yt-dlp calls = %q, want %q", got, want)
	}
}

// TestDownloadExternal_SkipsNonHTTPURL: an item whose URL is not http(s) is
// reported in the error and never reaches yt-dlp, and later items still run.
func TestDownloadExternal_SkipsNonHTTPURL(t *testing.T) {
	calls := installFakeYTDLP(t)

	bad := []string{"--exec=rm -rf /", "file:///etc/passwd", "ftp://example.com/v.mp4"}
	var media []parser.MediaItem
	for _, u := range bad {
		media = append(media, parser.MediaItem{Type: "external_video", URL: u})
	}
	const goodURL = "https://youtu.be/ok"
	media = append(media, parser.MediaItem{Type: "external_video", URL: goodURL})

	err := DownloadExternal(&recordingLogger{}, media, t.TempDir())
	if err == nil {
		t.Fatal("DownloadExternal: nil, want error listing the skipped URLs")
	}
	for _, u := range bad {
		if !strings.Contains(err.Error(), fmt.Sprintf("%q: not an http(s) URL", u)) {
			t.Errorf("error %q does not report skipped URL %q", err, u)
		}
	}
	want := [][]string{{"--no-playlist", "-o", "external_%(extractor_key)s_%(id)s.%(ext)s", "--", goodURL}}
	if got := calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("yt-dlp calls = %q, want only the https item %q", got, want)
	}
}

// TestDownloadExternal_AggregatesFailures runs two external items with a fake
// yt-dlp that always fails; errors.Join must surface both URLs.
func TestDownloadExternal_AggregatesFailures(t *testing.T) {
	installFakeYTDLP(t)

	log := &recordingLogger{}
	media := []parser.MediaItem{
		{Type: "external_video", URL: "https://youtu.be/aaa"},
		{Type: "external_video", URL: "https://vk.com/video=bbb"},
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

// fakeYTDLPSrc appends its argv as one JSON array per line to
// $FAKE_YTDLP_ARGV, then exits 1.
const fakeYTDLPSrc = `package main

import (
	"encoding/json"
	"os"
)

func main() {
	b, _ := json.Marshal(os.Args[1:])
	if f, err := os.OpenFile(os.Getenv("FAKE_YTDLP_ARGV"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Write(append(b, '\n'))
		f.Close()
	}
	os.Exit(1)
}
`

// installFakeYTDLP builds fakeYTDLPSrc as yt-dlp (yt-dlp.exe on Windows),
// makes its directory the whole PATH, and returns a func that reads back the
// argv of every invocation so far. A compiled fake records argv exactly as
// exec passed it, which echo in a shell or batch script cannot.
func installFakeYTDLP(t *testing.T) func() [][]string {
	t.Helper()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(fakeYTDLPSrc), 0o644); err != nil {
		t.Fatalf("write fake yt-dlp source: %v", err)
	}
	binDir := t.TempDir()
	name := "yt-dlp"
	if isWindows() {
		name += ".exe"
	}
	build := exec.Command("go", "build", "-o", filepath.Join(binDir, name), "main.go")
	build.Dir = srcDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake yt-dlp: %v\n%s", err, out)
	}
	t.Setenv("PATH", binDir)
	argvLog := filepath.Join(t.TempDir(), "argv.jsonl")
	t.Setenv("FAKE_YTDLP_ARGV", argvLog)

	return func() [][]string {
		t.Helper()
		raw, err := os.ReadFile(argvLog)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatalf("read fake yt-dlp argv log: %v", err)
		}
		var calls [][]string
		for line := range strings.Lines(string(raw)) {
			var argv []string
			if err := json.Unmarshal([]byte(line), &argv); err != nil {
				t.Fatalf("decode argv line %q: %v", line, err)
			}
			calls = append(calls, argv)
		}
		return calls
	}
}
