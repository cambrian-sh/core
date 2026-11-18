//go:build windows

package agentmgr

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile takes an exclusive, non-blocking lock on f's first byte
// (LockFileEx). held reports that another handle — another kernel, or another
// owner in this process — holds it. Windows releases the lock when the
// holding process dies, however it dies.
func tryLockFile(f *os.File) (held bool, err error) {
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return true, nil
	}
	return false, err
}
