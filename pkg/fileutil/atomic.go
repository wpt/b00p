// Package fileutil holds small file-I/O helpers shared across packages.
//
// The package is intentionally tiny — it exists so that the atomic-write
// contract (temp file + fsync + chmod + rename in the same directory) lives
// in exactly one place and cannot drift between auth.json, _state.json,
// post.json, and post.md writers.
package fileutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path via a temp file in the same directory,
// fsyncs it, chmods it to perm, then renames over the destination. A crash
// mid-write leaves the original file untouched; a successful return means the
// bytes are on disk under perm with the new contents.
//
// The temp file is created in filepath.Dir(path) so the final os.Rename never
// crosses filesystems. When path is a bare filename like "auth.json",
// filepath.Dir returns ".", which lands the temp next to the destination —
// keeping the rename atomic.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if anything below fails. A successful Rename makes
	// this a no-op because tmpPath no longer exists.
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
