package syncer

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/state"
)

func TestMediaPlan_Replaces(t *testing.T) {
	landed := map[string]string{"image_001.jpg": "a", "video_001.mp4": "v"}
	for _, tc := range []struct {
		name string
		plan mediaPlan
		item parser.MediaItem
		want bool
	}{
		{"not edited keeps", mediaPlan{landed: landed}, parser.MediaItem{Filename: "image_001.jpg", ID: "b"}, false},
		{"edited, same block", mediaPlan{landed: landed, edited: true}, parser.MediaItem{Filename: "image_001.jpg", ID: "a"}, false},
		{"edited, swapped block", mediaPlan{landed: landed, edited: true}, parser.MediaItem{Filename: "image_001.jpg", ID: "b"}, true},
		{"edited, no record for file", mediaPlan{landed: landed, edited: true}, parser.MediaItem{Filename: "image_002.jpg", ID: "a"}, true},
		{"edited, no record at all", mediaPlan{edited: true}, parser.MediaItem{Filename: "image_001.jpg", ID: "a"}, true},
		{"edited, item without ID", mediaPlan{landed: map[string]string{"image_001.jpg": ""}, edited: true}, parser.MediaItem{Filename: "image_001.jpg"}, true},
		{"named by check-media", mediaPlan{landed: landed, replace: []string{"video_001.mp4"}}, parser.MediaItem{Filename: "video_001.mp4", ID: "v"}, true},
		{"not named by check-media", mediaPlan{landed: landed, replace: []string{"video_001.mp4"}}, parser.MediaItem{Filename: "video_002.mp4", ID: "w"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.plan.replaces(tc.item); got != tc.want {
				t.Errorf("replaces = %v, want %v", got, tc.want)
			}
		})
	}
}

// A failed download leaves the old bytes in place, so the old entry must
// survive; a landed item without an ID leaves nothing that names its bytes.
func TestMediaPlan_RecordKeepsFailedFiles(t *testing.T) {
	plan := mediaPlan{landed: map[string]string{
		"image_001.jpg": "old-1", // replaced: lands below
		"image_002.jpg": "old-2", // replacement failed: not in landed
		"image_003.jpg": "old-3", // landed for an item without an ID
	}}
	got := plan.record([]parser.MediaItem{
		{Filename: "image_001.jpg", ID: "new-1"},
		{Filename: "image_003.jpg"},
		{Filename: "video_001.mp4", ID: "v"},
	})
	want := map[string]string{"image_001.jpg": "new-1", "image_002.jpg": "old-2", "video_001.mp4": "v"}
	if !maps.Equal(got, want) {
		t.Errorf("record = %v, want %v", got, want)
	}
	if plan.landed["image_001.jpg"] != "old-1" {
		t.Error("record mutated the plan's own map")
	}
	if rec := (mediaPlan{}).record(nil); rec == nil {
		t.Error("record after a run that landed nothing = nil, want an empty record (nil means the channel did not run)")
	}
}

// The post.json fallback for entries without a media record is trusted only
// at state's UpdatedAt: runApplyActions writes post.json before media, so a
// newer one may sit next to bytes whose replacement failed.
func TestNewMediaPlan_PostJSONFallback(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "post.json"), `{"id":"p1","updatedAt":100,"data":[
		{"type":"image","id":"img-a","url":"https://cdn/a.jpg"},
		{"type":"image","url":"https://cdn/no-id.jpg"}]}`)
	fromDisk := map[string]string{"image_001.jpg": "img-a"}
	recorded := map[string]string{"image_001.jpg": "img-r"}

	for _, tc := range []struct {
		name   string
		old    state.PostEntry
		edited bool
		want   map[string]string
	}{
		{"post.json at state's updatedAt", state.PostEntry{UpdatedAt: 100}, true, fromDisk},
		{"post.json ahead of state", state.PostEntry{UpdatedAt: 50}, true, nil},
		{"no state updatedAt", state.PostEntry{}, true, nil},
		{"recorded entry wins", state.PostEntry{UpdatedAt: 100, Media: recorded}, true, recorded},
		{"not edited reads nothing", state.PostEntry{UpdatedAt: 100}, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newMediaPlan(dir, tc.old, tc.edited, nil).landed; !maps.Equal(got, tc.want) || (got == nil) != (tc.want == nil) {
				t.Errorf("landed = %v, want %v", got, tc.want)
			}
		})
	}
	if got := newMediaPlan(t.TempDir(), state.PostEntry{UpdatedAt: 100}, true, nil).landed; got != nil {
		t.Errorf("landed without post.json = %v, want nil", got)
	}
}

