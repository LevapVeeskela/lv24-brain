package daemon

import (
	"errors"
	"io/fs"
	"time"

	"golang.org/x/sys/windows"
)

// replaceFile is MoveFileEx with a short backoff for transient Windows
// "access denied"/sharing errors, e.g. another process is reading the target.
func replaceFile(from, to string) error {
	fromPath, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPath, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	delay := 5 * time.Millisecond
	for attempt := 0; attempt < 20; attempt++ {
		err = windows.MoveFileEx(fromPath, toPath, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
		if err == nil || !transientReplaceError(err) {
			return err
		}
		time.Sleep(delay)
		if delay < 100*time.Millisecond {
			delay *= 2
		}
	}
	return err
}

func transientReplaceError(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
