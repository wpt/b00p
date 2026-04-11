package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wpt/b00p/pkg/state"
)

func TestCheckMissingFiles_AllPresent(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "post.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "comments.json"), []byte("[]"), 0644)
	os.WriteFile(filepath.Join(dir, "post.md"), []byte("# title"), 0644)

	entry := state.PostEntry{HasComments: true, HasMd: true}
	got := checkMissingFiles(entry, dir)
	if got != "" {
		t.Errorf("checkMissingFiles = %q, want empty", got)
	}
}

func TestCheckMissingFiles_CommentsMissing(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "post.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "post.md"), []byte("# title"), 0644)

	entry := state.PostEntry{HasComments: true, HasMd: true}
	got := checkMissingFiles(entry, dir)
	if got != "comments.json" {
		t.Errorf("checkMissingFiles = %q, want 'comments.json'", got)
	}
}

func TestCheckMissingFiles_MdMissing(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "post.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "comments.json"), []byte("[]"), 0644)

	entry := state.PostEntry{HasComments: true, HasMd: true}
	got := checkMissingFiles(entry, dir)
	if got != "post.md" {
		t.Errorf("checkMissingFiles = %q, want 'post.md'", got)
	}
}

func TestCheckMissingFiles_PostJsonMissing(t *testing.T) {
	dir := t.TempDir()

	entry := state.PostEntry{}
	got := checkMissingFiles(entry, dir)
	if got != "post.json" {
		t.Errorf("checkMissingFiles = %q, want 'post.json'", got)
	}
}

func TestCheckMissingFiles_MultipleMissing(t *testing.T) {
	dir := t.TempDir()

	entry := state.PostEntry{HasComments: true, HasMd: true}
	got := checkMissingFiles(entry, dir)
	if got != "post.json, comments.json, post.md" {
		t.Errorf("checkMissingFiles = %q, want 'post.json, comments.json, post.md'", got)
	}
}

func TestCheckMissingFiles_NoCommentsNoMd(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "post.json"), []byte("{}"), 0644)

	entry := state.PostEntry{HasComments: false, HasMd: false}
	got := checkMissingFiles(entry, dir)
	if got != "" {
		t.Errorf("checkMissingFiles = %q, want empty (comments/md not expected)", got)
	}
}
