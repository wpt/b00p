package syncer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
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

// post.json and comments.json must hold the API bytes, not a re-marshal of
// the boosty structs: fields and block shapes the structs do not declare
// (list items, smile name/URLs, link explicit, a block type nobody knows
// yet) are otherwise gone for good once the post is deleted. The readers
// that decode those files back must still work on the raw shape. Covers both
// decode paths: the list endpoint (FetchPosts) and the per-post endpoint
// (GetJSON, as --url uses it).
func TestEngine_ArchivesRawPostAndComments(t *testing.T) {
	const rawPost = `{"id":"p1","title":"Raw","hasAccess":true,"publishTime":1700000000,"updatedAt":1700000100,
		"count":{"comments":2,"reactions":{"heart":3}},"isLiked":false,
		"data":[
			{"type":"list","style":"ordered","items":[{"data":[{"type":"text","content":"[\"one\",\"unstyled\",[]]","modificator":"BLOCK_END"}],"items":[]}]},
			{"type":"smile","id":"s1","name":"cat","smallUrl":"https://s/c-s.png","mediumUrl":"https://s/c-m.png","largeUrl":"https://s/c-l.png","isAnimated":false},
			{"type":"link","url":"https://e.com/<x>&y","content":"[\"l\",\"unstyled\",[]]","explicit":true},
			{"type":"poll","id":"q1","options":[{"text":"да"}]}
		]}`
	const rawComment = `{"id":"c1","intId":1,"author":{"id":5,"name":"A"},"createdAt":1,"replyCount":1,
		"data":[{"type":"smile","id":"s2","name":"dog","largeUrl":"https://s/d.png"}],"reactions":{"like":1},
		"replies":{"data":[{"id":"r1","data":[],"isDeleted":false,"replyCount":0,"isBlocked":false}],"extra":{"isLast":true}}}`

	for _, path := range []string{"list", "single"} {
		t.Run(path, func(t *testing.T) {
			blog := "myblog"
			f := newFakeAPI(t)
			f.HandleFunc("GET", "/v1/blog/"+blog+"/post/", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"data":[%s],"extra":{"isLast":true}}`, rawPost)
			})
			f.HandleFunc("GET", "/v1/blog/"+blog+"/post/p1", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, rawPost)
			})
			f.HandleFunc("GET", "/v1/blog/"+blog+"/post/p1/comment/", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"data":[%s],"extra":{"isLast":true}}`, rawComment)
			})
			cfg := Config{Blog: blog, OutputDir: t.TempDir(), WithComments: true}
			e := New(f.client, cfg)

			var dirName string
			if path == "list" {
				if err := e.DownloadAll(); err != nil {
					t.Fatalf("DownloadAll: %v", err)
				}
				st, err := state.Load(filepath.Join(cfg.OutputDir, blog))
				if err != nil {
					t.Fatalf("load state: %v", err)
				}
				dirName = st.Posts["p1"].DirName
			} else {
				var post boosty.Post
				if err := f.client.GetJSON(boosty.PostURL(blog, "p1"), &post); err != nil {
					t.Fatalf("GetJSON: %v", err)
				}
				var err error
				if dirName, _, err = e.SavePost(&post); err != nil {
					t.Fatalf("SavePost: %v", err)
				}
			}
			blogDir := filepath.Join(cfg.OutputDir, blog)
			dir := filepath.Join(blogDir, dirName)

			if got, want := compactFile(t, filepath.Join(dir, "post.json")), compactJSON(t, rawPost); got != want {
				t.Errorf("post.json lost API content\n got %s\nwant %s", got, want)
			}
			if got, want := compactFile(t, filepath.Join(dir, "comments.json")), compactJSON(t, "["+rawComment+"]"); got != want {
				t.Errorf("comments.json lost API content\n got %s\nwant %s", got, want)
			}

			if got, ok := readPostUpdatedAt(dir); !ok || got != 1700000100 {
				t.Errorf("readPostUpdatedAt = %d, %v; want 1700000100, true", got, ok)
			}
			if !newDirReserver().diskOwnedBy(blogDir, dirName, "p1") || newDirReserver().diskOwnedBy(blogDir, dirName, "p2") {
				t.Errorf("diskOwnedBy does not read the post id from the raw post.json")
			}
			if got, ok := diskCommentCount(dir); !ok || got != 2 {
				t.Errorf("diskCommentCount = %d, %v; want 2, true", got, ok)
			}
		})
	}
}

