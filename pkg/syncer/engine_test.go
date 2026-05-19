package syncer

import (
	"encoding/json"
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

	dirName, err := e.SavePost(&post)
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

	dirName, err := e.SavePost(&post)
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
