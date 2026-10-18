package syncer

import (
	"errors"
	"os"
	"syscall"
)

// lockFile opens path with share mode 0: while the handle is open, every
// other open of the file, from this process or another, fails with
// ERROR_SHARING_VIOLATION. Windows closes the handle when the process exits,
// including on a crash.
func lockFile(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		const errorSharingViolation syscall.Errno = 32
		if errors.Is(err, errorSharingViolation) {
			return nil, errLockHeld
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