func compactJSON(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		t.Fatalf("compact %s: %v", s, err)
	}
	return b.String()
}

func compactFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return compactJSON(t, string(data))
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
	e.applyItem(st, item)

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
	e.applyItem(st, item)

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
	e.applyItem(st, item)

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
			e.applyItem(st, item)

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
		DirName:      "p1_dir", // ignored: pickDirName decides, and the surviving dir must win
		Existing:     st.Posts["p1"],
		InState:      true,
		JustUnlocked: true,
	}
	e.applyItem(st, item)

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
	e.applyItem(st, item)

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
	e.applyItem(st, item)

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

// A NEW post whose formatted name collides with a tracked post's directory
// must be suffixed even when that directory's post.json is missing — the
// disk probe alone reports it as free, and the NEW post would adopt the
// tracked post's media as its own. loadState seeds the reserver from state.
func TestEngine_Sync_NewPostDoesNotClaimTrackedDir(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)

	const publish = int64(1700000000)
	shared := parser.FormatDirName(parser.DefaultFormat, "", publish, "aaaaaaaa-old")
	mustMkdir(t, filepath.Join(blogDir, shared))
	writeFile(t, filepath.Join(blogDir, shared, "image_001.jpg"), "A-image")
	// No post.json: the FILES_MISSING repair window.
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"aaaaaaaa-old": {Title: "", DirName: shared, UpdatedAt: 100},
	})

	f.Media("b.jpg", []byte("B-image"))
	old := boosty.Post{ID: "aaaaaaaa-old", HasAccess: true, PublishTime: publish, UpdatedAt: 100}
	fresh := boosty.Post{
		ID: "bbbbbbbb-new", HasAccess: true, PublishTime: publish, UpdatedAt: 300,
		Data: []boosty.ContentBlock{{Type: "image", URL: f.MediaURL("b.jpg")}},
	}
	f.PostsList(blog, fresh, old) // newest first, like the API

	e := New(f.client, Config{Blog: blog, OutputDir: outDir, AutoApply: true})
	if err := e.Sync(); err != nil {
		t.Fatalf("Sync: %v\n%s", err, f.log.joined())
	}

	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	got := st.Posts["bbbbbbbb-new"].DirName
	if got == shared || got == "" {
		t.Fatalf("NEW post DirName = %q, must be suffixed away from the tracked %q\n%s", got, shared, f.log.joined())
	}
	if data, _ := os.ReadFile(filepath.Join(blogDir, got, "image_001.jpg")); string(data) != "B-image" {
		t.Errorf("NEW post image = %q, want its own bytes", data)
	}
	if data, _ := os.ReadFile(filepath.Join(blogDir, shared, "image_001.jpg")); string(data) != "A-image" {
		t.Errorf("tracked post image = %q, want untouched", data)
	}
	if _, err := os.Stat(filepath.Join(blogDir, shared, "post.json")); err == nil {
		t.Error("NEW post wrote post.json into the tracked post's directory")
	}
}

// A post marked Locked by a sync is still in state; the default download
// mode must re-download it once it is accessible again instead of counting
// it as already synced forever.
func TestEngine_DownloadAll_RedownloadsLockedEntry(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	mustMkdir(t, postDir)
	writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1","title":"stale","updatedAt":100}`)
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "old", DirName: "p1_dir", UpdatedAt: 100, Locked: true},
	})

	fresh := boosty.Post{
		ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 200, PublishTime: 1700000000,
		Data: []boosty.ContentBlock{{Type: "text", Content: `["body","unstyled",[]]`}},
	}
	f.PostsList(blog, fresh)

	e := New(f.client, Config{Blog: blog, OutputDir: outDir})
	if err := e.DownloadAll(); err != nil {
		t.Fatalf("DownloadAll: %v\n%s", err, f.log.joined())
	}

	st, _ := state.Load(blogDir)
	got := st.Posts["p1"]
	if got.Locked {
		t.Error("Locked = true, want false after re-download")
	}
	if got.UpdatedAt != 200 {
		t.Errorf("UpdatedAt = %d, want 200", got.UpdatedAt)
	}
	if got.DirName != "p1_dir" {
		t.Errorf("DirName = %q, want the surviving 'p1_dir'", got.DirName)
	}
	pj, _ := os.ReadFile(filepath.Join(postDir, "post.json"))
	if !strings.Contains(string(pj), `"title": "new"`) {
		t.Errorf("post.json not refreshed: %s", pj)
	}
}

