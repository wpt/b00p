package syncer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/state"
)

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

	got := buildSyncEntry(old, full, "dir", true, applyOutcome{
		PostJSONOK: true, MediaOK: true, MDOK: true, CommentsOK: true,
	})

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
	got := buildSyncEntry(old, full, "dir", true, applyOutcome{
		PostJSONOK: true, MediaOK: false, MDOK: true, CommentsOK: true,
	})

	if got.UpdatedAt != 100 {
		t.Errorf("UpdatedAt = %d, want 100 (preserved on media failure)", got.UpdatedAt)
	}
}

func TestBuildSyncEntry_EditedPostJSONFailed_PreservesUpdatedAt(t *testing.T) {
	old := state.PostEntry{UpdatedAt: 100}
	full := &boosty.Post{UpdatedAt: 200}

	got := buildSyncEntry(old, full, "dir", true, applyOutcome{
		PostJSONOK: false, MediaOK: true, MDOK: true, CommentsOK: true,
	})

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

	got := buildSyncEntry(old, full, "dir", true, applyOutcome{
		PostJSONOK: true, MediaOK: true, MDOK: false, CommentsOK: true,
	})

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

	got := buildSyncEntry(old, full, "dir", true, applyOutcome{
		PostJSONOK: true, MediaOK: true, MDOK: true, CommentsOK: false,
	})

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

	got := buildSyncEntry(old, full, "dir", false, applyOutcome{
		PostJSONOK: true, MediaOK: true, MDOK: true, CommentsOK: true,
		CommentsWritten: true, MDWritten: true,
	})

	if got.UpdatedAt != 100 {
		t.Errorf("UpdatedAt = %d, want 100 (not edited)", got.UpdatedAt)
	}
}

func TestBuildSyncEntry_CommentsWritten_BumpsCount(t *testing.T) {
	old := state.PostEntry{CommentsCount: 5, HasComments: false}
	full := &boosty.Post{}
	full.Count.Comments = 10

	got := buildSyncEntry(old, full, "dir", false, applyOutcome{
		PostJSONOK: true, MediaOK: true, MDOK: true, CommentsOK: true,
		CommentsWritten: true,
	})

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
	got := buildSyncEntry(old, full, "dir", false, applyOutcome{
		PostJSONOK: true, MediaOK: true, MDOK: true, CommentsOK: false,
	})

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

	got := buildSyncEntry(old, full, "dir", false, applyOutcome{
		PostJSONOK: true, MediaOK: true, MDOK: false, CommentsOK: true,
	})

	if !got.HasMd {
		t.Error("HasMd = false, want true (preserved when this run did not write md)")
	}
}

func TestBuildSyncEntry_TierClearedWhenSubLevelNil(t *testing.T) {
	old := state.PostEntry{Tier: "premium"}
	full := &boosty.Post{SubscriptionLevel: nil}

	got := buildSyncEntry(old, full, "dir", false, applyOutcome{
		PostJSONOK: true, MediaOK: true, MDOK: true, CommentsOK: true,
	})

	if got.Tier != "" {
		t.Errorf("Tier = %q, want empty (post no longer gated)", got.Tier)
	}
}

// --- decideApplyActions: pure trigger matrix ---
//
// The decision logic was previously inlined in applyItem and only covered
// indirectly. Now that it is a pure function, every cell of the trigger
// matrix is reachable in a unit test without a faked client.

func TestDecideApplyActions_Empty(t *testing.T) {
	got := decideApplyActions(syncItem{}, Config{})
	if got != (applyActions{}) {
		t.Errorf("empty item → %+v, want zero value", got)
	}
	if got.NeedFetch() {
		t.Error("NeedFetch on empty actions = true, want false")
	}
}

func TestDecideApplyActions_EditedTriggersPostAndMedia(t *testing.T) {
	got := decideApplyActions(syncItem{Edited: true}, Config{})
	if !got.Post {
		t.Error("Edited → Post = false, want true")
	}
	if !got.Media {
		t.Error("Edited → Media = false, want true")
	}
	if got.MD {
		t.Error("Edited alone (no WithMD/HasMd) → MD = true, want false")
	}
	if got.Comments {
		t.Error("Edited alone (no WithComments/HasComments) → Comments = true, want false")
	}
	if !got.NeedFetch() {
		t.Error("NeedFetch with Post/Media = false, want true")
	}
}

func TestDecideApplyActions_EditedWithMD_TriggersMD(t *testing.T) {
	got := decideApplyActions(syncItem{Edited: true}, Config{WithMD: true})
	if !got.MD {
		t.Error("Edited+WithMD → MD = false, want true")
	}
}

