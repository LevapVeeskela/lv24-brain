//go:build !windows

package daemon

import "os"

func securePath(path string, mode os.FileMode, _ bool) error { return os.Chmod(path, mode) }
