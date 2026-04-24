package cmd

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
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

// --- buildSyncEntry: state-after-failure invariants ---
//
// Regression for the codex re-review P1: applyItem used to call
// postStateEntryPreserving(&fullPost, ...), which bumped UpdatedAt and
// CommentsCount from the freshly-fetched post regardless of whether the
// underlying writes succeeded. A failed writeJSON or DownloadMedia would
// then advance state past disk reality and suppress the next sync's retry.
// buildSyncEntry must only advance retry-controlling fields when their
// artefact actually landed on disk.

func TestBuildSyncEntry_EditedAllOK_BumpsUpdatedAt(t *testing.T) {
	old := state.PostEntry{UpdatedAt: 100, Title: "old"}
	full := &boosty.Post{Title: "new", UpdatedAt: 200, Price: 50}

	// edited=true, all artefacts OK, no md/comments written.
	got := buildSyncEntry(old, full, "dir",
		true /*edited*/, true /*postJSONOK*/, true /*mediaOK*/, true /*mdOK*/, true, /*commentsOK*/
		false, false)

	if got.UpdatedAt != 200 {
		t.Errorf("UpdatedAt = %d, want 200", got.UpdatedAt)
	}
	if got.Title != "new" {
		t.Errorf("Title = %q, want 'new'", got.Title)
	}
	if got.Price != 50 {
		t.Errorf("Price = %d, want 50", got.Price)
	}
}

func TestBuildSyncEntry_EditedMediaFailed_PreservesUpdatedAt(t *testing.T) {
	old := state.PostEntry{UpdatedAt: 100}
	full := &boosty.Post{UpdatedAt: 200}

	// Edited + post.json OK + media FAILED → UpdatedAt must not advance.
	got := buildSyncEntry(old, full, "dir",
		true, true, false /*mediaOK*/, true /*mdOK*/, true, /*commentsOK*/
		false, false)

	if got.UpdatedAt != 100 {
		t.Errorf("UpdatedAt = %d, want 100 (preserved on media failure)", got.UpdatedAt)
	}
}

func TestBuildSyncEntry_EditedPostJSONFailed_PreservesUpdatedAt(t *testing.T) {
	old := state.PostEntry{UpdatedAt: 100}
	full := &boosty.Post{UpdatedAt: 200}

	got := buildSyncEntry(old, full, "dir",
		true, false /*postJSONOK*/, true, true /*mdOK*/, true, /*commentsOK*/
		false, false)

	if got.UpdatedAt != 100 {
		t.Errorf("UpdatedAt = %d, want 100 (preserved on post.json failure)", got.UpdatedAt)
	}
}

// Round 2 codex regression: an Edited post whose post.md regeneration
// fails must NOT bump UpdatedAt. Otherwise next sync sees the post as
// caught-up and never retries the markdown — leaving a stale or missing
// post.md without a normal recovery path (FILES_MISSING only fires when
// the file is fully absent, and only with --check-files).
func TestBuildSyncEntry_EditedMdFailed_PreservesUpdatedAt(t *testing.T) {
	old := state.PostEntry{UpdatedAt: 100, HasMd: true}
	full := &boosty.Post{UpdatedAt: 200}

	// Edited + post.json OK + media OK + md FAILED → UpdatedAt must not advance.
	got := buildSyncEntry(old, full, "dir",
		true /*edited*/, true, true, false /*mdOK*/, true, /*commentsOK*/
		false, false /*mdWritten*/)

	if got.UpdatedAt != 100 {
		t.Errorf("UpdatedAt = %d, want 100 (preserved on md failure)", got.UpdatedAt)
	}
	if !got.HasMd {
		t.Error("HasMd should remain true (preserved from old)")
	}
}

