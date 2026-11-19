package syncer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/state"
)

// countPostFetches serves post at the per-post endpoint and counts the GETs.
func countPostFetches(f *fakeAPI, blog string, post boosty.Post) *atomic.Int32 {
	var n atomic.Int32
	f.HandleFunc("GET", "/v1/blog/"+blog+"/post/"+post.ID, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_ = json.NewEncoder(w).Encode(post)
	})
	return &n
}

func mustLoadState(t *testing.T, blogDir string) *state.State {
	t.Helper()
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return st
}

// A listing that cannot be fetched must fail the run, or cron sees exit 0
// on a run that archived nothing.
func TestEngine_ListingFailureFailsRun(t *testing.T) {
	for name, run := range map[string]func(*Engine) error{
		"Sync":        (*Engine).Sync,
		"DownloadAll": (*Engine).DownloadAll,
	} {
		t.Run(name, func(t *testing.T) {
			blog := "myblog"
			f := newFakeAPI(t)
			f.HandleFunc("GET", "/v1/blog/"+blog+"/post/", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			})
			err := run(New(f.client, Config{Blog: blog, OutputDir: t.TempDir(), AutoApply: true}))
			if !errors.Is(err, boosty.ErrFetchPage) {
				t.Errorf("err = %v, want ErrFetchPage", err)
			}
		})
	}
}

// A post whose media fails to download must fail the run.
func TestEngine_FailedMediaFailsRun(t *testing.T) {
	for name, run := range map[string]func(*Engine) error{
		"Sync":        (*Engine).Sync,
		"DownloadAll": (*Engine).DownloadAll,
	} {
		t.Run(name, func(t *testing.T) {
			blog := "myblog"
			f := newFakeAPI(t)
			f.PostsList(blog, boosty.Post{
				ID: "p1", Title: "t", HasAccess: true, PublishTime: 1700000000, UpdatedAt: 100,
				Data: []boosty.ContentBlock{imageBlock(f, "img-1", "gone.jpg")}, // unregistered: 404
			})
			if err := run(New(f.client, Config{Blog: blog, OutputDir: t.TempDir(), AutoApply: true})); err == nil {
				t.Errorf("run returned nil with a failed media download\n%s", f.log.joined())
			}
		})
	}
}

// A tracked post that loses access keeps its archive and is marked Locked.
func TestEngine_Sync_LostAccessMarksLockedKeepsArchive(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	mustMkdir(t, postDir)
	const archived = `{"id":"p1","title":"t","updatedAt":100,"data":[{"type":"text","content":"[\"body\",\"unstyled\",[]]"}]}`
	writeFile(t, filepath.Join(postDir, "post.json"), archived)
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "t", DirName: "p1_dir", UpdatedAt: 100, Archive: archiveFormat},
	})
	f.PostsList(blog, boosty.Post{ID: "p1", Title: "t", HasAccess: false, UpdatedAt: 100})

	if err := New(f.client, Config{Blog: blog, OutputDir: outDir, AutoApply: true}).Sync(); err != nil {
		t.Fatalf("Sync: %v\n%s", err, f.log.joined())
	}
	if !mustLoadState(t, blogDir).Posts["p1"].Locked {
		t.Error("Locked = false, want true after losing access")
	}
	if got := readFileString(t, filepath.Join(postDir, "post.json")); got != archived {
		t.Errorf("post.json changed:\n%s", got)
	}
}

