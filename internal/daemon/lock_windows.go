package daemon

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

type heldLock struct {
	file       *os.File
	overlapped *windows.Overlapped
}

var errAgentLockBusy = errors.New("agent lock is held")

func acquireAgentLock(path string) (heldLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return heldLock{}, err
	}
	o := &windows.Overlapped{}
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, o)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return heldLock{}, errAgentLockBusy
		}
		return heldLock{}, err
	}
	return heldLock{file: f, overlapped: o}, nil
}
func (l heldLock) Close() error {
	if l.file == nil {
		return nil
	}
	err := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, l.overlapped)
	return errors.Join(err, l.file.Close())
}
