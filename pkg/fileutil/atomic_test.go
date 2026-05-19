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
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prevWD); err != nil {
			t.Logf("restore cwd: %v", err)
		}
	})

	if err := WriteFileAtomic("out.json", []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.json")); err != nil {
		t.Fatalf("expected out.json in cwd: %v", err)
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
