//go:build !unix && !windows

package fsstore

// pidAlive cannot probe another platform here; a lock is then stale only after
// its TTL, which never breaks a lock early.
func pidAlive(pid int) bool {
	return false
}

// bootID is unknown on this platform.
func bootID() string {
	return ""
}
