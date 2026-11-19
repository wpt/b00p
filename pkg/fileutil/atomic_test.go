package fileutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileAtomic_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	if err := WriteFileAtomic(path, []byte("hello"), 0644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("contents = %q, want %q", string(data), "hello")
	}
}

func TestWriteFileAtomic_Overwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	if err := os.WriteFile(path, []byte("stale"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("fresh"), 0644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "fresh" {
		t.Errorf("contents = %q, want %q", string(data), "fresh")
	}
}

// Regression: a bare filename like "auth.json" must place the temp file in
// the current working directory, not os.TempDir, or os.Rename can cross
// filesystems and stop being atomic.
func TestWriteFileAtomic_BareFilename(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// A temp file created in os.TempDir would fail here instead of
	// landing on the same volume and passing unnoticed.
	missing := filepath.Join(dir, "no-such-tmp")
	for _, env := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(env, missing)
	}

	if err := WriteFileAtomic("out.json", []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.json")); err != nil {
		t.Fatalf("expected out.json in cwd: %v", err)
	}
}

// A failed rename (here: onto a non-empty directory) must not leave the
// temp file behind; every later write would add another orphan.
func TestWriteFileAtomic_FailedRenameRemovesTemp(t *testing.T) {
	saved := LockRetryWindow
	LockRetryWindow = 0
	defer func() { LockRetryWindow = saved }()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.json")
	if err := os.MkdirAll(filepath.Join(dest, "child"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(dest, []byte("data"), 0644); err == nil {
		t.Fatal("WriteFileAtomic onto a non-empty directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "out.json" {
			t.Errorf("leftover %q after failed rename", e.Name())
		}
	}
}

// Permissions are advisory on Windows (os.Chmod only toggles read-only) so
// we verify exact mode only on POSIX. On Windows, just confirm the file
// exists and is readable.
func TestWriteFileAtomic_Perm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets")

	if err := WriteFileAtomic(path, []byte("x"), 0600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0600 {
			t.Errorf("perm = %v, want 0600", got)
		}
	}
}

// A symlinked destination (an auth.json linked from a config directory) keeps
// its link; the file it points to receives the bytes.
func TestWriteFileAtomic_FollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if err := WriteFileAtomic(link, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link replaced by a regular file (mode %v, err %v)", fi.Mode(), err)
	}
	if data, _ := os.ReadFile(target); string(data) != "new" {
		t.Errorf("target = %q, want new", data)
	}
}
