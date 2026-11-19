//go:build !unix

package fileutil

// syncDir is a no-op where a directory cannot be opened for fsync (Windows:
// NTFS journals the rename itself).
func syncDir(string) error { return nil }
