//go:build windows

package fsstore

import (
	"strconv"
	"syscall"
	"time"
)

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
	// bootIDBucketSeconds makes the derived boot time stable across processes:
	// two writers started at different times must agree on the boot identity.
	bootIDBucketSeconds = 600
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetTickCount64 = kernel32.NewProc("GetTickCount64")
)

// bootID identifies the current boot. Windows has no boot-id file, so the value
// is the boot time derived from the system uptime, rounded down to ten minutes.
// Clock adjustments of minutes could change it, which at worst makes a lock
// look like it came from another boot; the TTL still applies.
func bootID() string {
	tick, _, _ := procGetTickCount64.Call()
	if tick == 0 {
		return ""
	}
	boot := time.Now().Unix() - int64(tick)/1000
	return "win-" + strconv.FormatInt(boot/bootIDBucketSeconds, 10)
}

// pidAlive reports whether pid names a running process on this host.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = syscall.CloseHandle(handle) }()
	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		// The handle opened, so the process exists; an exit-code probe failure
		// must not make a live lock look stale.
		return true
	}
	return code == stillActive
}
