//go:build !windows

package daemon

import "os"

func replaceFile(from, to string) error { return os.Rename(from, to) }
