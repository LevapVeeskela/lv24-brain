//go:build linux

package fsstore

import (
	"os"
	"strings"
)

// bootID returns the kernel boot id, which is unique per boot.
func bootID() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
