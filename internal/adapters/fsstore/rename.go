package fsstore

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"time"
)

// renameAttempts bounds retries of a rename that Windows refuses while
// another process (a reader, an indexer, antivirus) has the target open.
const renameAttempts = 8

// renameFile is os.Rename with a short backoff for transient Windows
// "access denied" / sharing errors. Other errors and other OSes fail at once.
func renameFile(from, to string) error {
	delay := 5 * time.Millisecond
	var err error
	for attempt := 0; attempt < renameAttempts; attempt++ {
		if err = os.Rename(from, to); err == nil || !transientRenameError(err) {
			return err
		}
		time.Sleep(delay)
		delay *= 2
	}
	return err
}

func transientRenameError(err error) bool {
	return runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission)
}

// removeAttempts bounds retries of a delete that Windows refuses while
// another process (a reader, an indexer, antivirus) still has the file open.
const removeAttempts = 8

// removeFile is os.Remove with a short backoff. On Windows a reader of a lock
// file can make the owner's delete fail with a sharing violation; without the
// retry the lock would outlive its writer and block every other writer, since
// a lock with a live pid is never broken.
func removeFile(path string) error {
	delay := 5 * time.Millisecond
	var err error
	for attempt := 0; attempt < removeAttempts; attempt++ {
		err = os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		time.Sleep(delay)
		delay *= 2
	}
	return err
}
