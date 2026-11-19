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

func TestMarkdownArchive_ForceRegeneratesUnchangedPost(t *testing.T) {
	f := newFakeAPI(t)
	const blog = "myblog"
	root := t.TempDir()
	blogDir := filepath.Join(root, blog)
	postDir := filepath.Join(blogDir, "existing")
	mustMkdir(t, postDir)
	writeFile(t, filepath.Join(postDir, "post.json"), `{"id":"p1","updatedAt":100}`)
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
	f.HandleFunc("GET", "/_media/diagram.png", func(w http.ResponseWriter, r *http.Request) {
		t.Error("unchanged media should be reused during Markdown backfill")
		http.Error(w, "unexpected download", http.StatusInternalServerError)
	})
	cfg := Config{Blog: blog, OutputDir: root, WithMD: true, AutoApply: true, CheckFiles: true}
	if err := New(f.client, cfg).Sync(); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(filepath.Join(postDir, "post.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(md) != "old renderer output" {
		t.Fatalf("unchanged sync unexpectedly rewrote Markdown: %s", md)
	}
	cfg.Force = true
	if err := New(f.client, cfg).DownloadAll(); err != nil {
		t.Fatal(err)
	}
	md, err = os.ReadFile(filepath.Join(postDir, "post.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "# Example\n\nBefore\n\n![image_001.png](image_001.png)\n\n## Section\n\n- Step\n\nAfter\n\n"
	if !strings.HasSuffix(string(md), want) {
		t.Fatalf("Markdown backfill lost content or order: %s", md)
	}
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	if entry := st.Posts["p1"]; entry.DirName != "existing" || entry.UpdatedAt != 100 || !entry.HasMd {
		t.Fatalf("backfill changed tracking: %+v", entry)
	}
}

func TestMarkdownArchive_NestedMediaChecks(t *testing.T) {
	for _, kind := range []string{"ok_video", "audio_file", "file", "image"} {
		t.Run(kind, func(t *testing.T) {
			var post boosty.Post
			source := fmt.Sprintf(`{"data":[{"type":"list","items":[{"items":[{"data":[{"type":%q}]}]}]}]}`, kind)
			if err := json.Unmarshal([]byte(source), &post); err != nil {
				t.Fatal(err)
			}
			if got, want := hasOkVideo(post.Data), kind == "ok_video"; got != want {
				t.Errorf("hasOkVideo = %v, want %v", got, want)
			}
			if got, want := hasSignedMedia(post.Data), kind != "image"; got != want {
				t.Errorf("hasSignedMedia = %v, want %v", got, want)
			}
		})
	}
}