// Round 3 codex regression: edited post with existing comments where
// downloadComments fails. With only a count check on the next sync,
// a stale comments.json could otherwise live indefinitely if the count
// happened to match. UpdatedAt must NOT advance so the Edited trigger
// fires again next sync and retries comments.
func TestBuildSyncEntry_EditedCommentsFailed_PreservesUpdatedAt(t *testing.T) {
	old := state.PostEntry{UpdatedAt: 100, HasComments: true, CommentsCount: 5}
	full := &boosty.Post{UpdatedAt: 200}
	full.Count.Comments = 5 // unchanged count, but comments contents may have changed

	// Edited + everything else OK + comments FAILED → UpdatedAt must not advance.
	got := buildSyncEntry(old, full, "dir",
		true /*edited*/, true, true, true, false, /*commentsOK*/
		false /*commentsWritten*/, false)

	if got.UpdatedAt != 100 {
		t.Errorf("UpdatedAt = %d, want 100 (preserved on comments failure)", got.UpdatedAt)
	}
	if got.CommentsCount != 5 {
		t.Errorf("CommentsCount = %d, want 5 (preserved)", got.CommentsCount)
	}
	if !got.HasComments {
		t.Error("HasComments should remain true (preserved from old)")
	}
}

// Pure NewComments / VideoMismatch / Missing.* paths must not advance
// UpdatedAt — those triggers do not change the remote UpdatedAt and
// caching the new value here would suppress a later real edit.
func TestBuildSyncEntry_NotEdited_DoesNotBumpUpdatedAt(t *testing.T) {
	old := state.PostEntry{UpdatedAt: 100}
	full := &boosty.Post{UpdatedAt: 200}

	// edited=false → UpdatedAt must not advance even when everything succeeds.
	got := buildSyncEntry(old, full, "dir",
		false /*edited*/, true, true, true /*mdOK*/, true, /*commentsOK*/
		true, true)

	if got.UpdatedAt != 100 {
		t.Errorf("UpdatedAt = %d, want 100 (not edited)", got.UpdatedAt)
	}
}

func TestBuildSyncEntry_CommentsWritten_BumpsCount(t *testing.T) {
	old := state.PostEntry{CommentsCount: 5, HasComments: false}
	full := &boosty.Post{}
	full.Count.Comments = 10

	got := buildSyncEntry(old, full, "dir",
		false, true, true, true /*mdOK*/, true, /*commentsOK*/
		true /*commentsWritten*/, false)

	if got.CommentsCount != 10 {
		t.Errorf("CommentsCount = %d, want 10", got.CommentsCount)
	}
	if !got.HasComments {
		t.Error("HasComments = false, want true")
	}
}

func TestBuildSyncEntry_CommentsFailed_PreservesCount(t *testing.T) {
	old := state.PostEntry{CommentsCount: 5, HasComments: true}
	full := &boosty.Post{}
	full.Count.Comments = 10

	// Comments fetch failed → CommentsCount must stay at 5 so next sync
	// still sees a mismatch and retries.
	got := buildSyncEntry(old, full, "dir",
		false, true, true, true /*mdOK*/, false, /*commentsOK*/
		false /*commentsWritten*/, false)

	if got.CommentsCount != 5 {
		t.Errorf("CommentsCount = %d, want 5 (preserved on failure)", got.CommentsCount)
	}
	if !got.HasComments {
		t.Error("HasComments = false, want true (preserved)")
	}
}

func TestBuildSyncEntry_MdFailed_PreservesHasMd(t *testing.T) {
	// Existing entry had md; this run was supposed to regenerate it but
	// failed. HasMd must stay true so FILES_MISSING can detect the gap.
	old := state.PostEntry{HasMd: true}
	full := &boosty.Post{}

	got := buildSyncEntry(old, full, "dir",
		false, true, true, false /*mdOK*/, true /*commentsOK*/, false, false /*mdWritten*/)

	if !got.HasMd {
		t.Error("HasMd = false, want true (preserved when this run did not write md)")
	}
}

func TestBuildSyncEntry_TierClearedWhenSubLevelNil(t *testing.T) {
	old := state.PostEntry{Tier: "premium"}
	full := &boosty.Post{SubscriptionLevel: nil}

	got := buildSyncEntry(old, full, "dir",
		false, true, true, true, true /*commentsOK*/, false, false)

	if got.Tier != "" {
		t.Errorf("Tier = %q, want empty (post no longer gated)", got.Tier)
	}
}

// --- invalidateMediaForRedownload: skip-existing override ---
//
// Regression for the codex re-review P1: DownloadFile skips existing
// non-empty files, so an edited post that replaces media at the same
// filename (image_001.jpg, video_001.mp4) would keep the stale bytes
// while the apply path silently recorded success. invalidateMedia must
// remove the right files for the right trigger.

