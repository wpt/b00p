package syncer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/state"
)

// An archive written by v0.5.0 holds a post.json rebuilt from the structs (a
// list without its items) and a post.md without lists or headers, under an
// entry with no Archive format. Sync must rewrite both from the list payload
// once, without fetching the post or its unchanged media, and --force must
// do the same. Neither may change what the entry tracks.
func TestMarkdownArchive_RewritesOldArchive(t *testing.T) {
	const want = "# Example\n\nBefore\n\n![image_001.png](image_001.png)\n\n## Section\n\n- Step\n\nAfter\n\n"
	for _, tc := range []struct {
		name string
		run  func(*Engine) error
	}{
		{"sync", (*Engine).Sync},
		{"force", (*Engine).DownloadAll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			const blog = "myblog"
			root := t.TempDir()
			blogDir := filepath.Join(root, blog)
			postDir := filepath.Join(blogDir, "existing")
			mustMkdir(t, postDir)
			writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1","updatedAt":100,"data":[{"type":"list"}]}`)
			writeFile(t, filepath.Join(postDir, "post.md"), "old renderer output")
			writeFile(t, filepath.Join(postDir, "image_001.png"), "existing media")
			writeInitialState(t, blogDir, map[string]state.PostEntry{
				"p1": {Title: "Example", DirName: "existing", UpdatedAt: 100, HasMd: true},
			})
			var post boosty.Post
			source := fmt.Sprintf(`{"id":"p1","title":"Example","hasAccess":true,"updatedAt":100,"data":[
				{"type":"text","content":"Before"},
				{"type":"image","url":%q},
				{"type":"header","content":"[\"Section\",\"header-two\",[]]"},
				{"type":"list","style":"unordered","items":[{"data":[{"type":"text","content":"Step"}]}]},
				{"type":"text","content":"After"}
			]}`, f.MediaURL("diagram.png"))
			if err := json.Unmarshal([]byte(source), &post); err != nil {
				t.Fatal(err)
			}
			f.PostsList(blog, post)
			f.HandleFunc("GET", "/v1/blog/"+blog+"/post/p1", func(w http.ResponseWriter, r *http.Request) {
				t.Error("rewriting an old archive should not fetch the post")
				http.Error(w, "unexpected fetch", http.StatusInternalServerError)
			})
			f.HandleFunc("GET", "/_media/diagram.png", func(w http.ResponseWriter, r *http.Request) {
				t.Error("unchanged media should be reused during the rewrite")
				http.Error(w, "unexpected download", http.StatusInternalServerError)
			})

			cfg := Config{Blog: blog, OutputDir: root, AutoApply: true, Force: tc.name == "force"}
			if err := tc.run(New(f.client, cfg)); err != nil {
				t.Fatal(err)
			}
			md, err := os.ReadFile(filepath.Join(postDir, "post.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(string(md), want) {
				t.Fatalf("post.md lost content or order: %s", md)
			}
			pj, err := os.ReadFile(filepath.Join(postDir, "post.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(pj), `"Step"`) {
				t.Errorf("post.json still lacks the list items: %s", pj)
			}
			st, err := state.Load(blogDir)
			if err != nil {
				t.Fatal(err)
			}
			entry := st.Posts["p1"]
			if entry.DirName != "existing" || entry.UpdatedAt != 100 || !entry.HasMd || entry.Archive != archiveFormat {
				t.Fatalf("rewrite changed tracking or missed the format: %+v", entry)
			}

			f.log.mu.Lock()
			f.log.lines = nil
			f.log.mu.Unlock()
			if err := New(f.client, Config{Blog: blog, OutputDir: root, AutoApply: true}).Sync(); err != nil {
				t.Fatal(err)
			}
			if log := f.log.joined(); !strings.Contains(log, "Everything up to date.") {
				t.Errorf("sync after the rewrite is not quiet:\n%s", log)
			}
		})
	}
}