func TestDecideApplyActions_EditedExistingHasMD_TriggersMD(t *testing.T) {
	// No WithMD on this run, but the prior entry tracks md → must regenerate
	// to keep the on-disk artefact in sync with the edit.
	got := decideApplyActions(
		syncItem{Edited: true, Existing: state.PostEntry{HasMd: true}},
		Config{},
	)
	if !got.MD {
		t.Error("Edited+ExistingHasMd → MD = false, want true (preserve md tracking)")
	}
}

func TestDecideApplyActions_EditedExistingHasComments_TriggersComments(t *testing.T) {
	got := decideApplyActions(
		syncItem{Edited: true, Existing: state.PostEntry{HasComments: true}},
		Config{},
	)
	if !got.Comments {
		t.Error("Edited+ExistingHasComments → Comments = false, want true")
	}
}

func TestDecideApplyActions_EditedWithCommentsOptIn_TriggersComments(t *testing.T) {
	// User passes --comments NOW for a post that didn't track them before.
	// Edited+WithComments must opt the post in.
	got := decideApplyActions(syncItem{Edited: true}, Config{WithComments: true})
	if !got.Comments {
		t.Error("Edited+WithComments (no prior tracking) → Comments = false, want true")
	}
}

func TestDecideApplyActions_NewCommentsOnly(t *testing.T) {
	got := decideApplyActions(syncItem{NewComments: true}, Config{})
	if got.Post || got.Media || got.MD {
		t.Errorf("NewComments alone → Post=%v Media=%v MD=%v, want all false",
			got.Post, got.Media, got.MD)
	}
	if !got.Comments {
		t.Error("NewComments → Comments = false, want true")
	}
	if got.NeedFetch() {
		t.Error("NeedFetch with only Comments = true, want false (comments use a separate endpoint)")
	}
}

func TestDecideApplyActions_VideoMismatchOnly_TriggersMediaOnly(t *testing.T) {
	got := decideApplyActions(
		syncItem{VideoMismatch: "v.mp4: local 100 vs remote 200"},
		Config{},
	)
	if !got.Media {
		t.Error("VideoMismatch → Media = false, want true")
	}
	if got.Post || got.MD || got.Comments {
		t.Errorf("VideoMismatch alone → Post=%v MD=%v Comments=%v, want all false",
			got.Post, got.MD, got.Comments)
	}
}

func TestDecideApplyActions_MissingPostJSON(t *testing.T) {
	got := decideApplyActions(syncItem{Missing: missingFiles{PostJSON: true}}, Config{})
	if !got.Post {
		t.Error("Missing.PostJSON → Post = false, want true")
	}
	if got.Media || got.MD || got.Comments {
		t.Errorf("Missing.PostJSON only → Media=%v MD=%v Comments=%v, want all false",
			got.Media, got.MD, got.Comments)
	}
}

func TestDecideApplyActions_MissingMarkdownNeedsConfigOrPriorTracking(t *testing.T) {
	// Missing markdown alone, no WithMD, no prior HasMd → MUST NOT trigger.
	// We don't conjure md out of thin air for a post that never had it.
	got := decideApplyActions(syncItem{Missing: missingFiles{Markdown: true}}, Config{})
	if got.MD {
		t.Error("Missing.Markdown without WithMD/HasMd → MD = true, want false")
	}

	// Missing markdown + WithMD on this run → trigger.
	got = decideApplyActions(syncItem{Missing: missingFiles{Markdown: true}}, Config{WithMD: true})
	if !got.MD {
		t.Error("Missing.Markdown + WithMD → MD = false, want true")
	}

	// Missing markdown + prior HasMd (no flag) → trigger anyway, to heal the gap.
	got = decideApplyActions(
		syncItem{Missing: missingFiles{Markdown: true}, Existing: state.PostEntry{HasMd: true}},
		Config{},
	)
	if !got.MD {
		t.Error("Missing.Markdown + prior HasMd → MD = false, want true")
	}
}

func TestDecideApplyActions_MissingCommentsAlwaysTriggers(t *testing.T) {
	// FILES_MISSING for comments fires the comments fetch unconditionally —
	// the file existed before (HasComments was true at write time), no
	// further opt-in needed.
	got := decideApplyActions(syncItem{Missing: missingFiles{Comments: true}}, Config{})
	if !got.Comments {
		t.Error("Missing.Comments → Comments = false, want true")
	}
}

func TestDecideApplyActions_NeedFetch(t *testing.T) {
	cases := []struct {
		name    string
		actions applyActions
		want    bool
	}{
		{"empty", applyActions{}, false},
		{"post", applyActions{Post: true}, true},
		{"media", applyActions{Media: true}, true},
		{"md", applyActions{MD: true}, true},
		{"comments only", applyActions{Comments: true}, false},
		{"comments + post", applyActions{Comments: true, Post: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.actions.NeedFetch(); got != tc.want {
				t.Errorf("NeedFetch = %v, want %v", got, tc.want)
			}
		})
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
