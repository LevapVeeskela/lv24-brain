package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type testConnections struct {
	mu     sync.Mutex
	values []domain.Connection
}

func (s *testConnections) List(context.Context) ([]domain.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Connection(nil), s.values...), nil
}
func (s *testConnections) Get(context.Context, string) (domain.Connection, error) {
	return domain.Connection{}, domain.ErrNotFound
}
func (s *testConnections) Create(context.Context, domain.Connection) error { return nil }
func (s *testConnections) Update(context.Context, domain.Connection) error { return nil }
func (s *testConnections) Delete(context.Context, string) error            { return nil }

type runnerFunc func(context.Context, app.RunConnectionOptions) (app.RunConnectionResult, error)

func (f runnerFunc) RunConnection(ctx context.Context, opts app.RunConnectionOptions) (app.RunConnectionResult, error) {
	return f(ctx, opts)
}

type fixedClock struct {
	now   time.Time
	after chan time.Time
}

func (c *fixedClock) Now() time.Time                       { return c.now }
func (c *fixedClock) After(time.Duration) <-chan time.Time { return c.after }

func newTestAgent(t *testing.T, dir string, connections *testConnections, runner Runner, options ...func(*Options)) *Agent {
	t.Helper()
	opts := Options{DataDir: dir, Version: "test", Connections: connections, Runner: runner, Workers: 2, QueueSize: 8, LogMaxBytes: 256, LogRetention: 2}
	for _, apply := range options {
		apply(&opts)
	}
	agent, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.logFile.Close() })
	return agent
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestSingleInstanceStaleRuntimeRecoveryAndGracefulStop(t *testing.T) {
	dir := t.TempDir()
	conn := &testConnections{}
	if err := os.MkdirAll(filepath.Join(dir, "agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := Runtime{Port: 0, Token: "stale-token", PID: 987654, Version: "old", StartedAt: time.Now().Add(-time.Hour)}
	if err := writeJSONAtomic(filepath.Join(dir, "agent", "runtime.json"), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := runnerFunc(func(context.Context, app.RunConnectionOptions) (app.RunConnectionResult, error) {
		return app.RunConnectionResult{}, nil
	})
	a := newTestAgent(t, dir, conn, runner, func(o *Options) { o.PIDAlive = func(int) bool { return false } })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- a.Run(ctx) }()
	waitFor(t, func() bool {
		runtime, err := readRuntime(filepath.Join(dir, "agent", "runtime.json"))
		return err == nil && runtime.PID == os.Getpid() && runtime.Token != "stale-token"
	})
	second := newTestAgent(t, dir, conn, runner)
	if err := second.Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Run error = %v", err)
	}
	runtimeRecord, err := readRuntime(filepath.Join(dir, "agent", "runtime.json"))
	if err != nil || runtimeRecord.PID != os.Getpid() || runtimeRecord.Token == "stale-token" {
		t.Fatalf("runtime=%+v err=%v", runtimeRecord, err)
	}
	if runtimeRecord.Port != 0 || runtimeRecord.Version != "test" || runtimeRecord.StartedAt.IsZero() {
		t.Fatalf("runtime format incomplete: %+v", runtimeRecord)
	}
	if runtimeInfo, statErr := os.Stat(filepath.Join(dir, "agent", "runtime.json")); statErr != nil {
		t.Fatal(statErr)
	} else if runtime.GOOS != "windows" && runtimeInfo.Mode().Perm() != 0o600 {
		t.Fatalf("runtime permissions=%o", runtimeInfo.Mode().Perm())
	}
	status, err := ReadStatus(context.Background(), dir)
	if err != nil || !status.Running {
		t.Fatalf("ReadStatus=%+v err=%v", status, err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second*3)
	defer stopCancel()
	if err := a.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent", "runtime.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime remains: %v", err)
	}
	if err := StopProcess(context.Background(), filepath.Join(t.TempDir(), "empty")); err != nil {
		t.Fatalf("stop without agent: %v", err)
	}
}

func TestStopRequestWatcherRequestsCooperativeShutdown(t *testing.T) {
	dir := t.TempDir()
	a := newTestAgent(t, dir, &testConnections{}, runnerFunc(func(context.Context, app.RunConnectionOptions) (app.RunConnectionResult, error) {
		return app.RunConnectionResult{}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	waitFor(t, func() bool { status, err := ReadStatus(context.Background(), dir); return err == nil && status.Running })
	var runtimeRecord Runtime
	waitFor(t, func() bool {
		record, err := readRuntime(filepath.Join(dir, "agent", "runtime.json"))
		if err == nil {
			runtimeRecord = record
			return true
		}
		return false
	})
	if err := writeJSONAtomic(filepath.Join(dir, "agent", "stop.json"), stopRequest{PID: runtimeRecord.PID, Token: runtimeRecord.Token}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent", "runtime.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime remains: %v", err)
	}
}

func TestAgentDirectoryIsGitIgnored(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	if out, err := exec.Command(git, "-C", root, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	agent, err := New(Options{
		DataDir: root, Version: "test", Connections: &testConnections{},
		Runner: runnerFunc(func(context.Context, app.RunConnectionOptions) (app.RunConnectionResult, error) {
			return app.RunConnectionResult{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	out, err := exec.Command(git, "-C", root, "status", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, out)
	}
	if status := strings.TrimSpace(string(out)); status != "" {
		t.Fatalf("agent state is untracked: %s", status)
	}
}

func TestScheduleParsingJitterAndPersistedNextRun(t *testing.T) {
	for schedule, want := range map[string]time.Duration{"": 0, "manual": 0, "@hourly": time.Hour, "@daily": 24 * time.Hour, "@every 2m": 2 * time.Minute} {
		got, err := ParseSchedule(schedule)
		if err != nil || got != want {
			t.Errorf("ParseSchedule(%q)=%v,%v want %v", schedule, got, err, want)
		}
	}
	for _, schedule := range []string{"@every 59s", "@weekly", "@every nope"} {
		if _, err := ParseSchedule(schedule); err == nil {
			t.Errorf("ParseSchedule(%q) succeeded", schedule)
		}
	}
	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	jitter := DeterministicJitter("github", base, time.Minute)
	if jitter < 0 || jitter >= time.Minute || jitter != DeterministicJitter("github", base, time.Minute) {
		t.Fatalf("invalid/non-deterministic jitter: %v", jitter)
	}
	next, err := nextRun("github", "@every 2m", base)
	if err != nil || next.Sub(base) < 2*time.Minute || next.Sub(base) >= 2*time.Minute+6*time.Second {
		t.Fatalf("next=%v err=%v", next, err)
	}
	path := filepath.Join(t.TempDir(), "schedule.json")
	want := ScheduleState{Entries: map[string]ScheduleEntry{"github": {Schedule: "@every 2m", NextRun: next}}}
	if err := saveScheduleState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadScheduleState(path)
	if err != nil || !got.Entries["github"].NextRun.Equal(next) {
		t.Fatalf("loaded=%+v err=%v", got, err)
	}
}

func TestWorkerSerializesConnectionRunsDifferentConnectionsAndCancellation(t *testing.T) {
	dir := t.TempDir()
	started := make(chan string, 4)
	release := make(chan struct{})
	runner := runnerFunc(func(ctx context.Context, opts app.RunConnectionOptions) (app.RunConnectionResult, error) {
		started <- opts.ConnectionID
		select {
		case <-release:
			return app.RunConnectionResult{}, nil
		case <-ctx.Done():
			return app.RunConnectionResult{}, ctx.Err()
		}
	})
	a := newTestAgent(t, dir, &testConnections{}, runner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	waitFor(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.queue != nil })
	if _, err := a.QueueConnection(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if got := <-started; got != "alpha" {
		t.Fatalf("first runner=%s", got)
	}
	if _, err := a.QueueConnection(context.Background(), "alpha"); !errors.Is(err, ErrJobActive) {
		t.Fatalf("duplicate error=%v", err)
	}
	// A distinct connection starts on the second worker while alpha is blocked.
	if _, err := a.QueueConnection(context.Background(), "gamma"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if got != "gamma" {
			t.Fatalf("unexpected concurrent runner=%s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("different connection did not run concurrently")
	}
	jobID, err := a.QueueConnection(context.Background(), "beta")
	if err != nil {
		t.Fatal(err)
	}
	if !a.CancelJob(jobID) {
		t.Fatal("queued job not cancelable")
	}
	close(release)
	waitFor(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.active["alpha"] == "" && a.active["gamma"] == "" && a.active["beta"] == ""
	})
	select {
	case got := <-started:
		t.Fatalf("canceled queued job ran: %s", got)
	case <-time.After(30 * time.Millisecond):
	}
	stop, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	if err := a.Stop(stop); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPanicLoggedDoesNotKillAgentAndStatusOmitsToken(t *testing.T) {
	dir := t.TempDir()
	a := newTestAgent(t, dir, &testConnections{}, runnerFunc(func(context.Context, app.RunConnectionOptions) (app.RunConnectionResult, error) {
		panic("sensitive details")
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	waitFor(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.queue != nil })
	if _, err := a.QueueConnection(context.Background(), "panic-test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, job := range a.jobs {
			if job.view.Status == "failed" {
				return true
			}
		}
		return false
	})
	status, err := a.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), a.runtime.Token) || strings.Contains(string(raw), "token") {
		t.Fatalf("status leaked runtime token: %s", raw)
	}
	if !strings.Contains(string(raw), "job panicked") {
		t.Fatalf("panic not recorded: %s", raw)
	}
	stop, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	if err := a.Stop(stop); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestParentWatchdogAndSleepWakeCatchup(t *testing.T) {
	dir := t.TempDir()
	clock := &fixedClock{now: time.Now().UTC(), after: make(chan time.Time, 1)}
	connections := &testConnections{values: []domain.Connection{{ID: "due", Provider: "github", Schedule: "@every 1m"}}}
	runs := make(chan string, 2)
	a := newTestAgent(t, dir, connections, runnerFunc(func(_ context.Context, o app.RunConnectionOptions) (app.RunConnectionResult, error) {
		runs <- o.ConnectionID
		return app.RunConnectionResult{}, nil
	}), func(o *Options) { o.Clock = clock; o.PIDAlive = func(pid int) bool { return pid == os.Getpid() } })
	// An overdue persisted entry is run once, then advanced from wake time.
	state := ScheduleState{Version: scheduleStateVersion, Entries: map[string]ScheduleEntry{"due": {Schedule: "@every 1m", NextRun: clock.now.Add(-time.Hour)}}}
	if err := saveScheduleState(a.statePath, state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	waitFor(t, func() bool { a.mu.Lock(); queued := a.queue != nil; a.mu.Unlock(); return queued })
	select {
	case id := <-runs:
		if id != "due" {
			t.Fatalf("catch-up connection=%q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("missed sleep/wake catch-up")
	}
	updatedSchedule := func() bool {
		updated, loadErr := loadScheduleState(a.statePath)
		return loadErr == nil && updated.Entries["due"].NextRun.After(clock.now)
	}
	if !pollFor(3*time.Second, updatedSchedule) {
		logData, _ := os.ReadFile(a.logPath)
		stateData, _ := os.ReadFile(a.statePath)
		t.Fatalf("schedule did not advance: state=%s log=%s", stateData, logData)
	}
	state, err := loadScheduleState(a.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Entries["due"].NextRun.After(clock.now) {
		t.Fatalf("next run not advanced: %+v", state)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The watchdog hook is directly exercised, avoiding OS PID timing races.
	watchClock := &fixedClock{now: time.Now(), after: make(chan time.Time, 1)}
	watchClock.after <- time.Now()
	watch := newTestAgent(t, filepath.Join(t.TempDir(), "watch"), &testConnections{}, runnerFunc(func(context.Context, app.RunConnectionOptions) (app.RunConnectionResult, error) {
		return app.RunConnectionResult{}, nil
	}), func(o *Options) { o.ParentPID = 99; o.PIDAlive = func(int) bool { return false }; o.Clock = watchClock })
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	// Provide the watchdog with its cancellation target via the test hook.
	watch.mu.Lock()
	watch.cancel = watchCancel
	watch.mu.Unlock()
	watch.watchParent(watchCtx)
	if watchCtx.Err() == nil {
		t.Fatal("parent watchdog did not cancel agent")
	}
}

func pollFor(timeout time.Duration, check func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return check()
}

func TestLogRotationRetentionAndRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	logger, err := newRotatingLog(path, 70, 2)
	if err != nil {
		t.Fatal(err)
	}
	secret := "ghp_" + strings.Repeat("A", 36)
	for i := 0; i < 6; i++ {
		if _, err := logger.Write([]byte("event token=" + secret + " payload=abcdefghijklmnopqrstuvwxyz\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := LogFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) > 3 {
		t.Fatalf("retention kept %d files: %v", len(files), files)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("secret leaked to %s", file)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("log permissions=%o", info.Mode().Perm())
	}
}
