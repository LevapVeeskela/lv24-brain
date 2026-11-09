package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/levapveeskela/lv24-brain/internal/app"
)

type rotatingLog struct {
	mu        sync.Mutex
	path      string
	maxBytes  int64
	retention int
	file      *os.File
	size      int64
}

func newRotatingLog(path string, maxBytes int64, retention int) (*rotatingLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(filepath.Dir(path), 0o700)
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("daemon: log path is not a regular file")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: open log: %w", err)
	}
	if err := securePath(path, 0o600, false); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon: secure log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &rotatingLog{path: path, maxBytes: maxBytes, retention: retention, file: f, size: info.Size()}, nil
}

func (w *rotatingLog) Write(p []byte) (int, error) {
	originalLength := len(p)
	text, _ := app.RedactIngest(string(p))
	p = []byte(text)
	if int64(len(p)) > w.maxBytes {
		p = p[:w.maxBytes]
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, nil
	}
	return originalLength, nil
}

func (w *rotatingLog) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	for i := w.retention - 1; i >= 1; i-- {
		from, to := fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1)
		if _, err := os.Stat(from); err == nil {
			_ = os.Remove(to)
			if err := os.Rename(from, to); err != nil {
				return err
			}
		}
	}
	if _, err := os.Stat(w.path); err == nil {
		_ = os.Remove(w.path + ".1")
		if err := os.Rename(w.path, w.path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := securePath(w.path, 0o600, false); err != nil {
		_ = f.Close()
		return err
	}
	w.file, w.size = f, 0
	return nil
}

func (w *rotatingLog) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	message, _ := app.RedactIngest(err.Error())
	return strings.ReplaceAll(message, "\n", " ")
}

// LogFiles returns retained log paths in oldest-to-newest order.
func LogFiles(path string) ([]string, error) {
	matches, err := filepath.Glob(path + "*")
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}