func TestBuildSyncEntry_MediaFollowsFailedRun(t *testing.T) {
	old := state.PostEntry{UpdatedAt: 100, Media: map[string]string{"image_001.jpg": "a"}}
	post := &boosty.Post{UpdatedAt: 200}
	rec := map[string]string{"image_001.jpg": "b"}

	got := buildSyncEntry(old, post, "d", true, applyOutcome{PostJSONOK: true, MDOK: true, CommentsOK: true, Media: rec})
	if got.UpdatedAt != 100 {
		t.Errorf("UpdatedAt = %d, want 100 (media failed)", got.UpdatedAt)
	}
	if !maps.Equal(got.Media, rec) {
		t.Errorf("Media = %v, want the record of what landed %v", got.Media, rec)
	}

	got = buildSyncEntry(old, post, "d", false, applyOutcome{PostJSONOK: true, MediaOK: true, MDOK: true, CommentsOK: true})
	if !maps.Equal(got.Media, old.Media) {
		t.Errorf("Media = %v, want %v kept when the media channel did not run", got.Media, old.Media)
	}
}

// --- end-to-end: which media files an edit or a check re-downloads ---

// countGETs serves content at f.MediaURL(name), like fakeAPI.Media, and
// counts the GETs.
func countGETs(f *fakeAPI, name, content string) *atomic.Int32 {
	var n atomic.Int32
	f.Media(name, []byte(content))
	f.HandleFunc("GET", "/_media/"+name, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		fmt.Fprint(w, content)
	})
	return &n
}

func imageBlock(f *fakeAPI, id, name string) boosty.ContentBlock {
	return boosty.ContentBlock{Type: "image", ID: id, URL: f.MediaURL(name)}
}

func videoBlock(f *fakeAPI, id, name string) boosty.ContentBlock {
	return boosty.ContentBlock{Type: "ok_video", ID: id, PlayerURLs: []boosty.PlayerURL{{Type: "full_hd", URL: f.MediaURL(name)}}}
}

func textBlock(s string) boosty.ContentBlock {
	return boosty.ContentBlock{Type: "text", Content: `["` + s + `","unstyled",[]]`}
}

func writePostJSON(t *testing.T, dir string, p boosty.Post) {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "post.json"), string(data))
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A text-only edit keeps the same media blocks, so no media is fetched —
// whether the entry carries a media record or predates it (the post.json
// that landed at state's UpdatedAt stands in), and on every path that
// handles an edit: Sync UPDATED, --force, UNLOCKED.
func TestEngine_TextOnlyEditFetchesNoMedia(t *testing.T) {
	recorded := map[string]string{"image_001.jpg": "img-a", "video_001.mp4": "vid-a"}
	for _, tc := range []struct {
		name   string
		media  map[string]string
		locked bool
		run    func(*Engine) error
		force  bool
	}{
		{"sync updated", recorded, false, (*Engine).Sync, false},
		{"sync updated, entry without record", nil, false, (*Engine).Sync, false},
		{"force", recorded, false, (*Engine).DownloadAll, true},
		{"force, entry without record", nil, false, (*Engine).DownloadAll, true},
		{"sync unlocked", recorded, true, (*Engine).Sync, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blog := "myblog"
			f := newFakeAPI(t)
			imgGETs := countGETs(f, "a.jpg", "IMG-SERVER")
			vidGETs := countGETs(f, "a.mp4", "VID-SERVER")

			outDir := t.TempDir()
			blogDir := filepath.Join(outDir, blog)
			postDir := filepath.Join(blogDir, "p1_dir")
			mustMkdir(t, postDir)
			writeFile(t, filepath.Join(postDir, "image_001.jpg"), "IMG-A")
			writeFile(t, filepath.Join(postDir, "video_001.mp4"), "VID-A")
			writePostJSON(t, postDir, boosty.Post{
				ID: "p1", Title: "old", HasAccess: true, UpdatedAt: 100, PublishTime: 1700000000,
				Data: []boosty.ContentBlock{textBlock("old"), imageBlock(f, "img-a", "a.jpg"), videoBlock(f, "vid-a", "a.mp4")},
			})
			writeInitialState(t, blogDir, map[string]state.PostEntry{
				"p1": {Title: "old", DirName: "p1_dir", UpdatedAt: 100, Locked: tc.locked, Media: tc.media},
			})

			edited := boosty.Post{
				ID: "p1", Title: "new", HasAccess: true, UpdatedAt: 200, PublishTime: 1700000000,
				Data: []boosty.ContentBlock{textBlock("typo fixed"), imageBlock(f, "img-a", "a.jpg"), videoBlock(f, "vid-a", "a.mp4")},
			}
			f.PostsList(blog, edited)
			f.SinglePost(blog, "p1", edited)

			e := New(f.client, Config{Blog: blog, OutputDir: outDir, AutoApply: true, Force: tc.force})
			if err := tc.run(e); err != nil {
				t.Fatalf("run: %v\n%s", err, f.log.joined())
			}

			if n, m := imgGETs.Load(), vidGETs.Load(); n != 0 || m != 0 {
				t.Errorf("media GETs: image %d, video %d; want 0 (blocks unchanged)", n, m)
			}
			if got := readFileString(t, filepath.Join(postDir, "video_001.mp4")); got != "VID-A" {
				t.Errorf("video = %q, want the existing copy kept", got)
			}
			st, err := state.Load(blogDir)
			if err != nil {
				t.Fatal(err)
			}
			got := st.Posts["p1"]
			if got.UpdatedAt != 200 || got.Locked {
				t.Errorf("UpdatedAt = %d, Locked = %v; want 200, false", got.UpdatedAt, got.Locked)
			}
			if !maps.Equal(got.Media, recorded) {
				t.Errorf("Media = %v, want %v", got.Media, recorded)
			}
		})
	}
}

