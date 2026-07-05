package syncer

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/state"
)

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

	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
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

	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
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

	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
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

	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
	if got.NewComments {
		t.Error("NewComments = true, want false (no comments anywhere)")
	}
}

// CommentsCapped suppression is one-directional. disk < API on a capped post
// stays quiet (catch-up is structurally impossible — the endpoint can never
// return more than the cap), but disk > API still fires: the author deleted
// comments, and the refetch both drops the orphaned threads and re-evaluates
// the flag. Removing the suppression (refetch noise on every sync, forever)
// and making it bidirectional (deletions never heal) must both fail here.
func TestClassifyPost_CappedDiskBelowAPI_Suppressed(t *testing.T) {
	blogDir := t.TempDir()
	postDir := filepath.Join(blogDir, "post-dir")
	os.MkdirAll(postDir, 0755)
	os.WriteFile(filepath.Join(postDir, "comments.json"),
		[]byte(`[{"id":"1"},{"id":"2"},{"id":"3"}]`), 0644)

	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{
		DirName:        "post-dir",
		HasComments:    true,
		CommentsCapped: true,
		CommentsCount:  120,
		UpdatedAt:      100,
	})

	post := boosty.Post{ID: "post1", HasAccess: true, UpdatedAt: 100}
	post.Count.Comments = 120 // far above disk: the unreachable catch-up case

	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
	if got.NewComments {
		t.Error("NewComments = true, want false (capped post with disk < API must stay suppressed)")
	}
}

func TestClassifyPost_CappedDiskAboveAPI_StillFires(t *testing.T) {
	blogDir := t.TempDir()
	postDir := filepath.Join(blogDir, "post-dir")
	os.MkdirAll(postDir, 0755)
	os.WriteFile(filepath.Join(postDir, "comments.json"),
		[]byte(`[{"id":"1"},{"id":"2"},{"id":"3"},{"id":"4"},{"id":"5"}]`), 0644)

	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{
		DirName:        "post-dir",
		HasComments:    true,
		CommentsCapped: true,
		CommentsCount:  120,
		UpdatedAt:      100,
	})

	post := boosty.Post{ID: "post1", HasAccess: true, UpdatedAt: 100}
	post.Count.Comments = 3 // deletions pushed API below disk

	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
	if !got.NewComments {
		t.Error("NewComments = false, want true (deletions must fire even for capped posts so the flag can heal)")
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

	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
	if !got.NewComments {
		t.Error("NewComments = false, want true (HasComments=false, count grew 3→5)")
	}
}

// --- classifyPost: lifecycle branches ---
//
// The original test set covered only NewComments paths. The lifecycle
// branches (IsNew / IsLockedNew / JustLocked / JustUnlocked / Edited /
// BackfillUpdatedAt) drive the apply phase's full-redownload vs in-place
// update decision and need their own coverage.

func TestClassifyPost_NotInState_HasAccess_IsNew(t *testing.T) {
	blogDir := t.TempDir()
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}

	post := boosty.Post{ID: "post1", Title: "Hello", HasAccess: true, UpdatedAt: 200}
	post.PublishTime = 1700000000

	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
	if !got.IsNew {
		t.Error("IsNew = false, want true")
	}
	if got.InState {
		t.Error("InState = true, want false")
	}
	if got.IsLockedNew {
		t.Error("IsLockedNew = true, want false (HasAccess=true)")
	}
	if got.DirName != "" {
		t.Errorf("DirName = %q, want empty for out-of-state posts (SavePost decides the real name at apply time)", got.DirName)
	}
}

func TestClassifyPost_NotInState_NoAccess_IsLockedNew(t *testing.T) {
	blogDir := t.TempDir()
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}

	post := boosty.Post{ID: "post1", Title: "Hello", HasAccess: false}
	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
	if !got.IsLockedNew {
		t.Error("IsLockedNew = false, want true")
	}
	if got.IsNew {
		t.Error("IsNew = true, want false (no access)")
	}
}

func TestClassifyPost_InState_LostAccess_JustLocked(t *testing.T) {
	blogDir := t.TempDir()
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{DirName: "post-dir", UpdatedAt: 100, Locked: false})

	post := boosty.Post{ID: "post1", HasAccess: false}
	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
	if !got.JustLocked {
		t.Error("JustLocked = false, want true")
	}
	if got.Edited {
		t.Error("Edited = true, want false (we don't even look at UpdatedAt for locked posts)")
	}
	if got.DirName != "post-dir" {
		t.Errorf("DirName = %q, want 'post-dir' (preserved for locked posts)", got.DirName)
	}
}

