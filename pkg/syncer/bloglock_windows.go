package syncer

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockOpenWait bounds how long lockFile retries a sharing violation before
// reporting the lock as held. An indexer, sync client or AV scanner that
// opens .b00p.lock for a moment blocks the open exactly like another b00p
// run; without the retry such a run fails with a phantom "another b00p run".
// A package variable so tests can shrink it.
var lockOpenWait = 2 * time.Second

// lockFile opens path with share mode 0: while the handle is open, every
// other open of the file, from this process or another, fails with
// ERROR_SHARING_VIOLATION. Windows closes the handle when the process exits,
// including on a crash.
func lockFile(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	const errorSharingViolation syscall.Errno = 32
	deadline := time.Now().Add(lockOpenWait)
	for {
		h, err := syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil,
			syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			return os.NewFile(uintptr(h), path), nil
		}
		if !errors.Is(err, errorSharingViolation) {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		if time.Now().After(deadline) {
			return nil, errLockHeld
		}
		time.Sleep(50 * time.Millisecond)
	}
}
