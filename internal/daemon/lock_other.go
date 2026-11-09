//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package daemon

import (
	"errors"
	"os"
)

type heldLock struct{ file *os.File }

var errAgentLockBusy = errors.New("agent lock is held")

func acquireAgentLock(path string) (heldLock, error) {
	return heldLock{}, errors.New("daemon: OS process locks unsupported on this platform")
}
func (l heldLock) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}
