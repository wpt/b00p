//go:build unix

package fileutil

import (
	"errors"
	"os"
	"syscall"
)

// syncDir fsyncs dir so a rename into it is durable: without it a power loss
// can bring back the old directory entry, and with it the old auth.json
// holding a refresh token the server already rotated away. Filesystems that
// cannot fsync a directory (some FUSE and network mounts) answer EINVAL or
// ENOTSUP; that says nothing about the write, so it is not an error.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		err = nil
	}
	return errors.Join(err, d.Close())
}