func TestClassifyPost_InState_AlreadyLocked_NoFlag(t *testing.T) {
	blogDir := t.TempDir()
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{DirName: "post-dir", Locked: true})

	post := boosty.Post{ID: "post1", HasAccess: false}
	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
	if got.JustLocked {
		t.Error("JustLocked = true, want false (was already locked)")
	}
	if got.IsActionable() {
		t.Error("IsActionable = true, want false (no change for an already-locked post)")
	}
}

func TestClassifyPost_InState_RegainedAccess_JustUnlocked_RefreshesDirName(t *testing.T) {
	blogDir := t.TempDir()
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{
		DirName: "old-dir-name", Locked: true, UpdatedAt: 100,
	})

	post := boosty.Post{
		ID: "post1", Title: "Brand New Title", HasAccess: true,
		PublishTime: 1700000000, UpdatedAt: 100,
	}
	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
	if !got.JustUnlocked {
		t.Error("JustUnlocked = false, want true")
	}
	// DirName must be re-formatted from the current title (the previous run
	// may have stored a placeholder name when the post was locked).
	if got.DirName == "old-dir-name" {
		t.Error("DirName preserved on JustUnlocked; want fresh format from current title")
	}
}

func TestClassifyPost_InState_UpdatedAtChanged_Edited(t *testing.T) {
	blogDir := t.TempDir()
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{DirName: "d", UpdatedAt: 100})

	post := boosty.Post{ID: "post1", HasAccess: true, UpdatedAt: 200}
	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
	if !got.Edited {
		t.Error("Edited = false, want true (UpdatedAt 100→200)")
	}
	if got.BackfillUpdatedAt {
		t.Error("BackfillUpdatedAt = true, want false (had a real prior value)")
	}
}

func TestClassifyPost_InState_UpdatedAtUnchanged_NotEdited(t *testing.T) {
	blogDir := t.TempDir()
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{DirName: "d", UpdatedAt: 100})

	post := boosty.Post{ID: "post1", HasAccess: true, UpdatedAt: 100}
	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
	if got.Edited {
		t.Error("Edited = true, want false (UpdatedAt unchanged)")
	}
}

func TestClassifyPost_InState_LegacyZeroUpdatedAt_Backfill(t *testing.T) {
	// Legacy entries pre-UpdatedAt schema have UpdatedAt=0. First sync after
	// upgrade must NOT flag them as Edited; instead it backfills the value.
	blogDir := t.TempDir()
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Add("post1", state.PostEntry{DirName: "d", UpdatedAt: 0})

	post := boosty.Post{ID: "post1", HasAccess: true, UpdatedAt: 12345}
	got := classifyPost(post, st, blogDir, parser.DefaultFormat)
	if got.Edited {
		t.Error("Edited = true, want false (legacy UpdatedAt=0 must backfill, not edit)")
	}
	if !got.BackfillUpdatedAt {
		t.Error("BackfillUpdatedAt = false, want true")
	}
	if got.IsActionable() {
		t.Error("IsActionable = true, want false (pure backfill is persisted but not actionable)")
	}
}

// --- syncItem methods: IsActionable, Labels, Detail ---

