//go:build unix

package fsstore

import (
	"errors"

	"golang.org/x/sys/unix"
)

// pidAlive reports whether pid names a running process on this host. EPERM
// means the process exists but belongs to another user.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}
