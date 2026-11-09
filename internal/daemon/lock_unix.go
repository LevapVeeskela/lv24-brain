//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package daemon

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

type heldLock struct{ file *os.File }

var errAgentLockBusy = errors.New("agent lock is held")

func acquireAgentLock(path string) (heldLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return heldLock{}, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return heldLock{}, errAgentLockBusy
		}
		return heldLock{}, err
	}
	return heldLock{file: f}, nil
}
func (l heldLock) Close() error {
	if l.file == nil {
		return nil
	}
	err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return errors.Join(err, l.file.Close())
}
