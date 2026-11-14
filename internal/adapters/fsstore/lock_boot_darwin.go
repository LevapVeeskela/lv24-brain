//go:build darwin

package fsstore

import (
	"crypto/sha256"
	"encoding/hex"
	"syscall"
)

// bootID returns a stable digest of the kernel boot time, which changes on
// every reboot.
func bootID() string {
	raw, err := syscall.Sysctl("kern.boottime")
	if err != nil || raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return "darwin-" + hex.EncodeToString(sum[:8])
}
