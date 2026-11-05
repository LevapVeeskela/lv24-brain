//go:build !windows

package update

import (
	"errors"
	"time"
)

func scheduleReplace(string, string, string, func() time.Time) error {
	return errors.New("update: binary replacement helper is only available on Windows")
}