func TestInvalidateMedia_EditedRemovesAllNonExternal(t *testing.T) {
	dir := t.TempDir()
	must := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	must("image_001.jpg", "stale-image")
	must("video_001.mp4", "stale-video")
	must("external_video_001", "should-not-be-touched")

	media := []parser.MediaItem{
		{Type: "image", Filename: "image_001.jpg"},
		{Type: "video", Filename: "video_001.mp4"},
		{Type: "external_video", Filename: "external_video_001"},
	}
	log := &recordingLogger{}

	if !invalidateMediaForRedownload(media, dir, true /*edited*/, log) {
		t.Fatalf("returned false; log=%s", log.joined())
	}

	if _, err := os.Stat(filepath.Join(dir, "image_001.jpg")); !os.IsNotExist(err) {
		t.Errorf("image_001.jpg should have been removed; err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "video_001.mp4")); !os.IsNotExist(err) {
		t.Errorf("video_001.mp4 should have been removed; err=%v", err)
	}
	// external_video must be left alone — DownloadMedia ignores it, so
	// removing here would lose data the user manually fetched.
	if _, err := os.Stat(filepath.Join(dir, "external_video_001")); err != nil {
		t.Errorf("external_video_001 should NOT have been removed; err=%v", err)
	}
}

func TestInvalidateMedia_PureVideoMismatchOnlyRemovesVideos(t *testing.T) {
	dir := t.TempDir()
	must := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	must("image_001.jpg", "good-image")
	must("video_001.mp4", "wrong-size-video")

	media := []parser.MediaItem{
		{Type: "image", Filename: "image_001.jpg"},
		{Type: "video", Filename: "video_001.mp4"},
	}
	log := &recordingLogger{}

	if !invalidateMediaForRedownload(media, dir, false /*edited=false → pure VideoMismatch*/, log) {
		t.Fatalf("returned false; log=%s", log.joined())
	}

	// Image must NOT be removed — pure VideoMismatch only invalidates videos.
	if _, err := os.Stat(filepath.Join(dir, "image_001.jpg")); err != nil {
		t.Errorf("image_001.jpg should NOT have been removed under VideoMismatch only; err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "video_001.mp4")); !os.IsNotExist(err) {
		t.Errorf("video_001.mp4 should have been removed; err=%v", err)
	}
}

// ENOENT (file already gone or never existed) is normal and must not
// fail the invalidation — DownloadMedia will then create the file fresh.
func TestInvalidateMedia_MissingFilesAreOK(t *testing.T) {
	dir := t.TempDir()
	media := []parser.MediaItem{
		{Type: "image", Filename: "image_001.jpg"},
		{Type: "video", Filename: "video_001.mp4"},
	}
	log := &recordingLogger{}

	if !invalidateMediaForRedownload(media, dir, true, log) {
		t.Fatalf("returned false on missing files; log=%s", log.joined())
	}
	if log.joined() != "" {
		t.Errorf("expected no log lines for missing files, got %q", log.joined())
	}
}

// --- diskCommentCount and classifyPost: comment-count trigger uses disk reality ---
//
// Regression for the reply_limit bug: state.CommentsCount cached the API
// claim, while comments.json on disk had fewer (replies dropped). On the
// next sync, post.Count.Comments == state.CommentsCount → no mismatch →
// stale comments.json forever. classifyPost must read disk and trigger
// NewComments when it disagrees with the API.

func TestDiskCommentCount_FlatComments(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "comments.json"),
		[]byte(`[{"id":"1"},{"id":"2"},{"id":"3"}]`), 0644)

	n, ok := diskCommentCount(dir)
	if !ok {
		t.Fatal("diskCommentCount ok = false, want true")
	}
	if n != 3 {
		t.Errorf("diskCommentCount = %d, want 3", n)
	}
}

func TestDiskCommentCount_WithInlinedReplies(t *testing.T) {
	dir := t.TempDir()
	// 2 top-level comments; the first has 1 reply inlined, the second has 2.
	// Total = 2 + 1 + 2 = 5.
	os.WriteFile(filepath.Join(dir, "comments.json"), []byte(`[
		{"id":"1","replies":{"data":[{"id":"1a"}],"extra":{"isLast":true}}},
		{"id":"2","replies":{"data":[{"id":"2a"},{"id":"2b"}],"extra":{"isLast":true}}}
	]`), 0644)

	n, ok := diskCommentCount(dir)
	if !ok {
		t.Fatal("diskCommentCount ok = false, want true")
	}
	if n != 5 {
		t.Errorf("diskCommentCount = %d, want 5 (2 top-level + 3 replies)", n)
	}
}