// --check-files repairs a missing post.md of an accessible post and leaves a
// still-locked one alone: its per-post fetch returns a stub, so queuing it
// would fail every run.
func TestEngine_Sync_CheckFilesRepairsAndSkipsLocked(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)

	open := boosty.Post{
		ID: "p1", Title: "Open", HasAccess: true, PublishTime: 1700000000, UpdatedAt: 100,
		Data: []boosty.ContentBlock{textBlock("body")},
	}
	mustMkdir(t, filepath.Join(blogDir, "p1_dir"))
	writePostJSON(t, filepath.Join(blogDir, "p1_dir"), open)
	// p2_dir has no post.json: only the access guard keeps it out of apply.
	mustMkdir(t, filepath.Join(blogDir, "p2_dir"))
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "Open", DirName: "p1_dir", UpdatedAt: 100, HasMd: true, Archive: archiveFormat},
		"p2": {Title: "Locked", DirName: "p2_dir", UpdatedAt: 100, Locked: true, Archive: archiveFormat},
	})
	locked := boosty.Post{ID: "p2", Title: "Locked", HasAccess: false, UpdatedAt: 100}
	f.PostsList(blog, open, locked)
	lockedFetches := countPostFetches(f, blog, locked)

	cfg := Config{Blog: blog, OutputDir: outDir, AutoApply: true, CheckFiles: true}
	if err := New(f.client, cfg).Sync(); err != nil {
		t.Fatalf("Sync: %v\n%s", err, f.log.joined())
	}
	if md := readFileString(t, filepath.Join(blogDir, "p1_dir", "post.md")); !strings.Contains(md, "Open") {
		t.Errorf("post.md not restored: %q", md)
	}
	if n := lockedFetches.Load(); n != 0 {
		t.Errorf("locked post fetched %d time(s), want 0", n)
	}
	if !mustLoadState(t, blogDir).Posts["p2"].Locked {
		t.Error("p2 Locked = false, want true")
	}
}

// Edited, COMMENTS and LOCKED items applied by four workers. The point is
// CI's -race job over the stMu sections; the final state checks that no
// worker's commit was lost.
func TestEngine_Sync_ParallelApplyMixedItems(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)

	entries := map[string]state.PostEntry{}
	var listing []boosty.Post
	for i := range 9 {
		id := fmt.Sprintf("p%d", i)
		dir := id + "_dir"
		mustMkdir(t, filepath.Join(blogDir, dir))
		entry := state.PostEntry{Title: "old", DirName: dir, UpdatedAt: 100, Archive: archiveFormat}
		post := boosty.Post{ID: id, Title: "old", HasAccess: true, PublishTime: 1700000000, UpdatedAt: 100,
			Data: []boosty.ContentBlock{textBlock("body")}}
		switch i % 3 {
		case 0: // edited
			writePostJSON(t, filepath.Join(blogDir, dir), post)
			post.Title, post.UpdatedAt = "new", 200
			f.SinglePost(blog, id, post)
		case 1: // new comment
			writePostJSON(t, filepath.Join(blogDir, dir), post)
			writeFile(t, filepath.Join(blogDir, dir, "comments.json"), "[]")
			entry.HasComments = true
			post.Count.Comments = 1
			f.CommentsList(blog, id, boosty.Comment{ID: "c-" + id})
		case 2: // lost access
			post = boosty.Post{ID: id, Title: "old", HasAccess: false, UpdatedAt: 100}
		}
		entries[id] = entry
		listing = append(listing, post)
	}
	writeInitialState(t, blogDir, entries)
	f.PostsList(blog, listing...)

	cfg := Config{Blog: blog, OutputDir: outDir, AutoApply: true, Workers: 4}
	if err := New(f.client, cfg).Sync(); err != nil {
		t.Fatalf("Sync: %v\n%s", err, f.log.joined())
	}

	st := mustLoadState(t, blogDir)
	if st.Count() != len(entries) {
		t.Errorf("state holds %d entries, want %d", st.Count(), len(entries))
	}
	for i := range 9 {
		id := fmt.Sprintf("p%d", i)
		got := st.Posts[id]
		switch i % 3 {
		case 0:
			if got.UpdatedAt != 200 || got.Title != "new" {
				t.Errorf("%s: UpdatedAt=%d Title=%q, want 200 \"new\"", id, got.UpdatedAt, got.Title)
			}
		case 1:
			if got.CommentsCount != 1 {
				t.Errorf("%s: CommentsCount = %d, want 1", id, got.CommentsCount)
			}
		case 2:
			if !got.Locked {
				t.Errorf("%s: Locked = false, want true", id)
			}
		}
	}
}

