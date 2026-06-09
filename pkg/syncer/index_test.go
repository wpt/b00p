package syncer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/state"
)

func TestRenderBlogIndex_SortedWithMarkers(t *testing.T) {
	posts := map[string]state.PostEntry{
		"p2": {Title: "Second", DirName: "2026-02-01_Second", HasComments: true, CommentsCount: 9},
		"p3": {Title: "Locked one", DirName: "2026-03-01_Locked one", Locked: true},
		"p1": {Title: "First", DirName: "2026-01-01_First"},
	}

	got := renderBlogIndex("myblog", posts)

	if !strings.HasPrefix(got, "# myblog — archive index\n") {
		t.Errorf("missing header, got:\n%s", got)
	}
	if !strings.Contains(got, "3 post(s) (1 locked)") {
		t.Errorf("missing counts line, got:\n%s", got)
	}
	// Sorted by DirName → chronological under the default {date}_{title}.
	i1 := strings.Index(got, "2026-01-01_First")
	i2 := strings.Index(got, "2026-02-01_Second")
	i3 := strings.Index(got, "2026-03-01_Locked one")
	if !(i1 >= 0 && i1 < i2 && i2 < i3) {
		t.Errorf("entries out of DirName order (%d, %d, %d):\n%s", i1, i2, i3, got)
	}
	// Angle-bracket destination survives spaces in the directory name.
	if !strings.Contains(got, "- [Second](<./2026-02-01_Second/>) — 9 comment(s)") {
		t.Errorf("comments line malformed, got:\n%s", got)
	}
	if !strings.Contains(got, "- [Locked one](<./2026-03-01_Locked one/>) (locked)") {
		t.Errorf("locked marker malformed, got:\n%s", got)
	}
	// First has HasComments=false: no comment count even though the field
	// defaults to zero.
	if strings.Contains(got, "[First](<./2026-01-01_First/>) —") {
		t.Errorf("comment count rendered for a post without tracked comments:\n%s", got)
	}
}

func TestRenderBlogIndex_EscapesBracketsInTitle(t *testing.T) {
	posts := map[string]state.PostEntry{
		"p1": {Title: "[draft] stream", DirName: "d1"},
	}
	got := renderBlogIndex("b", posts)
	if !strings.Contains(got, `- [\[draft\] stream](<./d1/>)`) {
		t.Errorf("brackets in title must be escaped, got:\n%s", got)
	}
}

func TestRenderBlogIndex_EscapesBackslashInTitle(t *testing.T) {
	// A raw trailing backslash would render as `[title\](...)` where \] is
	// a CommonMark escape — the link text never closes and the entry
	// degrades to literal text. Backslashes must be doubled.
	posts := map[string]state.PostEntry{
		"p1": {Title: `ends with \`, DirName: "d1"},
		"p2": {Title: `a\[b`, DirName: "d2"},
	}
	got := renderBlogIndex("b", posts)
	if !strings.Contains(got, `- [ends with \\](<./d1/>)`) {
		t.Errorf("trailing backslash must be doubled, got:\n%s", got)
	}
	if !strings.Contains(got, `- [a\\\[b](<./d2/>)`) {
		t.Errorf("backslash-before-bracket must escape both, got:\n%s", got)
	}
}

// Sync writes the index after the apply phase; DownloadAll after its pool.
// Both also self-heal a deleted index on no-change runs (pinned for Sync's
// no-action branch below).
func TestEngine_Sync_WritesBlogIndex(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)

	post := boosty.Post{
		ID: "p1", Title: "Indexed post", HasAccess: true,
		PublishTime: 1700000000, UpdatedAt: 100,
		Data: []boosty.ContentBlock{{Type: "text", Content: `["body","unstyled",[]]`}},
	}
	f.PostsList(blog, post)

	cfg := Config{Blog: blog, OutputDir: t.TempDir(), AutoApply: true}
	e := New(f.client, cfg)
	if err := e.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	idx, err := os.ReadFile(filepath.Join(cfg.OutputDir, blog, blogIndexName))
	if err != nil {
		t.Fatalf("index.md not written: %v", err)
	}
	if !strings.Contains(string(idx), "Indexed post") {
		t.Errorf("index.md missing the post title:\n%s", idx)
	}

	// Delete the index and run a no-change sync: it must self-heal.
	if err := os.Remove(filepath.Join(cfg.OutputDir, blog, blogIndexName)); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.OutputDir, blog, blogIndexName)); err != nil {
		t.Errorf("index.md not regenerated on a no-change sync: %v", err)
	}
}

func TestEngine_DownloadAll_WritesBlogIndex(t *testing.T) {
	blog := "myblog"
	f := newFakeAPI(t)
	f.PostsList(blog, boosty.Post{
		ID: "p1", Title: "Indexed post", HasAccess: true,
		PublishTime: 1700000000, UpdatedAt: 100,
	})

	cfg := Config{Blog: blog, OutputDir: t.TempDir()}
	e := New(f.client, cfg)
	if err := e.DownloadAll(); err != nil {
		t.Fatalf("DownloadAll: %v", err)
	}

	idx, err := os.ReadFile(filepath.Join(cfg.OutputDir, blog, blogIndexName))
	if err != nil {
		t.Fatalf("index.md not written: %v", err)
	}
	if !strings.Contains(string(idx), "Indexed post") {
		t.Errorf("index.md missing the post title:\n%s", idx)
	}
}
