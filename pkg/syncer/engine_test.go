package syncer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/state"
)

// Phase 3: integration tests that exercise Engine against a fake Boosty API.
//
// Pure-function coverage (decideApplyActions, buildSyncEntry, classifyPost,
// dirReserver) lives in apply_test.go / classify_test.go / save_test.go.
// These tests verify the call chain — fetch → write → state.Save —
// composes the way the unit tests claim.

// --- SavePost ---

func TestEngine_SavePost_HappyPathWritesAllArtefacts(t *testing.T) {
	f := newFakeAPI(t)
	f.Media("img.jpg", []byte("FAKEIMAGE"))

	post := boosty.Post{
		ID:          "p1",
		Title:       "Hello",
		HasAccess:   true,
		PublishTime: 1700000000,
		UpdatedAt:   1700000100,
		Data: []boosty.ContentBlock{
			{Type: "text", Content: `["intro","unstyled",[]]`},
			{Type: "image", URL: f.MediaURL("img.jpg")},
		},
	}

	cfg := Config{
		Blog:      "myblog",
		OutputDir: t.TempDir(),
		WithMD:    true,
	}
	e := New(f.client, cfg)

	dirName, _, err := e.SavePost(&post)
	if err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	if dirName == "" {
		t.Fatal("dirName empty for accessible post")
	}

	postDir := filepath.Join(cfg.OutputDir, cfg.Blog, dirName)
	requireFile(t, filepath.Join(postDir, "post.json"))
	requireFile(t, filepath.Join(postDir, "post.md"))

	img, err := os.ReadFile(filepath.Join(postDir, "image_001.jpg"))
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	if string(img) != "FAKEIMAGE" {
		t.Errorf("image bytes = %q, want FAKEIMAGE", string(img))
	}
}

// No-access posts must not write anything — including the blog directory.
// Earlier code created the blog dir unconditionally; the contract is that
// SavePost is a no-op for locked posts so a syncing loop can skip them
// silently without leaving empty directories behind.
func TestEngine_SavePost_NoAccessIsNoOp(t *testing.T) {
	f := newFakeAPI(t)
	post := boosty.Post{ID: "p1", Title: "Locked", HasAccess: false}

	cfg := Config{Blog: "myblog", OutputDir: t.TempDir(), WithMD: true}
	e := New(f.client, cfg)

	dirName, _, err := e.SavePost(&post)
	if err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	if dirName != "" {
		t.Errorf("dirName = %q, want empty for no-access post", dirName)
	}
	if _, err := os.Stat(filepath.Join(cfg.OutputDir, cfg.Blog)); err == nil {
		t.Errorf("blog dir created for no-access post — should not exist")
	}
}

// --- applyItem: fail-closed state-after-failure contract via Client ---
//
// buildSyncEntry's unit tests pin down the per-flag contract; this test
// proves the contract actually composes in applyItem when a real HTTP call
// fails. Without this, a regression that bypasses out.CommentsOK could pass
// every unit test and still break the state-vs-disk invariant in production.

func TestEngine_ApplyItem_EditedAllSucceedsAdvancesUpdatedAt(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)

	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	if err := os.MkdirAll(postDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Stale on-disk artefacts the apply pass should rewrite.
	writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1","title":"old"}`)
	writeFile(t, filepath.Join(postDir, "comments.json"), `[]`)
	writeFile(t, filepath.Join(postDir, "post.md"), "# old")

	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "old", DirName: "p1_dir", UpdatedAt: 100, HasComments: true, HasMd: true},
	})

	newPost := boosty.Post{
		ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 200,
		Count: boosty.PostCount{Comments: 1},
		Data:  []boosty.ContentBlock{{Type: "text", Content: `["body","unstyled",[]]`}},
	}
	f.SinglePost(blog, "p1", newPost)
	f.CommentsList(blog, "p1", boosty.Comment{ID: "c1", CreatedAt: 12345})

	cfg := Config{Blog: blog, OutputDir: outDir, WithMD: true, WithComments: true}
	e := New(f.client, cfg)

	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	item := syncItem{
		Post: boosty.Post{
			ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 200,
			Count: boosty.PostCount{Comments: 1},
		},
		DirName:  "p1_dir",
		Existing: st.Posts["p1"],
		InState:  true,
		Edited:   true,
	}
	e.applyItem(blogDir, st, item)

	reloaded, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Posts["p1"]
	if got.UpdatedAt != 200 {
		t.Errorf("UpdatedAt = %d, want 200", got.UpdatedAt)
	}
	if got.Title != "new" {
		t.Errorf("Title = %q, want 'new'", got.Title)
	}
	pj, _ := os.ReadFile(filepath.Join(postDir, "post.json"))
	if !strings.Contains(string(pj), `"title": "new"`) {
		t.Errorf("post.json not refreshed: %s", pj)
	}
}

