// Package fileutil holds small file-I/O helpers shared across packages.
//
// The package is intentionally tiny — it exists so that the atomic-write
// contract (temp file + fsync + chmod + rename in the same directory) lives
// in exactly one place and cannot drift between auth.json, _state.json,
// post.json, and post.md writers.
package fileutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// LockRetryWindow bounds how long RetryLocked retries an operation that
// another process's open handle blocks on Windows. A variable so tests can
// shrink it.
var LockRetryWindow = 2 * time.Second

// RetryLocked runs op until it succeeds, fails with anything but a Windows
// file lock (WindowsFileLocked), or LockRetryWindow runs out. An antivirus,
// indexer, backup client or media player holding a file open makes a rename
// over it fail with "Access is denied" or a sharing violation until that
// handle closes.
func RetryLocked(op func() error) error {
	deadline := time.Now().Add(LockRetryWindow)
	for {
		err := op()
		if err == nil || !WindowsFileLocked(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// WindowsFileLocked reports whether err is ERROR_ACCESS_DENIED or
// ERROR_SHARING_VIOLATION on Windows. Numeric codes keep this free of build
// tags: syscall.Errno exists everywhere, but 5 and 32 are EIO and EPIPE on
// Unix, hence the GOOS guard.
func WindowsFileLocked(err error) bool {
	var errno syscall.Errno
	if runtime.GOOS != "windows" || !errors.As(err, &errno) {
		return false
	}
	const errorAccessDenied, errorSharingViolation = 5, 32
	return errno == errorAccessDenied || errno == errorSharingViolation
}

// WriteFileAtomic writes data to path via a temp file in the same directory,
// fsyncs it, chmods it to perm, renames it over the destination (retrying a
// Windows lock, see RetryLocked) and, on Unix, fsyncs the directory so the
// rename itself survives a power loss. A crash mid-write leaves the original
// file untouched; a successful return means the bytes are on disk under perm
// with the new contents.
//
// A symlink at path is followed: the file it points to is replaced and the
// link stays, so an auth.json linked from elsewhere keeps receiving the
// rotated tokens.
//
// The temp file is created in the destination's directory so the final
// rename never crosses filesystems. When path is a bare filename like
// "auth.json", filepath.Dir returns ".", which lands the temp next to the
// destination — keeping the rename atomic.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
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
	if err := RetryLocked(func() error { return os.Rename(tmpPath, path) }); err != nil {
		return err
	}
	return syncDir(dir)
}

// RemoveIfExists removes path, treating an already-absent file
// (os.IsNotExist) as success. Any other error — a permission problem or a
// Windows sharing-violation from a file held open by an indexer/AV — is
// returned so the caller decides whether a failed unlink is fatal. Lives
// here so the cross-platform unlink contract does not drift between the
// media-download cleanup (pkg/boosty) and the syncer's redownload
// invalidation (pkg/syncer).
func RemoveIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