// A tracked, unlocked post is skipped without --force: nothing is fetched
// or rewritten.
func TestEngine_DownloadAll_SkipsTrackedPost(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	mustMkdir(t, postDir)
	const archived = `{"id":"p1","title":"old","updatedAt":100}`
	writeFile(t, filepath.Join(postDir, "post.json"), archived)
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "old", DirName: "p1_dir", UpdatedAt: 100, Archive: archiveFormat},
	})
	// Signed video media would otherwise trigger a per-post fetch.
	post := boosty.Post{
		ID: "p1", Title: "new", HasAccess: true, PublishTime: 1700000000, UpdatedAt: 200,
		Data: []boosty.ContentBlock{imageBlock(f, "img-1", "a.jpg"), videoBlock(f, "vid-1", "v.mp4")},
	}
	f.PostsList(blog, post)
	postFetches := countPostFetches(f, blog, post)
	imageGets, videoGets := countGETs(f, "a.jpg", "IMG"), countGETs(f, "v.mp4", "VID")

	if err := New(f.client, Config{Blog: blog, OutputDir: outDir}).DownloadAll(); err != nil {
		t.Fatalf("DownloadAll: %v\n%s", err, f.log.joined())
	}
	if n := postFetches.Load() + imageGets.Load() + videoGets.Load(); n != 0 {
		t.Errorf("%d request(s) for a tracked post, want 0", n)
	}
	if got := readFileString(t, filepath.Join(postDir, "post.json")); got != archived {
		t.Errorf("post.json rewritten:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(postDir, "image_001.jpg")); !os.IsNotExist(err) {
		t.Errorf("image_001.jpg stat err = %v, want not exist", err)
	}
}

// The listing's signed video URL may be dead by download time; the per-post
// payload carries a fresh one, and both the download and post.json must use it.
func TestEngine_DownloadAll_UsesRefreshedSignedURL(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	listed := boosty.Post{
		ID: "p1", Title: "t", HasAccess: true, PublishTime: 1700000000, UpdatedAt: 100,
		Data: []boosty.ContentBlock{videoBlock(f, "vid-1", "stale.mp4")}, // unregistered: 404
	}
	fresh := listed
	fresh.Data = []boosty.ContentBlock{videoBlock(f, "vid-1", "fresh.mp4")}
	f.PostsList(blog, listed)
	f.SinglePost(blog, "p1", fresh)
	f.Media("fresh.mp4", []byte("FRESH-VIDEO"))

	outDir := t.TempDir()
	if err := New(f.client, Config{Blog: blog, OutputDir: outDir}).DownloadAll(); err != nil {
		t.Fatalf("DownloadAll: %v\n%s", err, f.log.joined())
	}
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, mustLoadState(t, blogDir).Posts["p1"].DirName)
	if got := readFileString(t, filepath.Join(postDir, "video_001.mp4")); got != "FRESH-VIDEO" {
		t.Errorf("video_001.mp4 = %q, want FRESH-VIDEO", got)
	}
	pj := readFileString(t, filepath.Join(postDir, "post.json"))
	if !strings.Contains(pj, "fresh.mp4") || strings.Contains(pj, "stale.mp4") {
		t.Errorf("post.json does not carry the refreshed URL:\n%s", pj)
	}
}