// 404 on the comments endpoint is non-transient (GetJSON only retries 5xx/429),
// so the call fails fast and applyItem records out.CommentsOK=false. The
// load-bearing assertion: UpdatedAt MUST stay at the old value, otherwise
// the next sync will see state in sync with API and never re-attempt the
// failed comments fetch.
func TestEngine_ApplyItem_FailedCommentsPreservesUpdatedAt(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)

	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	if err := os.MkdirAll(postDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1"}`)
	writeFile(t, filepath.Join(postDir, "comments.json"), `[]`)
	writeFile(t, filepath.Join(postDir, "post.md"), "# old")

	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "old", DirName: "p1_dir", UpdatedAt: 100, HasComments: true, HasMd: true},
	})

	newPost := boosty.Post{
		ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 200,
		Count: boosty.PostCount{Comments: 1},
		Data:  []boosty.ContentBlock{{Type: "text", Content: `["body","unstyled",[]]`}},
	}
	f.SinglePost(blog, "p1", newPost)
	// CommentsList intentionally not registered → 404 from fakeAPI default.

	cfg := Config{Blog: blog, OutputDir: outDir, WithMD: true, WithComments: true}
	e := New(f.client, cfg)

	st, _ := state.Load(blogDir)
	item := syncItem{
		Post: boosty.Post{
			ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 200,
			Count: boosty.PostCount{Comments: 1},
		},
		DirName:  "p1_dir",
		Existing: st.Posts["p1"],
		InState:  true,
		Edited:   true,
	}
	e.applyItem(blogDir, st, item)

	reloaded, _ := state.Load(blogDir)
	if got := reloaded.Posts["p1"].UpdatedAt; got != 100 {
		t.Errorf("UpdatedAt = %d, want 100 (advanced despite comments failure)", got)
	}
}

// Regression: edited post whose post.md write fails must not
// advance UpdatedAt — otherwise the next sync sees state in sync with API
// and never retries the markdown. apply_test.go pins this on buildSyncEntry;
// this test pins the full applyItem → state.Save path so a future regression
// that bypasses out.MDOK would not pass the unit tests yet still break
// production. We force the md write to fail by pre-creating a directory at
// post.md's path: WriteFileAtomic ultimately rename()s onto post.md, which
// errors when the destination is a non-empty directory on every platform.
func TestEngine_ApplyItem_EditedMdFailurePreservesUpdatedAt(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)

	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	if err := os.MkdirAll(postDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1"}`)
	writeFile(t, filepath.Join(postDir, "comments.json"), `[]`)
	// Pre-create post.md AS A DIRECTORY with content so the upcoming
	// WriteFileAtomic rename onto it fails on every OS. A bare empty dir
	// would also fail on Linux/macOS but not on Windows.
	mdAsDir := filepath.Join(postDir, "post.md")
	if err := os.Mkdir(mdAsDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(mdAsDir, "block.txt"), "blocking")

	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "old", DirName: "p1_dir", UpdatedAt: 100, HasComments: true, HasMd: true},
	})

	newPost := boosty.Post{
		ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 200,
		Count: boosty.PostCount{Comments: 1},
		Data:  []boosty.ContentBlock{{Type: "text", Content: `["body","unstyled",[]]`}},
	}
	f.SinglePost(blog, "p1", newPost)
	f.CommentsList(blog, "p1", boosty.Comment{ID: "c1", CreatedAt: 12345})

	cfg := Config{Blog: blog, OutputDir: outDir, WithMD: true, WithComments: true}
	e := New(f.client, cfg)

	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	item := syncItem{
		Post: boosty.Post{
			ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 200,
			Count: boosty.PostCount{Comments: 1},
		},
		DirName:  "p1_dir",
		Existing: st.Posts["p1"],
		InState:  true,
		Edited:   true,
	}
	e.applyItem(blogDir, st, item)

	reloaded, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Posts["p1"]
	if got.UpdatedAt != 100 {
		t.Errorf("UpdatedAt = %d, want 100 (advanced despite md failure)", got.UpdatedAt)
	}
	if !got.HasMd {
		t.Error("HasMd = false, want true (prior tracking must be preserved when this run did not write md)")
	}
}