// An edit that swaps one image re-downloads that image only.
func TestEngine_Sync_EditSwappingOneImageFetchesIt(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	keptGETs := countGETs(f, "a.jpg", "IMG-A-SERVER")
	swapGETs := countGETs(f, "c.jpg", "IMG-C")

	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	mustMkdir(t, postDir)
	writeFile(t, filepath.Join(postDir, "image_001.jpg"), "IMG-A")
	writeFile(t, filepath.Join(postDir, "image_002.jpg"), "IMG-B")
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "t", DirName: "p1_dir", UpdatedAt: 100,
			Media: map[string]string{"image_001.jpg": "img-a", "image_002.jpg": "img-b"}},
	})

	edited := boosty.Post{
		ID: "p1", Title: "t", HasAccess: true, UpdatedAt: 200,
		Data: []boosty.ContentBlock{imageBlock(f, "img-a", "a.jpg"), imageBlock(f, "img-c", "c.jpg")},
	}
	f.PostsList(blog, edited)
	f.SinglePost(blog, "p1", edited)

	e := New(f.client, Config{Blog: blog, OutputDir: outDir, AutoApply: true})
	if err := e.Sync(); err != nil {
		t.Fatalf("Sync: %v\n%s", err, f.log.joined())
	}

	if n, m := keptGETs.Load(), swapGETs.Load(); n != 0 || m != 1 {
		t.Errorf("GETs: unchanged image %d, swapped image %d; want 0 and 1", n, m)
	}
	if got := readFileString(t, filepath.Join(postDir, "image_001.jpg")); got != "IMG-A" {
		t.Errorf("image_001 = %q, want kept", got)
	}
	if got := readFileString(t, filepath.Join(postDir, "image_002.jpg")); got != "IMG-C" {
		t.Errorf("image_002 = %q, want the swapped-in image", got)
	}
	st, _ := state.Load(blogDir)
	want := map[string]string{"image_001.jpg": "img-a", "image_002.jpg": "img-c"}
	if got := st.Posts["p1"]; got.UpdatedAt != 200 || !maps.Equal(got.Media, want) {
		t.Errorf("entry UpdatedAt = %d, Media = %v; want 200, %v", got.UpdatedAt, got.Media, want)
	}
}