func TestDiskCommentCount_EmptyArray(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "comments.json"), []byte(`[]`), 0644)

	n, ok := diskCommentCount(dir)
	if !ok {
		t.Fatal("diskCommentCount ok = false, want true on empty array")
	}
	if n != 0 {
		t.Errorf("diskCommentCount = %d, want 0", n)
	}
}

func TestDiskCommentCount_MissingFile(t *testing.T) {
	dir := t.TempDir()

	if _, ok := diskCommentCount(dir); ok {
		t.Error("diskCommentCount ok = true, want false on missing file")
	}
}

func TestDiskCommentCount_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "comments.json"), []byte(`{not json`), 0644)

	if _, ok := diskCommentCount(dir); ok {
		t.Error("diskCommentCount ok = true, want false on corrupt JSON")
	}
}

// The bug we're fixing: API count matches stale state, but disk has fewer.
// Without disk-read comparison this would never refetch.
func TestClassifyPost_DiskMissesReplies_TriggersNewComments(t *testing.T) {
	blogDir := t.TempDir()
	postDir := filepath.Join(blogDir, "post-dir")
	os.MkdirAll(postDir, 0755)
	// Disk has 3 entries (3 top-level, no inlined replies), but API claims 4.
	os.WriteFile(filepath.Join(postDir, "comments.json"),
		[]byte(`[{"id":"1"},{"id":"2"},{"id":"3"}]`), 0644)

	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{
		DirName:       "post-dir",
		HasComments:   true,
		CommentsCount: 4, // Stale: matches API but not disk
		UpdatedAt:     100,
	})

	post := boosty.Post{
		ID:        "post1",
		HasAccess: true,
		UpdatedAt: 100, // unchanged → not Edited
	}
	post.Count.Comments = 4

	got := classifyPost(post, st, blogDir)
	if !got.NewComments {
		t.Error("NewComments = false, want true (disk has 3, API claims 4)")
	}
	if got.DiskCommentCount != 3 {
		t.Errorf("DiskCommentCount = %d, want 3", got.DiskCommentCount)
	}
}

// Disk count matches API → no NewComments trigger, even when state.CommentsCount
// is stale (e.g. legacy entry with a wrong cached value).
func TestClassifyPost_DiskMatchesAPI_NoNewComments(t *testing.T) {
	blogDir := t.TempDir()
	postDir := filepath.Join(blogDir, "post-dir")
	os.MkdirAll(postDir, 0755)
	// Disk has 4: 3 top-level + 1 inlined reply.
	os.WriteFile(filepath.Join(postDir, "comments.json"), []byte(`[
		{"id":"1","replies":{"data":[{"id":"1a"}],"extra":{"isLast":true}}},
		{"id":"2"},
		{"id":"3"}
	]`), 0644)

	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{
		DirName:       "post-dir",
		HasComments:   true,
		CommentsCount: 99, // intentionally garbage — disk should win
		UpdatedAt:     100,
	})

	post := boosty.Post{ID: "post1", HasAccess: true, UpdatedAt: 100}
	post.Count.Comments = 4

	got := classifyPost(post, st, blogDir)
	if got.NewComments {
		t.Errorf("NewComments = true, want false (disk=4 matches API=4); DiskCommentCount=%d", got.DiskCommentCount)
	}
	if got.DiskCommentCount != 4 {
		t.Errorf("DiskCommentCount = %d, want 4", got.DiskCommentCount)
	}
}

// HasComments=true but comments.json missing → fallback to "trigger when API has any".
func TestClassifyPost_HasCommentsTrueButFileMissing_TriggersWhenAPIHasAny(t *testing.T) {
	blogDir := t.TempDir()
	postDir := filepath.Join(blogDir, "post-dir")
	os.MkdirAll(postDir, 0755)
	// No comments.json on disk — file disappeared/never landed.

	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{
		DirName:       "post-dir",
		HasComments:   true,
		CommentsCount: 5,
		UpdatedAt:     100,
	})

	post := boosty.Post{ID: "post1", HasAccess: true, UpdatedAt: 100}
	post.Count.Comments = 5 // matches state, but file is gone

	got := classifyPost(post, st, blogDir)
	if !got.NewComments {
		t.Error("NewComments = false, want true (file missing + API has comments)")
	}
	if got.DiskCommentCount != -1 {
		t.Errorf("DiskCommentCount = %d, want -1 (file missing)", got.DiskCommentCount)
	}
}