// --- fetchFullPost stub guard ---
//
// The per-post endpoint can return a degraded payload (HasAccess=false or
// empty Data) when the subscription lapses between the list call and the
// per-post call. Without the guard, the stub would be written over good
// post.json, empty Data would parse into zero media (trivially "succeeding"),
// every channel would tick OK, and buildSyncEntry would advance UpdatedAt —
// permanent on-disk damage no later sync could detect.

func TestEngine_ApplyItem_StubPostPreservesDiskAndState(t *testing.T) {
	stubs := map[string]boosty.Post{
		"no_access": {ID: "p1", Title: "new", HasAccess: false, UpdatedAt: 200,
			Data: []boosty.ContentBlock{{Type: "text", Content: `["x","unstyled",[]]`}}},
		"empty_data": {ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 200},
	}
	for name, stub := range stubs {
		t.Run(name, func(t *testing.T) {
			blog := "myblog"
			f := newFakeAPI(t)
			outDir := t.TempDir()
			blogDir := filepath.Join(outDir, blog)
			postDir := filepath.Join(blogDir, "p1_dir")
			mustMkdir(t, postDir)
			writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1","title":"good payload"}`)
			writeInitialState(t, blogDir, map[string]state.PostEntry{
				"p1": {Title: "old", DirName: "p1_dir", UpdatedAt: 100},
			})
			f.SinglePost(blog, "p1", stub)

			e := New(f.client, Config{Blog: blog, OutputDir: outDir})
			st, err := state.Load(blogDir)
			if err != nil {
				t.Fatal(err)
			}
			item := syncItem{
				Post:     boosty.Post{ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 200},
				DirName:  "p1_dir",
				Existing: st.Posts["p1"],
				InState:  true,
				Edited:   true,
			}
			e.applyItem(blogDir, st, item)

			pj, _ := os.ReadFile(filepath.Join(postDir, "post.json"))
			if !strings.Contains(string(pj), "good payload") {
				t.Errorf("post.json was overwritten by the stub: %s", pj)
			}
			reloaded, _ := state.Load(blogDir)
			if got := reloaded.Posts["p1"].UpdatedAt; got != 100 {
				t.Errorf("UpdatedAt = %d, want 100 (a stub must never advance state)", got)
			}
			if got := e.failedPosts.Load(); got != 1 {
				t.Errorf("failedPosts = %d, want 1 (skipped apply must surface as a failure)", got)
			}
		})
	}
}

// MaybeRefreshSignedURLs carries the same stub guard: a degraded per-post
// payload must not replace the (complete) list-endpoint copy.
func TestMaybeRefreshSignedURLs_StubKeepsListPayload(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	f.SinglePost(blog, "p1", boosty.Post{ID: "p1", HasAccess: false}) // stub

	listPost := boosty.Post{
		ID: "p1", Title: "from list", HasAccess: true,
		Data: []boosty.ContentBlock{{Type: "ok_video", PlayerURLs: []boosty.PlayerURL{
			{Type: "high", URL: "https://cdn.example/v.mp4"},
		}}},
	}
	e := New(f.client, Config{Blog: blog, OutputDir: t.TempDir()})
	got := e.MaybeRefreshSignedURLs(&listPost)
	if got.Title != "from list" || !got.HasAccess {
		t.Errorf("stub must fall back to the list-endpoint payload, got %+v", got)
	}
}

