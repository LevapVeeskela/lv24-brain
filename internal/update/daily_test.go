package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func noEnv(string) string { return "" }

func TestMaybeDailyCheckFirstRunWritesState(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	calls := 0
	check := func(context.Context) (ports.ReleaseInfo, error) {
		calls++
		return ports.ReleaseInfo{Version: "1.2.3", Channel: "stable"}, nil
	}

	result, err := MaybeDailyCheck(context.Background(), DailyOptions{
		Dir: dir, Now: fixedClock(at), Check: check, Getenv: noEnv,
	})
	if err != nil {
		t.Fatalf("MaybeDailyCheck: %v", err)
	}
	if !result.Checked || result.Skipped != "" || result.Latest == nil || result.Latest.Version != "1.2.3" {
		t.Fatalf("result = %+v", result)
	}
	if calls != 1 {
		t.Fatalf("check calls = %d", calls)
	}
	state, ok := readDailyState(filepath.Join(dir, dailyStateName))
	if !ok || !state.LastCheckedAt.Equal(at) || !state.LastAttemptAt.Equal(at) ||
		state.LastVersion != "1.2.3" || state.LastChannel != "stable" {
		t.Fatalf("state = %+v, ok = %v", state, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, dailyStateName+dailyLockSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock not released: %v", err)
	}
}

func TestMaybeDailyCheckSkipsWithinInterval(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	calls := 0
	check := func(context.Context) (ports.ReleaseInfo, error) {
		calls++
		return ports.ReleaseInfo{Version: "1.2.3", Channel: "stable"}, nil
	}
	opts := DailyOptions{Dir: dir, Now: fixedClock(at), Check: check, Getenv: noEnv}
	if _, err := MaybeDailyCheck(context.Background(), opts); err != nil {
		t.Fatalf("first run: %v", err)
	}

	opts.Now = fixedClock(at.Add(23 * time.Hour))
	result, err := MaybeDailyCheck(context.Background(), opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if result.Checked || result.Skipped != "fresh" || calls != 1 {
		t.Fatalf("result = %+v, calls = %d", result, calls)
	}

	opts.Now = fixedClock(at.Add(25 * time.Hour))
	if _, err := MaybeDailyCheck(context.Background(), opts); err != nil {
		t.Fatalf("stale run: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls after interval = %d", calls)
	}
}

func TestMaybeDailyCheckHonorsDisableEnv(t *testing.T) {
	calls := 0
	check := func(context.Context) (ports.ReleaseInfo, error) {
		calls++
		return ports.ReleaseInfo{}, nil
	}
	result, err := MaybeDailyCheck(context.Background(), DailyOptions{
		Dir:    t.TempDir(),
		Check:  check,
		Getenv: func(string) string { return "1" },
	})
	if err != nil {
		t.Fatalf("MaybeDailyCheck: %v", err)
	}
	if result.Checked || result.Skipped != "disabled" || calls != 0 {
		t.Fatalf("result = %+v, calls = %d", result, calls)
	}
}

func TestMaybeDailyCheckLockContention(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	lockPath := filepath.Join(dir, dailyStateName+dailyLockSuffix)
	if err := os.WriteFile(lockPath, []byte("other process"), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	if err := os.Chtimes(lockPath, at.Add(-5*time.Minute), at.Add(-5*time.Minute)); err != nil {
		t.Fatalf("touch lock: %v", err)
	}
	calls := 0
	result, err := MaybeDailyCheck(context.Background(), DailyOptions{
		Dir: dir, Now: fixedClock(at), Getenv: noEnv,
		Check: func(context.Context) (ports.ReleaseInfo, error) {
			calls++
			return ports.ReleaseInfo{}, nil
		},
	})
	if err != nil {
		t.Fatalf("MaybeDailyCheck: %v", err)
	}
	if result.Checked || result.Skipped != "locked" || calls != 0 {
		t.Fatalf("result = %+v, calls = %d", result, calls)
	}
}

func TestMaybeDailyCheckRecoversStaleLock(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	lockPath := filepath.Join(dir, dailyStateName+dailyLockSuffix)
	if err := os.WriteFile(lockPath, []byte("crashed"), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	if err := os.Chtimes(lockPath, at.Add(-2*time.Hour), at.Add(-2*time.Hour)); err != nil {
		t.Fatalf("touch lock: %v", err)
	}
	calls := 0
	result, err := MaybeDailyCheck(context.Background(), DailyOptions{
		Dir: dir, Now: fixedClock(at), Getenv: noEnv,
		Check: func(context.Context) (ports.ReleaseInfo, error) {
			calls++
			return ports.ReleaseInfo{Version: "2.0.0"}, nil
		},
	})
	if err != nil {
		t.Fatalf("MaybeDailyCheck: %v", err)
	}
	if !result.Checked || calls != 1 {
		t.Fatalf("result = %+v, calls = %d", result, calls)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale lock not removed: %v", err)
	}
}

func TestMaybeDailyCheckFailureRecordsAttemptAndBacksOff(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	boom := errors.New("network down")
	calls := 0
	opts := DailyOptions{
		Dir: dir, Now: fixedClock(at), Getenv: noEnv,
		Check: func(context.Context) (ports.ReleaseInfo, error) {
			calls++
			return ports.ReleaseInfo{}, boom
		},
	}

	if _, err := MaybeDailyCheck(context.Background(), opts); !errors.Is(err, boom) {
		t.Fatalf("first run error = %v, want %v", err, boom)
	}
	statePath := filepath.Join(dir, dailyStateName)
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state after failure: %v", err)
	}
	if strings.Contains(string(raw), "network down") {
		t.Fatalf("state stores error text: %s", raw)
	}
	state, ok := readDailyState(statePath)
	if !ok || !state.LastAttemptAt.Equal(at) || !state.LastCheckedAt.IsZero() {
		t.Fatalf("state = %+v, ok = %v", state, ok)
	}
	if _, err := os.Stat(statePath + dailyLockSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock not released after failure: %v", err)
	}

	opts.Now = fixedClock(at.Add(29 * time.Minute))
	result, err := MaybeDailyCheck(context.Background(), opts)
	if err != nil {
		t.Fatalf("backoff run: %v", err)
	}
	if result.Checked || result.Skipped != "backoff" || calls != 1 {
		t.Fatalf("result = %+v, calls = %d", result, calls)
	}

	opts.Now = fixedClock(at.Add(31 * time.Minute))
	if _, err := MaybeDailyCheck(context.Background(), opts); !errors.Is(err, boom) {
		t.Fatalf("retry error = %v, want %v", err, boom)
	}
	if calls != 2 {
		t.Fatalf("calls after backoff = %d", calls)
	}
}

func TestMaybeDailyCheckFailureSurfacesStateWriteError(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	statePath := filepath.Join(dir, dailyStateName)
	if err := os.MkdirAll(statePath, 0o755); err != nil {
		t.Fatalf("prepare unwritable state path: %v", err)
	}
	boom := errors.New("network down")
	start := time.Now()
	_, err := MaybeDailyCheck(context.Background(), DailyOptions{
		Dir: dir, Now: fixedClock(at), Getenv: noEnv,
		Check: func(context.Context) (ports.ReleaseInfo, error) { return ports.ReleaseInfo{}, boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want %v", err, boom)
	}
	if !strings.Contains(err.Error(), "record update check attempt") {
		t.Fatalf("state write failure not surfaced: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("state write failure retried or blocked: %v", elapsed)
	}
}

func TestMaybeDailyCheckTimeout(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	_, err := MaybeDailyCheck(context.Background(), DailyOptions{
		Dir: dir, Getenv: noEnv, Timeout: 20 * time.Millisecond,
		Check: func(ctx context.Context) (ports.ReleaseInfo, error) {
			<-ctx.Done()
			return ports.ReleaseInfo{}, ctx.Err()
		},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout not honored: %v", elapsed)
	}
}

func TestMaybeDailyCheckFutureStateIsNotFresh(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	if err := writeDailyState(filepath.Join(dir, dailyStateName), dailyState{
		LastCheckedAt: at.Add(48 * time.Hour),
		LastVersion:   "9.9.9",
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}
	calls := 0
	result, err := MaybeDailyCheck(context.Background(), DailyOptions{
		Dir: dir, Now: fixedClock(at), Getenv: noEnv,
		Check: func(context.Context) (ports.ReleaseInfo, error) {
			calls++
			return ports.ReleaseInfo{Version: "1.1.1"}, nil
		},
	})
	if err != nil {
		t.Fatalf("MaybeDailyCheck: %v", err)
	}
	if !result.Checked || calls != 1 {
		t.Fatalf("result = %+v, calls = %d", result, calls)
	}
}

func TestMaybeDailyCheckFutureAttemptIsNotBackedOff(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	if err := writeDailyState(filepath.Join(dir, dailyStateName), dailyState{
		LastAttemptAt: at.Add(48 * time.Hour),
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}
	calls := 0
	result, err := MaybeDailyCheck(context.Background(), DailyOptions{
		Dir: dir, Now: fixedClock(at), Getenv: noEnv,
		Check: func(context.Context) (ports.ReleaseInfo, error) {
			calls++
			return ports.ReleaseInfo{Version: "1.1.1"}, nil
		},
	})
	if err != nil {
		t.Fatalf("MaybeDailyCheck: %v", err)
	}
	if !result.Checked || calls != 1 {
		t.Fatalf("result = %+v, calls = %d", result, calls)
	}
}

func TestMaybeDailyCheckRequiresCheckFunc(t *testing.T) {
	_, err := MaybeDailyCheck(context.Background(), DailyOptions{Dir: t.TempDir(), Getenv: noEnv})
	if err == nil {
		t.Fatal("expected error without check function")
	}
}
