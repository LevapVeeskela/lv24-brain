package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Defaults for the daily background update check.
const (
	// DailyCheckInterval is how often the background check may run.
	DailyCheckInterval = 24 * time.Hour
	// DailyCheckTimeout bounds one background release lookup.
	DailyCheckTimeout = 3 * time.Second
	// DailyRetryInterval is the backoff after a failed release lookup.
	DailyRetryInterval = 30 * time.Minute

	dailyStateName  = "last-check.json"
	dailyLockSuffix = ".lock"
	dailyStaleLock  = time.Hour
)

// DailyOptions configures MaybeDailyCheck. Zero values select production
// defaults; tests override Now, Check and the file paths.
type DailyOptions struct {
	Dir       string
	StatePath string
	LockPath  string
	Now       func() time.Time
	Interval  time.Duration
	Timeout   time.Duration
	StaleLock time.Duration
	// RetryInterval is the backoff after a failed lookup, so an offline machine
	// does not retry on every command. Zero selects DailyRetryInterval.
	RetryInterval time.Duration
	Check         func(context.Context) (ports.ReleaseInfo, error)
	Getenv        func(string) string
}

// DailyResult reports what the background check did.
type DailyResult struct {
	// Checked is true only when a release lookup ran and succeeded.
	Checked bool
	// Skipped explains why no lookup ran: "disabled", "fresh", "backoff" or
	// "locked".
	Skipped string
	// Latest is the release returned by the lookup, if any.
	Latest *ports.ReleaseInfo
}

type dailyState struct {
	LastCheckedAt time.Time `json:"last_checked_at"`
	LastAttemptAt time.Time `json:"last_attempt_at"`
	LastVersion   string    `json:"last_version"`
	LastChannel   string    `json:"last_channel"`
}

// MaybeDailyCheck runs a notify-only release lookup at most once per interval.
// It never downloads or applies an update, honors BRAIN_NO_UPDATE, uses a
// single-flight lock file, and bounds the lookup by Timeout. A successful
// lookup keeps the check fresh for Interval; a failed lookup records only the
// attempt time and is retried after RetryInterval.
func MaybeDailyCheck(ctx context.Context, opts DailyOptions) (DailyResult, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if envTruthy(getenv("BRAIN_NO_UPDATE")) {
		return DailyResult{Skipped: "disabled"}, nil
	}
	if opts.Check == nil {
		return DailyResult{}, errors.New("update: daily check function is required")
	}
	clock := opts.Now
	if clock == nil {
		clock = time.Now
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DailyCheckInterval
	}
	stale := opts.StaleLock
	if stale <= 0 {
		stale = dailyStaleLock
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DailyCheckTimeout
	}
	retry := opts.RetryInterval
	if retry <= 0 {
		retry = DailyRetryInterval
	}

	dir := opts.Dir
	if dir == "" {
		dir = DefaultUpdateDir(runtime.GOOS, getenv)
	}
	statePath := opts.StatePath
	if statePath == "" {
		statePath = filepath.Join(dir, dailyStateName)
	}
	lockPath := opts.LockPath
	if lockPath == "" {
		lockPath = statePath + dailyLockSuffix
	}
	at := clock().UTC()

	state, haveState := readDailyState(statePath)
	if haveState {
		if isFresh(state.LastCheckedAt, at, interval) {
			return DailyResult{Skipped: "fresh"}, nil
		}
		if isFresh(state.LastAttemptAt, at, retry) {
			return DailyResult{Skipped: "backoff"}, nil
		}
	}

	release, acquired, err := acquireDailyLock(lockPath, at, stale)
	if err != nil {
		return DailyResult{}, err
	}
	if !acquired {
		return DailyResult{Skipped: "locked"}, nil
	}
	defer release()

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	info, err := opts.Check(checkCtx)
	if err != nil {
		return DailyResult{}, recordFailedAttempt(statePath, state, at, err)
	}

	result := DailyResult{Checked: true, Latest: &info}
	next := dailyState{LastCheckedAt: at, LastAttemptAt: at, LastVersion: info.Version, LastChannel: info.Channel}
	if err := writeDailyState(statePath, next); err != nil {
		return result, err
	}
	return result, nil
}

// recordFailedAttempt stores only the attempt time, never the error text, so
// the next command backs off instead of repeating the lookup. The lookup error
// is still returned; a state write failure is joined with it.
func recordFailedAttempt(statePath string, previous dailyState, at time.Time, checkErr error) error {
	wrapped := fmt.Errorf("daily update check: %w", checkErr)
	previous.LastAttemptAt = at
	if err := writeDailyState(statePath, previous); err != nil {
		return errors.Join(wrapped, fmt.Errorf("record update check attempt: %w", err))
	}
	return wrapped
}

// isFresh reports whether last is within the window and not in the future.
// A future timestamp means the clock moved backwards, so the check runs again.
// The same rule backs off retries after a failed lookup.
func isFresh(last, now time.Time, interval time.Duration) bool {
	if last.IsZero() || last.After(now) {
		return false
	}
	return now.Sub(last) < interval
}

func readDailyState(path string) (dailyState, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return dailyState{}, false
	}
	var state dailyState
	if err := json.Unmarshal(raw, &state); err != nil {
		return dailyState{}, false
	}
	return state, true
}

func writeDailyState(path string, state dailyState) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create update state directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".last-check-*")
	if err != nil {
		return fmt.Errorf("create update state file: %w", err)
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		cleanup()
		return fmt.Errorf("encode update state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync update state: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("close update state: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace update state: %w", err)
	}
	return nil
}

// acquireDailyLock creates the single-flight lock. A lock older than stale is
// treated as abandoned by a crashed process and taken over once.
func acquireDailyLock(path string, at time.Time, stale time.Duration) (func(), bool, error) {
	acquired, err := createDailyLock(path)
	if err != nil {
		return nil, false, err
	}
	if acquired {
		return func() { _ = os.Remove(path) }, true, nil
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return acquireDailyLock(path, at, stale)
		}
		return nil, false, fmt.Errorf("inspect update lock: %w", statErr)
	}
	if at.Sub(info.ModTime()) < stale {
		return nil, false, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("clear stale update lock: %w", err)
	}
	acquired, err = createDailyLock(path)
	if err != nil {
		return nil, false, err
	}
	if !acquired {
		return nil, false, nil
	}
	return func() { _ = os.Remove(path) }, true, nil
}

func createDailyLock(path string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create update lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		_ = file.Close()
		return true, nil
	}
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	return false, fmt.Errorf("create update lock: %w", err)
}