// --- applyJustUnlocked: Locked clears only when every channel landed ---
//
// A tier-toggle lock with no content edit has no other re-trigger path
// (UpdatedAt unchanged → not Edited; Locked already false → not
// JustUnlocked), so clearing Locked on partial success would strand stale
// artefacts forever. These tests pin the all-channels-OK coupling end to end.

func TestEngine_ApplyItem_UnlockedAllOKClearsLocked(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	mustMkdir(t, postDir)
	writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1","title":"stale pre-lock"}`)
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "old", DirName: "p1_dir", UpdatedAt: 100, Locked: true},
	})

	fresh := boosty.Post{
		ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 200,
		Data: []boosty.ContentBlock{{Type: "text", Content: `["body","unstyled",[]]`}},
	}
	f.SinglePost(blog, "p1", fresh)

	e := New(f.client, Config{Blog: blog, OutputDir: outDir})
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	item := syncItem{
		Post:         fresh,
		DirName:      "p1_freshname", // classify re-derived name; surviving dir must win
		Existing:     st.Posts["p1"],
		InState:      true,
		JustUnlocked: true,
	}
	e.applyItem(blogDir, st, item)

	reloaded, _ := state.Load(blogDir)
	got := reloaded.Posts["p1"]
	if got.Locked {
		t.Error("Locked = true, want false (every channel succeeded)")
	}
	if got.UpdatedAt != 200 {
		t.Errorf("UpdatedAt = %d, want 200", got.UpdatedAt)
	}
	if got.DirName != "p1_dir" {
		t.Errorf("DirName = %q, want 'p1_dir' (surviving on-disk dir must be reused, not orphaned)", got.DirName)
	}
	if n := e.failedPosts.Load(); n != 0 {
		t.Errorf("failedPosts = %d, want 0", n)
	}
}

func TestEngine_ApplyItem_UnlockedPartialFailureKeepsLocked(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	mustMkdir(t, postDir)
	writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1","title":"stale pre-lock"}`)
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		// HasComments=true forces the comments channel; its endpoint is NOT
		// registered below, so the fetch 404s and the channel fails.
		"p1": {Title: "old", DirName: "p1_dir", UpdatedAt: 100, Locked: true, HasComments: true},
	})

	fresh := boosty.Post{
		ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 200,
		Data: []boosty.ContentBlock{{Type: "text", Content: `["body","unstyled",[]]`}},
	}
	f.SinglePost(blog, "p1", fresh)
	// CommentsList intentionally not registered → 404 → CommentsOK=false.

	e := New(f.client, Config{Blog: blog, OutputDir: outDir})
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	item := syncItem{
		Post:         fresh,
		DirName:      "p1_dir",
		Existing:     st.Posts["p1"],
		InState:      true,
		JustUnlocked: true,
	}
	e.applyItem(blogDir, st, item)

	reloaded, _ := state.Load(blogDir)
	got := reloaded.Posts["p1"]
	if !got.Locked {
		t.Error("Locked = false, want true (partial failure must keep the JustUnlocked trigger armed)")
	}
	if got.UpdatedAt != 100 {
		t.Errorf("UpdatedAt = %d, want 100 (must not advance on partial failure)", got.UpdatedAt)
	}
	// One failed post = exactly one failedPosts increment, even though both
	// an artefact channel failed AND state was saved afterwards.
	if n := e.failedPosts.Load(); n != 1 {
		t.Errorf("failedPosts = %d, want exactly 1 per failed post", n)
	}
}