// A listing entry that does not parse (schema drift) or carries no id must
// fail the run and be named, never be skipped with exit 0; a post listed
// twice is processed once; and the run's error names the posts that failed.
func TestEngine_ListingDriftFailsRunAndNamesPosts(t *testing.T) {
	for _, run := range []string{"sync", "download"} {
		t.Run(run, func(t *testing.T) {
			f := newFakeAPI(t)
			gets := countGETs(f, "a.jpg", "IMG")
			good := boosty.Post{ID: "p1", Title: "Good", HasAccess: true, PublishTime: 1700000000, UpdatedAt: 100,
				Data: []boosty.ContentBlock{imageBlock(f, "img-a", "a.jpg")}}
			bad := boosty.Post{ID: "p2", Title: "Broken media", HasAccess: true, PublishTime: 1700000001, UpdatedAt: 100,
				Data: []boosty.ContentBlock{imageBlock(f, "img-x", "missing.jpg")}}
			raws := []json.RawMessage{mustJSON(t, good), mustJSON(t, good), mustJSON(t, bad),
				json.RawMessage(`{"id":42,"title":"Typed wrong","hasAccess":true}`),
				json.RawMessage(`{"title":"No id","hasAccess":true}`)}
			f.HandleFunc("GET", "/v1/blog/myblog/post/", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(boosty.PostsResponse{Data: raws, Extra: boosty.PaginationExtra{IsLast: true}})
			})
			saved := boosty.RetryDelays
			boosty.RetryDelays = nil
			defer func() { boosty.RetryDelays = saved }()

			e := New(f.client, Config{Blog: "myblog", OutputDir: t.TempDir(), AutoApply: true})
			var err error
			if run == "sync" {
				err = e.Sync()
			} else {
				err = e.DownloadAll()
			}
			if err == nil {
				t.Fatal("run over a drifted listing returned nil")
			}
			for _, want := range []string{`"Broken media" (p2)`, "title=Typed wrong", "title=No id", "3 post(s) failed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s", err, want)
				}
			}
			if n := gets.Load(); n != 1 {
				t.Errorf("image GETs = %d, want 1 (the repeated post is processed once)", n)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An accessible post with no blocks in both the listing and the per-post
// payload is an empty post, not a stub: an edit to it must land instead of
// failing every sync.
func TestEngine_EmptyPostEditLands(t *testing.T) {
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, "myblog")
	postDir := filepath.Join(blogDir, "p1_dir")
	mustMkdir(t, postDir)
	writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1","updatedAt":100,"data":[]}`)
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "t", DirName: "p1_dir", UpdatedAt: 100, Archive: archiveFormat, Media: map[string]string{}},
	})
	post := boosty.Post{ID: "p1", Title: "t2", HasAccess: true, UpdatedAt: 200, PublishTime: 1700000000}
	f.PostsList("myblog", post)
	f.SinglePost("myblog", "p1", post)

	if err := New(f.client, Config{Blog: "myblog", OutputDir: outDir, AutoApply: true}).Sync(); err != nil {
		t.Fatalf("Sync: %v\n%s", err, f.log.joined())
	}
	if got := mustLoadState(t, blogDir).Posts["p1"].UpdatedAt; got != 200 {
		t.Errorf("UpdatedAt = %d, want 200", got)
	}
}

// A listing that lost its blocks (API drift) must not be archived as the
// post: a new post is refetched, and a payload without blocks never replaces
// an archived post.json that has them, even under --force.
func TestEngine_ListingWithoutBlocks(t *testing.T) {
	t.Run("new post refetched", func(t *testing.T) {
		f := newFakeAPI(t)
		countGETs(f, "a.jpg", "IMG")
		listed := boosty.Post{ID: "p1", Title: "t", HasAccess: true, UpdatedAt: 100, PublishTime: 1700000000}
		full := listed
		full.Data = []boosty.ContentBlock{textBlock("body"), imageBlock(f, "img-a", "a.jpg")}
		f.PostsList("myblog", listed)
		f.SinglePost("myblog", "p1", full)
		outDir := t.TempDir()
		if err := New(f.client, Config{Blog: "myblog", OutputDir: outDir, AutoApply: true}).DownloadAll(); err != nil {
			t.Fatalf("DownloadAll: %v", err)
		}
		dir := filepath.Join(outDir, "myblog", mustLoadState(t, filepath.Join(outDir, "myblog")).Posts["p1"].DirName)
		if pj := readFileString(t, filepath.Join(dir, "post.json")); !strings.Contains(pj, "body") {
			t.Errorf("post.json lacks the refetched blocks: %s", pj)
		}
	})
	t.Run("force keeps archived content", func(t *testing.T) {
		f := newFakeAPI(t)
		outDir := t.TempDir()
		blogDir := filepath.Join(outDir, "myblog")
		postDir := filepath.Join(blogDir, "p1_dir")
		mustMkdir(t, postDir)
		archived := `{"id":"p1","updatedAt":100,"data":[{"type":"text","content":"[\"body\",\"unstyled\",[]]"}]}`
		writeFile(t, filepath.Join(postDir, "post.json"), archived)
		writeInitialState(t, blogDir, map[string]state.PostEntry{
			"p1": {Title: "t", DirName: "p1_dir", UpdatedAt: 100, Archive: archiveFormat},
		})
		empty := boosty.Post{ID: "p1", Title: "t", HasAccess: true, UpdatedAt: 100, PublishTime: 1700000000}
		f.PostsList("myblog", empty)
		f.SinglePost("myblog", "p1", empty)
		err := New(f.client, Config{Blog: "myblog", OutputDir: outDir, AutoApply: true, Force: true}).DownloadAll()
		if err == nil {
			t.Error("DownloadAll --force over an empty payload returned nil")
		}
		if pj := readFileString(t, filepath.Join(postDir, "post.json")); pj != archived {
			t.Errorf("post.json replaced: %s", pj)
		}
	})
}

