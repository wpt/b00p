//go:build unix

package syncer

import (
	"errors"
	"os"
	"syscall"
)

// lockFile opens path and takes a non-blocking exclusive flock on it. flock
// belongs to the open file description, so a second open conflicts even
// inside one process; the kernel drops it when the descriptor closes,
// including on a crash. The file is opened for writing: NFS emulates flock
// with fcntl locks, and an exclusive one on a read-only descriptor fails
// with EBADF, which would refuse every run on a NAS-mounted archive.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(errLockHeld, closeErr)
		}
		return nil, errors.Join(&os.PathError{Op: "flock", Path: path, Err: err}, closeErr)
	}
	return f, nil
}