// --- applyUpdate: directory reservation under contention ---
//
// applyUpdate reserves item.DirName through the dirReserver so a same-run
// colliding NEW post cannot claim this post's directory while its post.json
// is missing (the Missing.PostJSON repair window, where the disk probe
// reports the dir as free). The load-bearing coupling: the RESERVED name —
// possibly suffixed — must feed both the artefact writes and the state
// entry. A drift that writes to the suffixed dir but records item.DirName
// (or vice versa) would split disk from state and reinstate the cross-post
// interleaving this wiring exists to prevent.
func TestEngine_ApplyUpdate_ReservedDirGetsSuffix(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	mustMkdir(t, blogDir)
	// In state, but NO post.json on disk — the repair window.
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"aaaabbbbcccc": {Title: "old", DirName: "shared", UpdatedAt: 100, HasComments: true},
	})
	f.CommentsList(blog, "aaaabbbbcccc", boosty.Comment{ID: "c1"})

	e := New(f.client, Config{Blog: blog, OutputDir: outDir})
	// Simulate the same-run colliding NEW post that already claimed "shared".
	e.res.reserve(blogDir, "otherpost99", "shared")

	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	item := syncItem{
		Post: boosty.Post{
			ID: "aaaabbbbcccc", Title: "new", HasAccess: true, UpdatedAt: 100,
			Count: boosty.PostCount{Comments: 1},
		},
		DirName:     "shared",
		Existing:    st.Posts["aaaabbbbcccc"],
		InState:     true,
		NewComments: true, // comments-only trigger: no per-post fetch needed
	}
	e.applyItem(blogDir, st, item)

	const want = "shared_aaaabbbb"
	requireFile(t, filepath.Join(blogDir, want, "comments.json"))
	if _, err := os.Stat(filepath.Join(blogDir, "shared", "comments.json")); !os.IsNotExist(err) {
		t.Errorf("comments.json written into the contested 'shared' dir, stat err = %v", err)
	}
	reloaded, _ := state.Load(blogDir)
	if got := reloaded.Posts["aaaabbbbcccc"].DirName; got != want {
		t.Errorf("state DirName = %q, want %q (must record the same reserved name the artefacts used)", got, want)
	}
}

// --- downloadComments: cap detection at the structural boundary ---
//
// commentsPageLimit=101 exists so a post with EXACTLY 100 top-level threads
// (uncapped) is distinguishable from one that hit the cap. A flipped
// comparison here silently sets commentsCapped on boundary posts and
// permanently suppresses their comment refetch; the per-thread signal
// (replyCount > inlined replies) is the only way reply truncation is ever
// detected.
func TestEngine_DownloadComments_CapBoundary(t *testing.T) {
	mk := func(n int) []boosty.Comment {
		cs := make([]boosty.Comment, n)
		for i := range cs {
			cs[i] = boosty.Comment{ID: fmt.Sprintf("c%d", i)}
		}
		return cs
	}
	cases := []struct {
		name     string
		comments []boosty.Comment
		want     bool
	}{
		{"exactly_100_uncapped", mk(100), false},
		{"101_capped", mk(101), true},
		{"reply_undercount_capped", []boosty.Comment{
			{ID: "c1", ReplyCount: 5, Replies: &boosty.CommentsResponse{
				Data: []boosty.Comment{{ID: "r1"}},
			}},
		}, true},
		{"replies_fully_inlined_uncapped", []boosty.Comment{
			{ID: "c1", ReplyCount: 1, Replies: &boosty.CommentsResponse{
				Data: []boosty.Comment{{ID: "r1"}},
			}},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blog := "myblog"
			f := newFakeAPI(t)
			f.CommentsList(blog, "p1", tc.comments...)
			e := New(f.client, Config{Blog: blog, OutputDir: t.TempDir()})

			dir := t.TempDir()
			capped, err := e.downloadComments("p1", dir, len(tc.comments))
			if err != nil {
				t.Fatalf("downloadComments: %v", err)
			}
			if capped != tc.want {
				t.Errorf("capped = %v, want %v", capped, tc.want)
			}
			requireFile(t, filepath.Join(dir, "comments.json"))
		})
	}
}

