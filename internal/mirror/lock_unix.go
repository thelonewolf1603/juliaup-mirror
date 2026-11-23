//go:build unix

package mirror

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lock takes an exclusive lock on path so two syncs of one mirror cannot run
// at once. The kernel drops it if the process dies.
func lock(path string) (func(), error) {
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fh.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another sync is running on this mirror (%s is locked)", path)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(fh.Fd()), syscall.LOCK_UN)
		fh.Close()
	}, nil
}
