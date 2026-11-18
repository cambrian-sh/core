//go:build unix

package agentmgr

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLockFile takes an exclusive, non-blocking flock on f. held reports that
// another open file description — another kernel, or another owner in this
// process — holds it. The kernel releases the lock when the holding process
// dies, however it dies.
func tryLockFile(f *os.File) (held bool, err error) {
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}