// HasComments=true, file missing, but API also reports zero → no refetch needed.
func TestClassifyPost_FileMissingAPIZero_NoNewComments(t *testing.T) {
	blogDir := t.TempDir()
	postDir := filepath.Join(blogDir, "post-dir")
	os.MkdirAll(postDir, 0755)

	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{
		DirName:       "post-dir",
		HasComments:   true,
		CommentsCount: 0,
		UpdatedAt:     100,
	})

	post := boosty.Post{ID: "post1", HasAccess: true, UpdatedAt: 100}
	post.Count.Comments = 0

	got := classifyPost(post, st, blogDir)
	if got.NewComments {
		t.Error("NewComments = true, want false (no comments anywhere)")
	}
}

// HasComments=false → preserves legacy state-vs-API count comparison.
// We have no comments.json to consult for posts the user opted out of.
func TestClassifyPost_HasCommentsFalse_FallsBackToStateCheck(t *testing.T) {
	blogDir := t.TempDir()
	postDir := filepath.Join(blogDir, "post-dir")
	os.MkdirAll(postDir, 0755)

	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{
		DirName:       "post-dir",
		HasComments:   false,
		CommentsCount: 3,
		UpdatedAt:     100,
	})

	post := boosty.Post{ID: "post1", HasAccess: true, UpdatedAt: 100}
	post.Count.Comments = 5 // grew from 3 → triggers under fallback path

	got := classifyPost(post, st, blogDir)
	if !got.NewComments {
		t.Error("NewComments = false, want true (HasComments=false, count grew 3→5)")
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

// --- confirmApply: --yes auto-confirm vs interactive prompt ---
//
// Regression for the "headless --sync" gap: a nohup'd `b00p download --sync`
// over ssh hangs forever on the "Apply changes? [y/N]" prompt because
// background jobs without a TTY can't read stdin. With --yes, the prompt
// is skipped entirely.

func TestConfirmApply_AutoYesSkipsPrompt(t *testing.T) {
	log := &recordingLogger{}
	// Empty stdin — would block forever on ReadString in interactive mode.
	in := bufio.NewReader(strings.NewReader(""))

	if !confirmApply(log, in, true) {
		t.Error("confirmApply(auto=true) = false, want true")
	}
	if !strings.Contains(log.joined(), "Auto-applying") {
		t.Errorf("expected 'Auto-applying' log line, got %q", log.joined())
	}
}

func TestConfirmApply_TypedY(t *testing.T) {
	log := &recordingLogger{}
	in := bufio.NewReader(strings.NewReader("y\n"))

	if !confirmApply(log, in, false) {
		t.Error("confirmApply(input='y') = false, want true")
	}
}

func TestConfirmApply_TypedYes(t *testing.T) {
	log := &recordingLogger{}
	in := bufio.NewReader(strings.NewReader("YES\n"))

	if !confirmApply(log, in, false) {
		t.Error("confirmApply(input='YES') = false, want true (case-insensitive)")
	}
}

func TestConfirmApply_TypedN(t *testing.T) {
	log := &recordingLogger{}
	in := bufio.NewReader(strings.NewReader("n\n"))

	if confirmApply(log, in, false) {
		t.Error("confirmApply(input='n') = true, want false")
	}
}

func TestConfirmApply_EmptyInputDefaultsToNo(t *testing.T) {
	log := &recordingLogger{}
	in := bufio.NewReader(strings.NewReader("\n"))

	if confirmApply(log, in, false) {
		t.Error("confirmApply(input='') = true, want false (default no)")
	}
}

func TestConfirmApply_EOFReturnsFalse(t *testing.T) {
	log := &recordingLogger{}
	// Empty reader → EOF immediately. Caller treats as cancellation.
	in := bufio.NewReader(strings.NewReader(""))

	if confirmApply(log, in, false) {
		t.Error("confirmApply(EOF) = true, want false")
	}
	if !strings.Contains(log.joined(), "failed to read confirmation") {
		t.Errorf("expected EOF warning in log, got %q", log.joined())
	}
}
