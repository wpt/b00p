package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/state"
)

type recordingLogger struct{ lines []string }

func (r *recordingLogger) Printf(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recordingLogger) joined() string { return strings.Join(r.lines, "\n") }

func TestCheckMissingFiles_AllPresent(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "post.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "comments.json"), []byte("[]"), 0644)
	os.WriteFile(filepath.Join(dir, "post.md"), []byte("# title"), 0644)

	entry := state.PostEntry{HasComments: true, HasMd: true}
	got := checkMissingFiles(entry, dir)
	if got != "" {
		t.Errorf("checkMissingFiles = %q, want empty", got)
	}
}

func TestCheckMissingFiles_CommentsMissing(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "post.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "post.md"), []byte("# title"), 0644)

	entry := state.PostEntry{HasComments: true, HasMd: true}
	got := checkMissingFiles(entry, dir)
	if got != "comments.json" {
		t.Errorf("checkMissingFiles = %q, want 'comments.json'", got)
	}
}

func TestCheckMissingFiles_MdMissing(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "post.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "comments.json"), []byte("[]"), 0644)

	entry := state.PostEntry{HasComments: true, HasMd: true}
	got := checkMissingFiles(entry, dir)
	if got != "post.md" {
		t.Errorf("checkMissingFiles = %q, want 'post.md'", got)
	}
}

func TestCheckMissingFiles_PostJsonMissing(t *testing.T) {
	dir := t.TempDir()

	entry := state.PostEntry{}
	got := checkMissingFiles(entry, dir)
	if got != "post.json" {
		t.Errorf("checkMissingFiles = %q, want 'post.json'", got)
	}
}

func TestCheckMissingFiles_MultipleMissing(t *testing.T) {
	dir := t.TempDir()

	entry := state.PostEntry{HasComments: true, HasMd: true}
	got := checkMissingFiles(entry, dir)
	if got != "post.json, comments.json, post.md" {
		t.Errorf("checkMissingFiles = %q, want 'post.json, comments.json, post.md'", got)
	}
}

func TestCheckMissingFiles_NoCommentsNoMd(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "post.json"), []byte("{}"), 0644)

	entry := state.PostEntry{HasComments: false, HasMd: false}
	got := checkMissingFiles(entry, dir)
	if got != "" {
		t.Errorf("checkMissingFiles = %q, want empty (comments/md not expected)", got)
	}
}

func TestHasOkVideo(t *testing.T) {
	cases := []struct {
		name   string
		blocks []boosty.ContentBlock
		want   bool
	}{
		{"empty", nil, false},
		{"only text", []boosty.ContentBlock{{Type: "text"}}, false},
		{"external video only", []boosty.ContentBlock{{Type: "video", URL: "https://youtu.be/x"}}, false},
		{"ok_video present", []boosty.ContentBlock{{Type: "text"}, {Type: "ok_video"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasOkVideo(tc.blocks); got != tc.want {
				t.Errorf("hasOkVideo = %v, want %v", got, tc.want)
			}
		})
	}
}

// fakeVideoServer serves HEAD responses per the test table.
func fakeVideoServer(t *testing.T, wantUA string, status int, contentLength int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %q, want HEAD", r.Method)
		}
		if wantUA != "" && r.Header.Get("User-Agent") != wantUA {
			// Simulate okcdn behavior: wrong UA → 400 with tiny body.
			w.Header().Set("Content-Length", "2")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if contentLength >= 0 {
			w.Header().Set("Content-Length", fmt.Sprint(contentLength))
		}
		w.WriteHeader(status)
	}))
}

func TestCheckRemoteVideoSize_Match(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "video_001.mp4")
	os.WriteFile(localPath, []byte("payload"), 0644) // 7 bytes

	srv := fakeVideoServer(t, boosty.UserAgent, http.StatusOK, 7)
	defer srv.Close()

	log := &recordingLogger{}
	got := checkRemoteVideoSize(srv.Client(), boosty.UserAgent, log, srv.URL, localPath, "video_001.mp4")
	if got != "" {
		t.Errorf("got %q, want empty; log=%s", got, log.joined())
	}
}

func TestCheckRemoteVideoSize_SizeMismatch(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "video_001.mp4")
	os.WriteFile(localPath, []byte("short"), 0644) // 5 bytes

	srv := fakeVideoServer(t, boosty.UserAgent, http.StatusOK, 999)
	defer srv.Close()

	log := &recordingLogger{}
	got := checkRemoteVideoSize(srv.Client(), boosty.UserAgent, log, srv.URL, localPath, "video_001.mp4")
	if !strings.Contains(got, "video_001.mp4") || !strings.Contains(got, "local") || !strings.Contains(got, "remote") {
		t.Errorf("got %q, want a size mismatch description", got)
	}
}

func TestCheckRemoteVideoSize_LocalMissing(t *testing.T) {
	srv := fakeVideoServer(t, boosty.UserAgent, http.StatusOK, 7)
	defer srv.Close()

	log := &recordingLogger{}
	got := checkRemoteVideoSize(srv.Client(), boosty.UserAgent, log,
		srv.URL, filepath.Join(t.TempDir(), "gone.mp4"), "gone.mp4")
	if got != "gone.mp4 missing" {
		t.Errorf("got %q, want 'gone.mp4 missing'", got)
	}
}

// Replicates the real-world bug: wrong UA causes okcdn to 400 with 2-byte body.
// Old code read ContentLength=2 and reported every video as mismatched.
// New code: wrong UA is a 400 → reported as "HEAD 400", not a size comparison.
func TestCheckRemoteVideoSize_WrongUAYields400(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "v.mp4")
	os.WriteFile(localPath, make([]byte, 1024), 0644)

	srv := fakeVideoServer(t, boosty.UserAgent, http.StatusOK, 1024)
	defer srv.Close()

	log := &recordingLogger{}
	got := checkRemoteVideoSize(srv.Client(), "Wrong/UA", log, srv.URL, localPath, "v.mp4")
	if !strings.Contains(got, "HEAD 400") {
		t.Errorf("got %q, want 'HEAD 400'", got)
	}
}

// Passing the correct UA through the real path should produce no mismatch.
func TestCheckRemoteVideoSize_CorrectUA(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "v.mp4")
	os.WriteFile(localPath, make([]byte, 1024), 0644)

	srv := fakeVideoServer(t, boosty.UserAgent, http.StatusOK, 1024)
	defer srv.Close()

	log := &recordingLogger{}
	got := checkRemoteVideoSize(srv.Client(), boosty.UserAgent, log, srv.URL, localPath, "v.mp4")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestCheckRemoteVideoSize_NoContentLengthIsLoggedNotMismatched(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "v.mp4")
	os.WriteFile(localPath, []byte("x"), 0644)

	// Server returns 200 with no Content-Length.
	srv := fakeVideoServer(t, boosty.UserAgent, http.StatusOK, -1)
	defer srv.Close()

	log := &recordingLogger{}
	got := checkRemoteVideoSize(srv.Client(), boosty.UserAgent, log, srv.URL, localPath, "v.mp4")
	if got != "" {
		t.Errorf("got %q, want empty (transient → logged, not mismatch)", got)
	}
	if !strings.Contains(log.joined(), "no Content-Length") {
		t.Errorf("expected log about missing Content-Length, got %q", log.joined())
	}
}