// buildSyncEntry rewrites CommentsCapped from the fresh fetch — but ONLY
// when comments were actually written this run; otherwise the prior flag
// must survive (clearing it without a fetch would re-enable the futile
// refetch loop the flag suppresses).
func TestBuildSyncEntry_CommentsCappedFollowsFreshFetch(t *testing.T) {
	post := &boosty.Post{Title: "t"}
	allOK := applyOutcome{PostJSONOK: true, MediaOK: true, MDOK: true, CommentsOK: true}

	fetchedUncapped := allOK
	fetchedUncapped.CommentsWritten = true
	fetchedUncapped.CommentsCapped = false
	if e := buildSyncEntry(state.PostEntry{CommentsCapped: true, HasComments: true},
		post, "d", false, fetchedUncapped); e.CommentsCapped {
		t.Error("CommentsCapped = true, want false (fresh uncapped fetch must clear the flag)")
	}

	fetchedCapped := allOK
	fetchedCapped.CommentsWritten = true
	fetchedCapped.CommentsCapped = true
	if e := buildSyncEntry(state.PostEntry{}, post, "d", false, fetchedCapped); !e.CommentsCapped {
		t.Error("CommentsCapped = false, want true (capped fetch must set the flag)")
	}

	if e := buildSyncEntry(state.PostEntry{CommentsCapped: true, HasComments: true},
		post, "d", false, allOK); !e.CommentsCapped {
		t.Error("CommentsCapped = false, want true (no comments written this run → prior flag survives)")
	}
}

// --- DownloadAll: concurrent workers ---

func TestEngine_DownloadAll_MultiWorkerSavesAllPosts(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)

	posts := []boosty.Post{
		{ID: "p1", Title: "First", HasAccess: true, PublishTime: 1700000000, UpdatedAt: 100},
		{ID: "p2", Title: "Second", HasAccess: true, PublishTime: 1700000100, UpdatedAt: 200},
		{ID: "p3", Title: "Third", HasAccess: true, PublishTime: 1700000200, UpdatedAt: 300},
	}
	f.PostsList(blog, posts...)

	cfg := Config{Blog: blog, OutputDir: t.TempDir(), Workers: 2}
	e := New(f.client, cfg)
	if err := e.DownloadAll(); err != nil {
		t.Fatalf("DownloadAll: %v", err)
	}

	st, err := state.Load(filepath.Join(cfg.OutputDir, cfg.Blog))
	if err != nil {
		t.Fatal(err)
	}
	if st.Count() != 3 {
		t.Errorf("state.Count = %d, want 3", st.Count())
	}
	for _, p := range posts {
		if !st.Has(p.ID) {
			t.Errorf("state missing post %q", p.ID)
		}
	}
}

// --- Sync end-to-end ---

// Empty state + 1 new post + AutoApply → applyItem hits the IsNew branch,
// SavePost runs, state ends up with one entry. Covers the cross-cut Sync
// path that classifyPost → applyItem dispatch and is the simplest smoke
// test of the whole pipeline.
func TestEngine_Sync_NewPostAutoApply(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)

	post := boosty.Post{
		ID: "p1", Title: "Brand New", HasAccess: true,
		PublishTime: 1700000000, UpdatedAt: 100,
		Data: []boosty.ContentBlock{{Type: "text", Content: `["body","unstyled",[]]`}},
	}
	f.PostsList(blog, post)

	cfg := Config{Blog: blog, OutputDir: t.TempDir(), AutoApply: true, WithMD: true}
	e := New(f.client, cfg)
	if err := e.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	st, err := state.Load(filepath.Join(cfg.OutputDir, cfg.Blog))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Has("p1") {
		t.Errorf("Sync did not record NEW post p1; log:\n%s", f.log.joined())
	}
}

// Sync without AutoApply must consult cfg.In for confirmation. "y\n" applies
// the change; "n\n" (or anything else) cancels. The injected reader replaces
// os.Stdin so the test does not need a TTY. Without the cfg.In field this
// path was untestable — the prompt blocked on real stdin.
func TestEngine_Sync_PromptApply_Yes(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)

	post := boosty.Post{
		ID: "p1", Title: "Prompted", HasAccess: true,
		PublishTime: 1700000000, UpdatedAt: 100,
		Data: []boosty.ContentBlock{{Type: "text", Content: `["body","unstyled",[]]`}},
	}
	f.PostsList(blog, post)

	cfg := Config{
		Blog:      blog,
		OutputDir: t.TempDir(),
		WithMD:    true,
		In:        strings.NewReader("y\n"),
	}
	e := New(f.client, cfg)
	if err := e.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	st, err := state.Load(filepath.Join(cfg.OutputDir, cfg.Blog))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Has("p1") {
		t.Errorf("Sync did not apply after Y prompt; log:\n%s", f.log.joined())
	}
}