// When one file of an edited post fails, UpdatedAt stays put and the next
// run retries the edit. The retry must fetch only what is still stale: not
// the unchanged image, and not the swapped video that already landed. The
// UNLOCKED path (here through DownloadAll) records what landed too.
func TestEngine_FailedEditRetryFetchesOnlyStaleMedia(t *testing.T) {
	for _, tc := range []struct {
		name   string
		locked bool
		run    func(*Engine) error
	}{
		{"sync updated", false, (*Engine).Sync},
		{"download unlocked", true, (*Engine).DownloadAll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blog := "myblog"
			f := newFakeAPI(t)
			keptGETs := countGETs(f, "a.jpg", "IMG-A-SERVER")
			vidGETs := countGETs(f, "b.mp4", "VID-B")
			var swapGETs atomic.Int32
			f.HandleFunc("GET", "/_media/c.jpg", func(w http.ResponseWriter, r *http.Request) {
				if swapGETs.Add(1) == 1 {
					http.NotFound(w, r) // 4xx fails fast; served on the retry
					return
				}
				fmt.Fprint(w, "IMG-C")
			})

			outDir := t.TempDir()
			blogDir := filepath.Join(outDir, blog)
			postDir := filepath.Join(blogDir, "p1_dir")
			mustMkdir(t, postDir)
			writeFile(t, filepath.Join(postDir, "image_001.jpg"), "IMG-A")
			writeFile(t, filepath.Join(postDir, "video_001.mp4"), "VID-A")
			writeFile(t, filepath.Join(postDir, "image_002.jpg"), "IMG-B")
			writeInitialState(t, blogDir, map[string]state.PostEntry{
				"p1": {Title: "t", DirName: "p1_dir", UpdatedAt: 100, Locked: tc.locked, Media: map[string]string{
					"image_001.jpg": "img-a", "video_001.mp4": "vid-a", "image_002.jpg": "img-b",
				}},
			})

			edited := boosty.Post{
				ID: "p1", Title: "t", HasAccess: true, UpdatedAt: 200, PublishTime: 1700000000,
				Data: []boosty.ContentBlock{
					imageBlock(f, "img-a", "a.jpg"), videoBlock(f, "vid-b", "b.mp4"), imageBlock(f, "img-c", "c.jpg"),
				},
			}
			f.PostsList(blog, edited)
			f.SinglePost(blog, "p1", edited)
			e := New(f.client, Config{Blog: blog, OutputDir: outDir, AutoApply: true})

			if err := tc.run(e); err == nil {
				t.Fatal("first run: nil error, want the failed image reported")
			}
			st, _ := state.Load(blogDir)
			if got := st.Posts["p1"]; got.UpdatedAt != 100 || got.Locked != tc.locked {
				t.Fatalf("after failure UpdatedAt = %d, Locked = %v; want 100, %v (fail-closed)", got.UpdatedAt, got.Locked, tc.locked)
			}
			if got := readFileString(t, filepath.Join(postDir, "image_002.jpg")); got != "IMG-B" {
				t.Fatalf("image_002 after failed replace = %q, want the old bytes kept", got)
			}

			if err := tc.run(e); err != nil {
				t.Fatalf("second run: %v\n%s", err, f.log.joined())
			}
			if n, v, s := keptGETs.Load(), vidGETs.Load(), swapGETs.Load(); n != 0 || v != 1 || s != 2 {
				t.Errorf("GETs over two runs: unchanged image %d, landed video %d, failed image %d; want 0, 1, 2", n, v, s)
			}
			if got := readFileString(t, filepath.Join(postDir, "image_002.jpg")); got != "IMG-C" {
				t.Errorf("image_002 = %q, want replaced on the retry", got)
			}
			st, _ = state.Load(blogDir)
			want := map[string]string{"image_001.jpg": "img-a", "video_001.mp4": "vid-b", "image_002.jpg": "img-c"}
			if got := st.Posts["p1"]; got.UpdatedAt != 200 || got.Locked || !maps.Equal(got.Media, want) {
				t.Errorf("entry UpdatedAt = %d, Locked = %v, Media = %v; want 200, false, %v", got.UpdatedAt, got.Locked, got.Media, want)
			}
		})
	}
}

// --check-media flags one of two videos; the apply re-downloads that one.
func TestEngine_Sync_CheckMediaReplacesOnlyMismatchedVideo(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	okGETs := countGETs(f, "v1.mp4", "AAAA")
	badGETs := countGETs(f, "v2.mp4", "CCCCC")

	outDir := t.TempDir()
	blogDir := filepath.Join(outDir, blog)
	postDir := filepath.Join(blogDir, "p1_dir")
	mustMkdir(t, postDir)
	writeFile(t, filepath.Join(postDir, "video_001.mp4"), "AAAA") // HEAD agrees
	writeFile(t, filepath.Join(postDir, "video_002.mp4"), "CC")   // truncated
	writeInitialState(t, blogDir, map[string]state.PostEntry{
		"p1": {Title: "t", DirName: "p1_dir", UpdatedAt: 100},
	})

	post := boosty.Post{
		ID: "p1", Title: "t", HasAccess: true, UpdatedAt: 100,
		Data: []boosty.ContentBlock{videoBlock(f, "v1", "v1.mp4"), videoBlock(f, "v2", "v2.mp4")},
	}
	f.PostsList(blog, post)
	f.SinglePost(blog, "p1", post)

	e := New(f.client, Config{Blog: blog, OutputDir: outDir, AutoApply: true, CheckMedia: true})
	if err := e.Sync(); err != nil {
		t.Fatalf("Sync: %v\n%s", err, f.log.joined())
	}

	if n, m := okGETs.Load(), badGETs.Load(); n != 0 || m != 1 {
		t.Errorf("GETs: matching video %d, mismatched video %d; want 0 and 1", n, m)
	}
	if got := readFileString(t, filepath.Join(postDir, "video_002.mp4")); got != "CCCCC" {
		t.Errorf("video_002 = %q, want re-downloaded", got)
	}
}