// --force without --md/--comments must keep tracking artefacts that are on
// disk and regenerate them against the fresh post, not reset the flags.
func TestEngine_DownloadAll_ForcePreservesTrackedArtefacts(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	mustMkdir(t, postDir)
	writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1","title":"old","updatedAt":100}`)
	writeFile(t, filepath.Join(postDir, "post.md"), "# old")
	writeFile(t, filepath.Join(postDir, "comments.json"), "[]")
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "old", DirName: "p1_dir", UpdatedAt: 100, HasMd: true, HasComments: true, CommentsCapped: true},
	})

	post := boosty.Post{
		ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 100, PublishTime: 1700000000,
		Count: boosty.PostCount{Comments: 1},
		Data:  []boosty.ContentBlock{{Type: "text", Content: `["body","unstyled",[]]`}},
	}
	f.PostsList(blog, post)
	f.CommentsList(blog, "p1", boosty.Comment{ID: "c1"})

	e := New(f.client, Config{Blog: blog, OutputDir: outDir, Force: true})
	if err := e.DownloadAll(); err != nil {
		t.Fatalf("DownloadAll: %v\n%s", err, f.log.joined())
	}

	st, _ := state.Load(blogDir)
	got := st.Posts["p1"]
	if !got.HasMd || !got.HasComments {
		t.Errorf("HasMd=%v HasComments=%v, want both preserved under --force", got.HasMd, got.HasComments)
	}
	if got.CommentsCapped {
		t.Error("CommentsCapped = true, want false (comments were refetched and fit)")
	}
	md, _ := os.ReadFile(filepath.Join(postDir, "post.md"))
	if !strings.Contains(string(md), "# new") {
		t.Errorf("post.md not regenerated: %q", md)
	}
	cj, _ := os.ReadFile(filepath.Join(postDir, "comments.json"))
	if !strings.Contains(string(cj), `"c1"`) {
		t.Errorf("comments.json not refetched: %q", cj)
	}
}

// SavePost keeps existing media only while the on-disk post.json carries the
// same updatedAt. An edit may have swapped media at the same slot, so a
// changed updatedAt re-downloads every item over its stale copy.
func TestEngine_SavePost_EditedSinceLastSaveReplacesMedia(t *testing.T) {
	f := newFakeAPI(t)
	f.Media("img.jpg", []byte("FRESH"))
	cfg := Config{Blog: "myblog", OutputDir: t.TempDir()}
	e := New(f.client, cfg)

	post := boosty.Post{
		ID: "p1", Title: "Hello", HasAccess: true, PublishTime: 1700000000, UpdatedAt: 200,
		Data: []boosty.ContentBlock{{Type: "image", URL: f.MediaURL("img.jpg")}},
	}
	dirName := parser.FormatDirName(parser.DefaultFormat, post.Title, post.PublishTime, post.ID)
	postDir := filepath.Join(cfg.OutputDir, cfg.Blog, dirName)
	mustMkdir(t, postDir)
	writeFile(t, filepath.Join(postDir, "image_001.jpg"), "STALE")

	// Same updatedAt on disk: the existing image is trusted and kept.
	writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1","updatedAt":200}`)
	if _, _, err := e.SavePost(&post); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(postDir, "image_001.jpg")); string(data) != "STALE" {
		t.Errorf("image = %q, want the existing copy kept (post not edited)", data)
	}

	// Older updatedAt on disk: the post was edited since; media is replaced.
	writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1","updatedAt":100}`)
	if _, _, err := e.SavePost(&post); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(postDir, "image_001.jpg")); string(data) != "FRESH" {
		t.Errorf("image = %q, want re-downloaded (post edited since last save)", data)
	}
}