// A payload without updatedAt says nothing about an edit: no UPDATED, no
// refetch, and state keeps its UpdatedAt.
func TestEngine_MissingUpdatedAtIsNoEdit(t *testing.T) {
	f := newFakeAPI(t)
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, "myblog")
	mustMkdir(t, filepath.Join(blogDir, "p1_dir"))
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "t", DirName: "p1_dir", UpdatedAt: 100, Archive: archiveFormat, Media: map[string]string{}},
	})
	f.PostsList("myblog", boosty.Post{ID: "p1", Title: "t", HasAccess: true, PublishTime: 1700000000})

	if err := New(f.client, Config{Blog: "myblog", OutputDir: outDir, AutoApply: true}).Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if log := f.log.joined(); !strings.Contains(log, "Everything up to date.") {
		t.Errorf("sync acted on a payload without updatedAt:\n%s", log)
	}
	if got := mustLoadState(t, blogDir).Posts["p1"].UpdatedAt; got != 100 {
		t.Errorf("UpdatedAt = %d, want 100", got)
	}
}

// After _state.json is lost, a post whose title changed must reuse its old
// folder (found by the id in its post.json), not download again into a new
// one, and keep tracking the post.md and comments.json already there.
func TestEngine_StateLossReusesArchivedFolder(t *testing.T) {
	f := newFakeAPI(t)
	gets := countGETs(f, "a.jpg", "IMG")
	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, "myblog")
	old := filepath.Join(blogDir, "2023-11-14_Old title")
	mustMkdir(t, old)
	post := boosty.Post{ID: "p1", Title: "New title", HasAccess: true, UpdatedAt: 100, PublishTime: 1700000000,
		Data: []boosty.ContentBlock{imageBlock(f, "img-a", "a.jpg")}}
	writePostJSON(t, old, post)
	writeFile(t, filepath.Join(old, "image_001.jpg"), "IMG")
	writeFile(t, filepath.Join(old, "post.md"), "old md")
	writeFile(t, filepath.Join(old, "comments.json"), `{"data":[]}`)
	f.PostsList("myblog", post)
	f.CommentsList("myblog", "p1")

	if err := New(f.client, Config{Blog: "myblog", OutputDir: outDir, AutoApply: true}).DownloadAll(); err != nil {
		t.Fatalf("DownloadAll: %v\n%s", err, f.log.joined())
	}
	entry := mustLoadState(t, blogDir).Posts["p1"]
	if entry.DirName != "2023-11-14_Old title" || !entry.HasMd || !entry.HasComments {
		t.Errorf("entry = %+v, want the old folder with post.md and comments.json tracked", entry)
	}
	if n := gets.Load(); n != 0 {
		t.Errorf("image GETs = %d, want 0 (the archived copy is reused)", n)
	}
	entries, err := os.ReadDir(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	var folders []string
	for _, d := range entries {
		if d.IsDir() {
			folders = append(folders, d.Name())
		}
	}
	if len(folders) != 1 {
		t.Errorf("post folders = %v, want only the old one", folders)
	}
}