func TestSyncItem_IsActionable(t *testing.T) {
	cases := []struct {
		name string
		item syncItem
		want bool
	}{
		{"zero", syncItem{}, false},
		{"backfill only", syncItem{BackfillUpdatedAt: true}, false},
		{"new", syncItem{IsNew: true}, true},
		{"unlocked", syncItem{JustUnlocked: true}, true},
		{"locked", syncItem{JustLocked: true}, true},
		{"edited", syncItem{Edited: true}, true},
		{"new comments", syncItem{NewComments: true}, true},
		{"video mismatch", syncItem{VideoMismatch: "x"}, true},
		{"missing files", syncItem{Missing: missingFiles{PostJSON: true}}, true},
		// IsLockedNew is NOT actionable on its own — apply skips it (no access
		// to download anything).
		{"locked new", syncItem{IsLockedNew: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.IsActionable(); got != tc.want {
				t.Errorf("IsActionable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSyncItem_Labels(t *testing.T) {
	cases := []struct {
		name string
		item syncItem
		want []string
	}{
		{"new", syncItem{IsNew: true}, []string{"NEW"}},
		{"locked new", syncItem{IsLockedNew: true}, []string{"LOCKED_NEW"}},
		{"locked", syncItem{JustLocked: true}, []string{"LOCKED"}},
		{"unlocked", syncItem{JustUnlocked: true}, []string{"UNLOCKED"}},
		{"edited only", syncItem{Edited: true}, []string{"UPDATED"}},
		{"comments", syncItem{NewComments: true}, []string{"COMMENTS"}},
		{"video mismatch", syncItem{VideoMismatch: "x"}, []string{"VIDEO_MISMATCH"}},
		{"missing", syncItem{Missing: missingFiles{Comments: true}}, []string{"FILES_MISSING"}},
		{
			"edited + comments + video + missing",
			syncItem{
				Edited:        true,
				NewComments:   true,
				VideoMismatch: "x",
				Missing:       missingFiles{PostJSON: true},
			},
			[]string{"UPDATED", "COMMENTS", "VIDEO_MISMATCH", "FILES_MISSING"},
		},
		// The lifecycle quartet (NEW/LOCKED_NEW/LOCKED/UNLOCKED) is exclusive
		// — only the first match wins, but other flags still attach.
		{"new wins over locked-new + still gets edited", syncItem{IsNew: true, Edited: true}, []string{"NEW", "UPDATED"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.item.Labels()
			if !slices.Equal(got, tc.want) {
				t.Errorf("Labels = %v, want %v", got, tc.want)
			}
		})
	}
}

// Detail() picks its "from" comment count carefully: the disk count when
// available (HasComments=true and file readable), otherwise the cached state
// count. This is the value the user sees in `[COMMENTS] (comments: X → Y)`.
func TestSyncItem_Detail_CommentsFromDisk(t *testing.T) {
	item := syncItem{
		Existing:         state.PostEntry{HasComments: true, CommentsCount: 99},
		DiskCommentCount: 3,
		NewComments:      true,
	}
	item.Post.Count.Comments = 5

	if got := item.Detail(); !strings.Contains(got, "comments: 3 → 5") {
		t.Errorf("Detail = %q, want disk count (3) → API (5), not cached (99)", got)
	}
}

func TestSyncItem_Detail_CommentsFileMissingNamesTheReason(t *testing.T) {
	// HasComments=true but disk file missing (DiskCommentCount = -1): the
	// trigger fired on the missing file, not a count delta, so the detail
	// names the real reason. A "comments: 7 → 9" (or worse, "9 → 9" when
	// the API count is unchanged) would read as a spurious no-op.
	item := syncItem{
		Existing:         state.PostEntry{HasComments: true, CommentsCount: 7},
		DiskCommentCount: -1,
		NewComments:      true,
	}
	item.Post.Count.Comments = 9

	got := item.Detail()
	if !strings.Contains(got, "comments.json missing or unreadable (API: 9)") {
		t.Errorf("Detail = %q, want the missing-file reason with the API count", got)
	}
	if strings.Contains(got, "→") {
		t.Errorf("Detail = %q, must not render a from→to pair for a missing file", got)
	}
}

func TestSyncItem_Detail_CommentsFromCachedWhenHasCommentsFalse(t *testing.T) {
	// HasComments=false → no disk file ever existed; cached state is the
	// only available source even if DiskCommentCount happens to be set.
	item := syncItem{
		Existing:         state.PostEntry{HasComments: false, CommentsCount: 3},
		DiskCommentCount: 99,
		NewComments:      true,
	}
	item.Post.Count.Comments = 8

	if got := item.Detail(); !strings.Contains(got, "comments: 3 → 8") {
		t.Errorf("Detail = %q, want cached count (3) when !HasComments", got)
	}
}

func TestSyncItem_Detail_MultipleParts(t *testing.T) {
	item := syncItem{
		Edited:        true,
		VideoMismatch: "v.mp4: local 100 vs remote 200",
		Missing:       missingFiles{PostJSON: true, Comments: true},
	}
	got := item.Detail()
	for _, want := range []string{"post edited", "v.mp4", "missing post.json, comments.json"} {
		if !strings.Contains(got, want) {
			t.Errorf("Detail = %q, missing %q", got, want)
		}
	}
	if strings.Count(got, "; ") < 2 {
		t.Errorf("Detail = %q, want at least two '; ' separators between parts", got)
	}
}

func TestSyncItem_Detail_LockedPaths(t *testing.T) {
	if got := (syncItem{JustLocked: true}).Detail(); !strings.Contains(got, "now locked") {
		t.Errorf("JustLocked Detail = %q, want 'now locked'", got)
	}
	if got := (syncItem{JustUnlocked: true}).Detail(); !strings.Contains(got, "now accessible") {
		t.Errorf("JustUnlocked Detail = %q, want 'now accessible'", got)
	}
}

func TestSyncItem_Detail_Empty(t *testing.T) {
	if got := (syncItem{}).Detail(); got != "" {
		t.Errorf("Detail() on empty item = %q, want ''", got)
	}
}