// A VIDEO_MISMATCH re-download that fails must leave the previous file in
// place: with the old delete-then-download order the archive lost every
// video of the post and no trigger short of another --check-media noticed.
func TestEngine_ApplyItem_VideoMismatchFailedRedownloadKeepsOldFile(t *testing.T) {
	saved := boosty.RetryDelays
	boosty.RetryDelays = []time.Duration{time.Millisecond}
	defer func() { boosty.RetryDelays = saved }()

	blog := "myblog"
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	mustMkdir(t, postDir)
	writeFile(t, filepath.Join(postDir, "video_001.mp4"), "old-video-bytes")
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "old", DirName: "p1_dir", UpdatedAt: 100},
	})

	// No Media handler for v.mp4 → the download 404s (non-retriable).
	post := boosty.Post{
		ID: "p1", Title: "old", HasAccess: true, UpdatedAt: 100,
		Data: []boosty.ContentBlock{{
			Type:       "ok_video",
			PlayerURLs: []boosty.PlayerURL{{Type: "full_hd", URL: f.MediaURL("v.mp4")}},
		}},
	}
	f.SinglePost(blog, "p1", post)

	e := New(f.client, Config{Blog: blog, OutputDir: outDir})
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	e.applyItem(st, syncItem{
		Post: post, DirName: "p1_dir", Existing: st.Posts["p1"], InState: true,
		VideoMismatch: "video_001.mp4: local 15 B vs remote 99 B",
	})

	if data, _ := os.ReadFile(filepath.Join(postDir, "video_001.mp4")); string(data) != "old-video-bytes" {
		t.Errorf("video = %q, want the old bytes kept after a failed re-download", data)
	}
	if n := e.failedPosts.Load(); n != 1 {
		t.Errorf("failedPosts = %d, want 1", n)
	}
	reloaded, _ := state.Load(blogDir)
	if got := reloaded.Posts["p1"].UpdatedAt; got != 100 {
		t.Errorf("UpdatedAt = %d, want 100 (unchanged)", got)
	}
}

