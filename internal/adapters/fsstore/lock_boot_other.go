//go:build unix && !linux && !darwin

package fsstore

// bootID is unknown on this platform; staleness then relies on the pid and the
// TTL, which is the conservative combination.
func bootID() string {
	return ""
}