func TestEngine_Sync_PromptApply_No(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)

	post := boosty.Post{
		ID: "p1", Title: "Declined", HasAccess: true,
		PublishTime: 1700000000, UpdatedAt: 100,
		Data: []boosty.ContentBlock{{Type: "text", Content: `["body","unstyled",[]]`}},
	}
	f.PostsList(blog, post)

	cfg := Config{
		Blog:      blog,
		OutputDir: t.TempDir(),
		WithMD:    true,
		In:        strings.NewReader("n\n"),
	}
	e := New(f.client, cfg)
	if err := e.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// _state.json should not exist — Sync cancelled before any writes.
	if _, err := os.Stat(filepath.Join(cfg.OutputDir, cfg.Blog, state.FileName)); err == nil {
		t.Errorf("state file created despite N prompt")
	}
}

// --- runCheckMedia: worker-pool dispatch ---
//
// checks_test.go exhaustively covers checkRemoteVideoSize (the leaf). This
// test covers the dispatcher itself: items in state with ok_video flow
// through workers and end up with VideoMismatch set per their local-vs-HEAD
// delta. Items without ok_video and items not in state are skipped.
func TestEngine_RunCheckMedia_FlagsOnlyMismatchedPosts(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)

	// p1: 4 bytes local, 4 bytes HEAD — match.
	mustMkdir(t, filepath.Join(blogDir, "p1_dir"))
	writeFile(t, filepath.Join(blogDir, "p1_dir", "video_001.mp4"), "AAAA")
	f.Media("v1.mp4", []byte("AAAA"))
	f.SinglePost(blog, "p1", boosty.Post{
		ID: "p1", HasAccess: true,
		Data: []boosty.ContentBlock{{Type: "ok_video", PlayerURLs: []boosty.PlayerURL{
			{Type: "high", URL: f.MediaURL("v1.mp4")},
		}}},
	})

	// p2: 5 bytes local, 999 bytes HEAD — mismatch.
	mustMkdir(t, filepath.Join(blogDir, "p2_dir"))
	writeFile(t, filepath.Join(blogDir, "p2_dir", "video_001.mp4"), "BBBBB")
	f.Media("v2.mp4", make([]byte, 999))
	f.SinglePost(blog, "p2", boosty.Post{
		ID: "p2", HasAccess: true,
		Data: []boosty.ContentBlock{{Type: "ok_video", PlayerURLs: []boosty.PlayerURL{
			{Type: "high", URL: f.MediaURL("v2.mp4")},
		}}},
	})

	items := []syncItem{
		{
			Post:    boosty.Post{ID: "p1", HasAccess: true, Data: []boosty.ContentBlock{{Type: "ok_video"}}},
			DirName: "p1_dir",
			InState: true,
		},
		{
			Post:    boosty.Post{ID: "p2", HasAccess: true, Data: []boosty.ContentBlock{{Type: "ok_video"}}},
			DirName: "p2_dir",
			InState: true,
		},
	}

	cfg := Config{Blog: blog, OutputDir: outDir, Workers: 2}
	e := New(f.client, cfg)
	e.runCheckMedia(blogDir, items)

	if items[0].VideoMismatch != "" {
		t.Errorf("p1 (matching size) flagged: %q", items[0].VideoMismatch)
	}
	if items[1].VideoMismatch == "" {
		t.Error("p2 (size mismatch) was not flagged")
	}
}

// --- helpers ---

func requireFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// writeInitialState seeds _state.json with the given entries so state.Load
// in the engine sees a pre-existing prior run.
func writeInitialState(t *testing.T, blogDir string, entries map[string]state.PostEntry) {
	t.Helper()
	mustMkdir(t, blogDir)
	payload := struct {
		Posts map[string]state.PostEntry `json:"posts"`
	}{Posts: entries}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blogDir, state.FileName), data, 0644); err != nil {
		t.Fatal(err)
	}
}