// --- downloadComments: cap detection at the structural boundary ---
//
// commentsPageLimit=101 exists so a post with EXACTLY 100 top-level threads
// (uncapped) is distinguishable from one that hit the cap. A flipped
// comparison here silently sets commentsCapped on boundary posts and
// permanently suppresses their comment refetch; the per-thread signal
// (fewer live replies inlined than the live replyCount claims exist) is the
// only way reply truncation is ever detected.
//
// The API count is the fetched live count plus apiDelta, so with apiDelta 0
// only the structural signals can set the flag. A non-zero apiDelta is a
// wrong count.comments: flagged, but never reported as a cap.
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
		apiDelta int
		want     bool
	}{
		{"exactly_100_uncapped", mk(100), 0, false},
		{"101_capped", mk(101), 0, true},
		{"api_count_below_live", []boosty.Comment{
			{ID: "c1", ReplyCount: 2, Replies: &boosty.CommentsResponse{
				Data: []boosty.Comment{{ID: "r1"}, {ID: "r2"}},
			}},
		}, -2, true},
		{"api_count_above_live", mk(3), 1, true},
		{"negative_api_count", nil, -3, true},
		{"reply_undercount_capped", []boosty.Comment{
			{ID: "c1", ReplyCount: 5, Replies: &boosty.CommentsResponse{
				Data: []boosty.Comment{{ID: "r1"}},
			}},
		}, 0, true},
		{"replies_fully_inlined_uncapped", []boosty.Comment{
			{ID: "c1", ReplyCount: 1, Replies: &boosty.CommentsResponse{
				Data: []boosty.Comment{{ID: "r1"}},
			}},
		}, 0, false},
		// ReplyCount counts live replies only, so the per-thread signal must
		// compare live-to-live: 2 items inlined but one is a deleted stub →
		// only 1 of 2 live replies actually stored → capped. Raw
		// len(Data) == ReplyCount here, which would mask the truncation.
		{"deleted_stub_padding_doesnt_mask_truncation", []boosty.Comment{
			{ID: "c1", ReplyCount: 2, Replies: &boosty.CommentsResponse{
				Data: []boosty.Comment{{ID: "r1"}, {ID: "r2", IsDeleted: true}},
			}},
		}, 0, true},
		// The reverse padding shape: all live replies present plus a stub on
		// top (len(Data) > ReplyCount, the common deleted-comment case) is
		// fully stored — not capped.
		{"deleted_stub_extra_uncapped", []boosty.Comment{
			{ID: "c1", ReplyCount: 1, Replies: &boosty.CommentsResponse{
				Data: []boosty.Comment{{ID: "r1"}, {ID: "r2", IsDeleted: true}},
			}},
		}, 0, false},
		// A thread claiming replies but carrying no replies object at all:
		// nothing was inlined, so its live replies are unreachable — capped
		// (fail-closed), not an endless COMMENTS refetch loop that never
		// recovers them.
		{"nil_replies_with_replycount_capped", []boosty.Comment{
			{ID: "c1", ReplyCount: 5},
		}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blog := "myblog"
			f := newFakeAPI(t)
			f.CommentsList(blog, "p1", tc.comments...)
			e := New(f.client, Config{Blog: blog, OutputDir: t.TempDir()})

			dir := t.TempDir()
			live, _ := liveCommentCount(tc.comments)
			capped, err := e.downloadComments("p1", dir, live+tc.apiDelta)
			if err != nil {
				t.Fatalf("downloadComments: %v", err)
			}
			if capped != tc.want {
				t.Errorf("capped = %v, want %v", capped, tc.want)
			}
			if tc.apiDelta != 0 && strings.Contains(f.log.joined(), "comments capped") {
				t.Errorf("a wrong API count was reported as a cap:\n%s", f.log.joined())
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

// A state entry with an empty DirName used to aim the whole apply phase at the
// blog root (filepath.Join(blogDir, "") == blogDir), writing post artefacts next
// to _state.json while Sync still exited 0. The post must be re-downloaded into
// a real directory and the bad entry replaced.
func TestEngine_Sync_EmptyDirNameEntryDoesNotWriteIntoBlogRoot(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	out := t.TempDir()
	blogDir := filepath.Join(out, blog)
	if err := os.MkdirAll(blogDir, 0755); err != nil {
		t.Fatal(err)
	}

	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	st.Add("p1", state.PostEntry{Title: "Poisoned", DirName: "", UpdatedAt: 100, HasMd: true})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	post := boosty.Post{
		ID: "p1", Title: "Poisoned", HasAccess: true,
		PublishTime: 1700000000, UpdatedAt: 200, // updatedAt drift → actionable
		Data: []boosty.ContentBlock{{Type: "text", Content: `["body","unstyled",[]]`}},
	}
	f.PostsList(blog, post)
	f.SinglePost(blog, "p1", post)

	cfg := Config{Blog: blog, OutputDir: out, AutoApply: true, WithMD: true, Workers: 1}
	if err := New(f.client, cfg).Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	for _, name := range []string{"post.json", "post.md", "comments.json"} {
		if _, err := os.Stat(filepath.Join(blogDir, name)); err == nil {
			t.Errorf("%s written into the blog root; log:\n%s", name, f.log.joined())
		}
	}

	reloaded, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reloaded.Get("p1")
	if !ok {
		t.Fatalf("p1 missing from state after Sync; log:\n%s", f.log.joined())
	}
	if entry.DirName == "" {
		t.Fatalf("state still carries an empty DirName; log:\n%s", f.log.joined())
	}
	requireFile(t, filepath.Join(blogDir, entry.DirName, "post.json"))
	requireFile(t, filepath.Join(blogDir, entry.DirName, "post.md"))
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

// Boosty's count.comments is sometimes wrong — seen live: 1 for a thread with
// two replies, -1 and -3 for posts with none — so the disk count can never
// match it and a disk-vs-API trigger refetched such posts on every sync. One
// refetch must settle the post until the API count moves; a failed refetch,
// or a missing comments.json, must still fire. A short comments.json under a
// correct count is the opposite case: it refetches and keeps the disk
// comparison.
func TestEngine_Sync_WrongCommentCountGoesQuiet(t *testing.T) {
	thread := func(id string, replies ...string) boosty.Comment {
		c := boosty.Comment{ID: id, ReplyCount: len(replies), Replies: &boosty.CommentsResponse{}}
		for _, r := range replies {
			c.Replies.Data = append(c.Replies.Data, boosty.Comment{ID: r})
		}
		return c
	}
	cases := []struct {
		name       string
		apiCount   int
		onDisk     string           // comments.json from the previous run
		live       []boosty.Comment // what the comments endpoint returns
		wantCapped bool
	}{
		{"api_count_below_live", 1,
			`[{"id":"c1","replyCount":2,"replies":{"data":[{"id":"r1"},{"id":"r2"}]}}]`,
			[]boosty.Comment{thread("c1", "r1", "r2")}, true},
		{"negative_api_count", -3, `[]`, nil, true},
		{"short_disk_correct_count", 3,
			`[{"id":"c1","replyCount":2,"replies":{"data":[]}}]`,
			[]boosty.Comment{thread("c1", "r1", "r2")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blog := "myblog"
			f := newFakeAPI(t)
			outDir := t.TempDir()
			blogDir := filepath.Join(outDir, blog)
			postDir := filepath.Join(blogDir, "p1_dir")
			mustMkdir(t, postDir)
			writeFile(t, filepath.Join(postDir, "comments.json"), tc.onDisk)
			writeInitialState(t, blogDir, map[string]state.PostEntry{
				"p1": {Title: "p1", DirName: "p1_dir", UpdatedAt: 100, HasComments: true, CommentsCount: tc.apiCount},
			})

			var fetches atomic.Int32
			serve := func(apiCount int, live []boosty.Comment, fail bool) {
				f.PostsList(blog, boosty.Post{
					ID: "p1", Title: "p1", HasAccess: true, UpdatedAt: 100,
					Count: boosty.PostCount{Comments: apiCount},
				})
				body := boosty.CommentsResponse{Data: live, Extra: boosty.PaginationExtra{IsLast: true, IsFirst: true}}
				f.HandleFunc("GET", "/v1/blog/"+blog+"/post/p1/comment/", func(w http.ResponseWriter, r *http.Request) {
					fetches.Add(1)
					if fail {
						http.NotFound(w, r) // deterministic 4xx: fails fast, no retry schedule
						return
					}
					_ = json.NewEncoder(w).Encode(body)
				})
			}
			run := func(step string, wantFetches int32, wantErr bool) {
				t.Helper()
				fetchesBefore := fetches.Load()
				quietBefore := strings.Count(f.log.joined(), "Everything up to date.")
				err := New(f.client, Config{Blog: blog, OutputDir: outDir, AutoApply: true}).Sync()
				if (err != nil) != wantErr {
					t.Fatalf("%s: Sync error = %v, want error: %v\n%s", step, err, wantErr, f.log.joined())
				}
				if got := fetches.Load() - fetchesBefore; got != wantFetches {
					t.Fatalf("%s: comments fetched %d time(s), want %d\n%s", step, got, wantFetches, f.log.joined())
				}
				quiet := strings.Count(f.log.joined(), "Everything up to date.") > quietBefore
				if quiet != (wantFetches == 0) {
					t.Fatalf("%s: up to date = %v, want %v\n%s", step, quiet, wantFetches == 0, f.log.joined())
				}
			}

			serve(tc.apiCount, tc.live, false)
			run("first sync", 1, false)
			st, err := state.Load(blogDir)
			if err != nil {
				t.Fatal(err)
			}
			if got := st.Posts["p1"]; got.CommentsCapped != tc.wantCapped || got.CommentsCount != tc.apiCount {
				t.Fatalf("entry after first sync: CommentsCapped=%v CommentsCount=%d, want %v and %d",
					got.CommentsCapped, got.CommentsCount, tc.wantCapped, tc.apiCount)
			}
			run("second sync", 0, false)

			// A new comment moves the API count, wrong or not.
			moved := append(tc.live, boosty.Comment{ID: "c9"})
			serve(tc.apiCount+1, moved, true)
			run("API count moved, fetch fails", 1, true)
			serve(tc.apiCount+1, moved, false)
			run("retry after the failed fetch", 1, false)
			run("after the retry", 0, false)

			if err := os.Remove(filepath.Join(postDir, "comments.json")); err != nil {
				t.Fatal(err)
			}
			run("comments.json removed", 1, false)
			run("after the restore", 0, false)
		})
	}
}

// Damaged media used to be invisible to every sync mode: a deleted post
// folder came back as post.json and post.md only (or comments.json alone),
// and the next sync reported everything up to date. Each kind of damage must
// be restored by one sync, with or without --check-files, fetching only the
// missing media, and the next sync must go quiet.
func TestEngine_Sync_RestoresMissingFiles(t *testing.T) {
	cases := []struct {
		name      string
		damage    func(t *testing.T, postDir string)
		refetched []string // media fetched again by the repair
	}{
		{"post dir deleted", func(t *testing.T, postDir string) {
			if err := os.RemoveAll(postDir); err != nil {
				t.Fatal(err)
			}
		}, []string{"a.jpg", "b.jpg", "v.mp4"}},
		{"image deleted", func(t *testing.T, postDir string) {
			if err := os.Remove(filepath.Join(postDir, "image_001.jpg")); err != nil {
				t.Fatal(err)
			}
		}, []string{"a.jpg"}},
		{"image emptied", func(t *testing.T, postDir string) {
			writeFile(t, filepath.Join(postDir, "image_001.jpg"), "")
		}, []string{"a.jpg"}},
	}
	for _, tc := range cases {
		for _, checkFiles := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/check-files=%v", tc.name, checkFiles), func(t *testing.T) {
				blog := "myblog"
				f := newFakeAPI(t)
				media := map[string]string{"a.jpg": "IMAGE-A", "b.jpg": "IMAGE-B", "v.mp4": "VIDEO"}
				gets := map[string]*atomic.Int32{}
				for name, body := range media {
					n := new(atomic.Int32)
					gets[name] = n
					f.HandleFunc("GET", "/_media/"+name, func(w http.ResponseWriter, r *http.Request) {
						n.Add(1)
						fmt.Fprint(w, body)
					})
				}
				post := boosty.Post{
					ID: "p1", Title: "Hello", HasAccess: true, PublishTime: 1700000000, UpdatedAt: 100,
					Count: boosty.PostCount{Comments: 1},
					Data: []boosty.ContentBlock{
						{Type: "text", Content: `["body","unstyled",[]]`},
						{Type: "image", URL: f.MediaURL("a.jpg")},
						{Type: "image", URL: f.MediaURL("b.jpg")},
						{Type: "ok_video", PlayerURLs: []boosty.PlayerURL{{Type: "high", URL: f.MediaURL("v.mp4")}}},
					},
				}
				f.PostsList(blog, post)
				var postFetches atomic.Int32
				f.HandleFunc("GET", "/v1/blog/"+blog+"/post/p1", func(w http.ResponseWriter, r *http.Request) {
					postFetches.Add(1)
					_ = json.NewEncoder(w).Encode(post)
				})
				f.CommentsList(blog, "p1", boosty.Comment{ID: "c1"})

				outDir := t.TempDir()
				blogDir := filepath.Join(outDir, blog)
				if err := New(f.client, Config{Blog: blog, OutputDir: outDir, WithMD: true, WithComments: true}).DownloadAll(); err != nil {
					t.Fatalf("DownloadAll: %v\n%s", err, f.log.joined())
				}
				st, err := state.Load(blogDir)
				if err != nil {
					t.Fatal(err)
				}
				postDir := filepath.Join(blogDir, st.Posts["p1"].DirName)
				files := map[string]string{"image_001.jpg": "IMAGE-A", "image_002.jpg": "IMAGE-B", "video_001.mp4": "VIDEO"}

				// The disk checks must stay local: a quiet sync fetches no post.
				runSync := func(step string, checkFiles, wantQuiet bool) {
					t.Helper()
					quietBefore := strings.Count(f.log.joined(), "Everything up to date.")
					fetchesBefore := postFetches.Load()
					cfg := Config{Blog: blog, OutputDir: outDir, AutoApply: true, CheckFiles: checkFiles}
					if err := New(f.client, cfg).Sync(); err != nil {
						t.Fatalf("%s: Sync: %v\n%s", step, err, f.log.joined())
					}
					if quiet := strings.Count(f.log.joined(), "Everything up to date.") > quietBefore; quiet != wantQuiet {
						t.Fatalf("%s: up to date = %v, want %v\n%s", step, quiet, wantQuiet, f.log.joined())
					}
					if n := postFetches.Load() - fetchesBefore; wantQuiet && n != 0 {
						t.Fatalf("%s: quiet sync fetched the post %d time(s)", step, n)
					}
				}
				runSync("before the damage", checkFiles, true)

				tc.damage(t, postDir)
				getsBefore := map[string]int32{}
				for name, n := range gets {
					getsBefore[name] = n.Load()
				}
				runSync("repair", checkFiles, false)

				for name, want := range files {
					if data, err := os.ReadFile(filepath.Join(postDir, name)); string(data) != want {
						t.Errorf("%s = %q (%v), want %q\n%s", name, data, err, want, f.log.joined())
					}
				}
				for _, name := range []string{"post.json", "post.md", "comments.json"} {
					requireFile(t, filepath.Join(postDir, name))
				}
				for name, n := range gets {
					want := getsBefore[name]
					if slices.Contains(tc.refetched, name) {
						want++
					}
					if got := n.Load(); got != want {
						t.Errorf("%s fetched %d time(s), want %d (only missing media is downloaded)", name, got, want)
					}
				}
				runSync("after the repair", checkFiles, true)
			})
		}
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

// After an edit, the first re-download writes the new post.json before the
// media replacement fails; the retry must still replace the media. The edit
// verdict has to come from state (UpdatedAt advances only when every
// channel landed), not from the post.json now on disk, or the stale bytes
// are kept and state advances past them.
func TestEngine_SaveNewPost_FailedMediaReplaceStillReplacesOnRetry(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	var calls atomic.Int32
	f.HandleFunc("GET", "/_media/img.jpg", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.NotFound(w, r) // deterministic 4xx: fails fast, no retry schedule
			return
		}
		fmt.Fprint(w, "FRESH")
	})
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	mustMkdir(t, postDir)
	writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1","updatedAt":100}`)
	writeFile(t, filepath.Join(postDir, "image_001.jpg"), "STALE")
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "old", DirName: "p1_dir", UpdatedAt: 100},
	})

	post := boosty.Post{
		ID: "p1", Title: "edited", HasAccess: true, UpdatedAt: 200, PublishTime: 1700000000,
		Data: []boosty.ContentBlock{{Type: "image", URL: f.MediaURL("img.jpg")}},
	}
	e := New(f.client, Config{Blog: blog, OutputDir: outDir, Force: true})
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}

	if e.saveNewPost(st, &post) {
		t.Fatal("first saveNewPost reported success, want the media failure surfaced")
	}
	if data, _ := os.ReadFile(filepath.Join(postDir, "image_001.jpg")); string(data) != "STALE" {
		t.Fatalf("image after failed replace = %q, want the old bytes kept", data)
	}
	if got := st.Posts["p1"].UpdatedAt; got != 100 {
		t.Fatalf("UpdatedAt after failure = %d, want 100 (not advanced)", got)
	}
	// post.json on disk now already says 200 — the trap for a disk-only check.
	if prev, _ := readPostUpdatedAt(postDir); prev != 200 {
		t.Fatalf("post.json updatedAt = %d, want 200 (written before media)", prev)
	}

	e.failedPosts.Store(0)
	if !e.saveNewPost(st, &post) {
		t.Fatalf("second saveNewPost failed\n%s", f.log.joined())
	}
	if data, _ := os.ReadFile(filepath.Join(postDir, "image_001.jpg")); string(data) != "FRESH" {
		t.Errorf("image after retry = %q, want replaced (post edited since state's UpdatedAt)", data)
	}
	if got := st.Posts["p1"].UpdatedAt; got != 200 {
		t.Errorf("UpdatedAt after retry = %d, want 200", got)
	}
}
