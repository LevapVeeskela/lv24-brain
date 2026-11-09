// Package daemon implements the foreground background-sync engine.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	defaultWorkers    = 2
	defaultQueueSize  = 32
	defaultLogBytes   = 4 << 20
	defaultLogFiles   = 4
	maxJobHistory     = 500
	defaultRunTimeout = 5 * time.Minute

	// agentIgnoreContent keeps runtime tokens, schedules, stop requests and
	// logs out of the data repository. The pattern also ignores this file.
	agentIgnoreContent = "# lv24-brain: private agent state; nothing in this directory is committed.\n*\n"
)

var ErrAlreadyRunning = errors.New("agent is already running")
var ErrJobActive = errors.New("connection already queued or running")

// Runner executes one configured connection. The composition root should
// adapt app.Ingestion; the daemon deliberately does not construct adapters.
type Runner interface {
	RunConnection(context.Context, app.RunConnectionOptions) (app.RunConnectionResult, error)
}

// Clock allows deterministic scheduling tests. After must return a channel
// that receives once when duration elapses.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Control is the authenticated loopback API started together with the agent.
// Start must bind its listener before returning the selected port; serving
// continues until ctx is canceled. Stop drains the listener on shutdown.
type Control interface {
	Start(ctx context.Context, runtime Runtime) (port int, err error)
	Stop(ctx context.Context) error
}

// Options configures an Agent. DataDir is the local data directory, Runner
// and Connections are supplied by the composition root. ParentPID <= 0
// disables the optional parent watchdog. Control, when set, is the local
// control API started after the runtime record is ready.
type Options struct {
	DataDir      string
	Version      string
	ParentPID    int
	Connections  ports.ConnectionStore
	Runner       Runner
	Control      Control
	Clock        Clock
	Workers      int
	QueueSize    int
	RunTimeout   time.Duration
	LogMaxBytes  int64
	LogRetention int
	PIDAlive     func(int) bool
}

// Runtime is the owner-only on-disk process rendezvous record. Token is never
// included in Status and is reserved for WP2.2's authenticated local API.
type Runtime struct {
	Port      int       `json:"port"`
	Token     string    `json:"token"`
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

// Status is intentionally safe to print: it contains no token or credentials.
type Status struct {
	Running   bool      `json:"running"`
	PID       int       `json:"pid,omitempty"`
	Port      int       `json:"port,omitempty"`
	Version   string    `json:"version,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Jobs      []JobView `json:"jobs,omitempty"`
}

// JobView is the credential-free public view of a scheduled/manual run.
type JobView struct {
	ID           string    `json:"id"`
	ConnectionID string    `json:"connectionId"`
	Status       string    `json:"status"`
	StartedAt    time.Time `json:"startedAt,omitempty"`
	FinishedAt   time.Time `json:"finishedAt,omitempty"`
	Error        string    `json:"error,omitempty"`
}

type Agent struct {
	opts        Options
	dataDir     string
	runtime     Runtime
	runtimePath string
	statePath   string
	stopPath    string
	logPath     string
	logger      *log.Logger
	logFile     *rotatingLog
	clock       Clock

	mu       sync.Mutex
	cancel   context.CancelFunc
	started  bool
	jobs     map[string]*job
	active   map[string]string
	queue    chan *job
	done     chan struct{}
	stopOnce sync.Once
	lock     heldLock
}

type job struct {
	view   JobView
	opts   app.RunConnectionOptions
	ctx    context.Context
	cancel context.CancelFunc
	run    domain.ConnectorRun
	hasRun bool
}

// New validates dependencies and creates the private agent state directory.
func New(opts Options) (*Agent, error) {
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, errors.New("daemon: data directory is required")
	}
	if opts.Connections == nil || opts.Runner == nil {
		return nil, errors.New("daemon: connections and runner are required")
	}
	root, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("daemon: resolve data directory: %w", err)
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultQueueSize
	}
	if opts.RunTimeout <= 0 {
		opts.RunTimeout = defaultRunTimeout
	}
	if opts.LogMaxBytes <= 0 {
		opts.LogMaxBytes = defaultLogBytes
	}
	if opts.LogRetention <= 0 {
		opts.LogRetention = defaultLogFiles
	}
	if opts.PIDAlive == nil {
		opts.PIDAlive = pidAlive
	}
	dir := filepath.Join(root, "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create agent directory: %w", err)
	}
	ignorePath := filepath.Join(dir, ".gitignore")
	if _, statErr := os.Stat(ignorePath); errors.Is(statErr, os.ErrNotExist) {
		if err := os.WriteFile(ignorePath, []byte(agentIgnoreContent), 0o600); err != nil {
			return nil, fmt.Errorf("daemon: write agent ignore file: %w", err)
		}
	} else if statErr != nil {
		return nil, fmt.Errorf("daemon: inspect agent ignore file: %w", statErr)
	}
	if canonical, err := filepath.EvalSymlinks(root); err == nil {
		root = canonical
		dir = filepath.Join(root, "agent")
	}
	if err := securePath(dir, 0o700, true); err != nil {
		return nil, fmt.Errorf("daemon: secure agent directory: %w", err)
	}
	logs, err := newRotatingLog(filepath.Join(dir, "agent.log"), opts.LogMaxBytes, opts.LogRetention)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		opts: opts, dataDir: root, runtimePath: filepath.Join(dir, "runtime.json"),
		statePath: filepath.Join(dir, "schedule.json"), stopPath: filepath.Join(dir, "stop.json"), logPath: filepath.Join(dir, "agent.log"),
		logFile: logs, logger: log.New(logs, "", log.LstdFlags|log.LUTC), clock: opts.Clock,
		jobs: make(map[string]*job), active: make(map[string]string), done: make(chan struct{}),
	}
	return a, nil
}

// Close releases the agent log file. Run closes it automatically; call Close
// when an Agent created with New is never run.
func (a *Agent) Close() error {
	if a == nil || a.logFile == nil {
		return nil
	}
	return a.logFile.Close()
}

// Run acquires the per-data-directory OS lock, writes the runtime record and
// runs workers/scheduler until ctx, Stop, or the parent watchdog requests exit.
func (a *Agent) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return errors.New("daemon: Run called more than once")
	}
	a.started = true
	a.mu.Unlock()
	defer close(a.done)
	defer func() { a.mu.Lock(); a.cancel = nil; a.mu.Unlock() }()
	defer a.logFile.Close()

	lock, err := acquireAgentLock(filepath.Join(a.dataDir, "agent", "agent.lock"))
	if err != nil {
		return fmt.Errorf("%w: %s", ErrAlreadyRunning, a.runtimePath)
	}
	a.lock = lock
	defer lock.Close()
	if err := a.recoverRuntime(); err != nil {
		return err
	}
	root, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()
	runtime, err := newRuntime(a.opts.Version, a.clock.Now())
	if err != nil {
		return err
	}
	if a.opts.Control != nil {
		port, err := a.opts.Control.Start(root, runtime)
		if err != nil {
			return fmt.Errorf("daemon: start local control API: %w", err)
		}
		runtime.Port = port
	}
	a.mu.Lock()
	a.runtime = runtime
	a.mu.Unlock()
	ownerPath := filepath.Join(a.dataDir, "agent", "owner.json")
	if err := writeJSONAtomic(ownerPath, lockOwner{PID: runtime.PID, Token: runtime.Token}, 0o600); err != nil {
		return fmt.Errorf("daemon: write lock owner: %w", err)
	}
	if err := writeJSONAtomic(a.runtimePath, runtime, 0o600); err != nil {
		_ = os.Remove(ownerPath)
		return fmt.Errorf("daemon: write runtime: %w", err)
	}
	a.logger.Printf("agent started pid=%d version=%s", runtime.PID, runtime.Version)

	a.mu.Lock()
	a.queue = make(chan *job, a.opts.QueueSize)
	a.mu.Unlock()
	var workers sync.WaitGroup
	for i := 0; i < a.opts.Workers; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); a.worker(root) }()
	}
	var tasks sync.WaitGroup
	tasks.Add(1)
	go func() { defer tasks.Done(); a.scheduler(root) }()
	if a.opts.ParentPID > 0 {
		tasks.Add(1)
		go func() { defer tasks.Done(); a.watchParent(root) }()
	}
	tasks.Add(1)
	go func() { defer tasks.Done(); a.watchStop(root) }()
	<-root.Done()
	// Cancel queued jobs and signal active jobs before joining workers.
	a.cancelAllJobs()
	workers.Wait()
	tasks.Wait()
	if a.opts.Control != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := a.opts.Control.Stop(stopCtx); err != nil {
			a.logger.Printf("local control API stop failed: %v", err)
		}
		stopCancel()
	}
	a.removeRuntime()
	a.logger.Printf("agent stopped")
	return nil
}

func newRuntime(version string, now time.Time) (Runtime, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return Runtime{}, fmt.Errorf("daemon: generate runtime token: %w", err)
	}
	return Runtime{Port: 0, Token: hex.EncodeToString(secret), PID: os.Getpid(), Version: version, StartedAt: now.UTC()}, nil
}

func (a *Agent) recoverRuntime() error {
	_, err := readRuntime(a.runtimePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("daemon: inspect runtime: %w", err)
	}
	// The OS lock is held exclusively here, so no live agent for this data
	// directory can own the record. A stale PID may have been reused by an
	// unrelated process and is deliberately not used as an ownership signal.
	if err := os.Remove(a.runtimePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon: remove stale runtime: %w", err)
	}
	return nil
}

func (a *Agent) removeRuntime() {
	a.mu.Lock()
	ownedRuntime := a.runtime
	a.mu.Unlock()
	runtime, err := readRuntime(a.runtimePath)
	if err == nil && runtime.PID == os.Getpid() && runtime.Token == ownedRuntime.Token {
		if err := os.Remove(a.runtimePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.logger.Printf("remove runtime failed: %v", err)
		}
		ownerPath := filepath.Join(a.dataDir, "agent", "owner.json")
		if owner, readErr := readLockOwner(ownerPath); readErr == nil && owner.PID == os.Getpid() && owner.Token == ownedRuntime.Token {
			_ = os.Remove(ownerPath)
		}
		if request, readErr := readStopRequest(a.stopPath); readErr == nil && request.Token == ownedRuntime.Token {
			_ = os.Remove(a.stopPath)
		}
	}
}

func (a *Agent) cancelAllJobs() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, queued := range a.jobs {
		if queued.cancel != nil {
			queued.cancel()
		}
	}
}

// QueueConnection adds a run to the bounded job queue and returns a job ID.
// The same connection cannot be queued or executing twice in this agent.
func (a *Agent) QueueConnection(ctx context.Context, connectionID string) (string, error) {
	return a.QueueConnectionOptions(ctx, connectionID, app.RunConnectionOptions{})
}

// QueueConnectionOptions queues a run with bounded read options (since,
// limit, window). The connection ID in opts is overwritten by connectionID.
func (a *Agent) QueueConnectionOptions(ctx context.Context, connectionID string, opts app.RunConnectionOptions) (string, error) {
	if err := domain.ValidateConnectionID("connection.id", connectionID); err != nil {
		return "", err
	}
	if opts.Limit < 0 || opts.MaxBatches < 0 || opts.InitialWindow < 0 {
		return "", domain.NewValidationError("run.options", "limit, maxBatches and initialWindow must not be negative")
	}
	opts.ConnectionID = connectionID
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	if !a.started || a.queue == nil {
		a.mu.Unlock()
		return "", errors.New("daemon: agent is not running")
	}
	if _, exists := a.active[connectionID]; exists {
		a.mu.Unlock()
		return "", ErrJobActive
	}
	jobID := fmt.Sprintf("%s-%d", connectionID, a.clock.Now().UnixNano())
	jobCtx, cancel := context.WithCancel(ctx)
	j := &job{view: JobView{ID: jobID, ConnectionID: connectionID, Status: "queued"}, opts: opts, ctx: jobCtx, cancel: cancel}
	a.jobs[jobID] = j
	a.active[connectionID] = jobID
	queue := a.queue
	a.mu.Unlock()
	select {
	case queue <- j:
		a.logger.Printf("job queued connection=%s job=%s", connectionID, jobID)
		return jobID, nil
	case <-ctx.Done():
		cancel()
		a.finishJob(jobID, "canceled", "")
		return "", ctx.Err()
	default:
		cancel()
		a.finishJob(jobID, "rejected", "queue is full")
		return "", errors.New("daemon: job queue is full")
	}
}

// CancelJob cancels a queued or running job. A queued canceled job is skipped
// without invoking Runner when a worker dequeues it.
func (a *Agent) CancelJob(jobID string) bool {
	a.mu.Lock()
	j, ok := a.jobs[jobID]
	if !ok {
		a.mu.Unlock()
		return false
	}
	j.cancel()
	a.mu.Unlock()
	return true
}

func (a *Agent) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-a.queue:
			if j == nil {
				continue
			}
			a.runJob(ctx, j)
		}
	}
}

func (a *Agent) runJob(agentCtx context.Context, j *job) {
	a.mu.Lock()
	if j.view.Status != "queued" {
		a.mu.Unlock()
		return
	}
	if j.ctx.Err() != nil || agentCtx.Err() != nil {
		a.mu.Unlock()
		a.finishJob(j.view.ID, "canceled", "")
		return
	}
	jobCtx, cancel := context.WithTimeout(j.ctx, a.opts.RunTimeout)
	stopAgentCancel := context.AfterFunc(agentCtx, cancel)
	// Explicit CancelJob now cancels the context actually passed to Runner.
	j.cancel = cancel
	j.view.Status, j.view.StartedAt = "running", a.clock.Now().UTC()
	a.mu.Unlock()

	status, message := "success", ""
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				status = "failed"
				message = "job panicked"
				a.logger.Printf("job panic connection=%s job=%s", j.view.ConnectionID, j.view.ID)
			}
		}()
		result, err := a.opts.Runner.RunConnection(jobCtx, j.opts)
		if result.Run.ID != "" {
			a.mu.Lock()
			j.run, j.hasRun = result.Run, true
			a.mu.Unlock()
		}
		if err != nil {
			status = "failed"
			message = "connection run failed"
		} else if jobCtx.Err() != nil {
			status = "canceled"
		} else if result.Busy {
			status = "busy"
		} else if result.Run.Status != "" {
			status = string(result.Run.Status)
			if result.Run.Error != "" {
				message = "connection run completed with an error"
			}
		}
	}()
	stopAgentCancel()
	cancel()
	a.finishJob(j.view.ID, status, message)
}

func (a *Agent) finishJob(id, status, message string) {
	message, _ = app.RedactIngest(message)
	a.mu.Lock()
	j, ok := a.jobs[id]
	if !ok {
		a.mu.Unlock()
		return
	}
	j.view.Status, j.view.Error, j.view.FinishedAt = status, message, a.clock.Now().UTC()
	delete(a.active, j.view.ConnectionID)
	if len(a.jobs) > maxJobHistory {
		var oldestID string
		var oldest time.Time
		for jobID, candidate := range a.jobs {
			if candidate.view.Status == "queued" || candidate.view.Status == "running" {
				continue
			}
			if oldestID == "" || candidate.view.FinishedAt.Before(oldest) {
				oldestID, oldest = jobID, candidate.view.FinishedAt
			}
		}
		if oldestID != "" {
			delete(a.jobs, oldestID)
		}
	}
	a.mu.Unlock()
	a.logger.Printf("job finished connection=%s job=%s status=%s error=%s", j.view.ConnectionID, id, status, message)
}

// Status returns a redacted snapshot of the process and recent in-memory jobs.
func (a *Agent) Status(ctx context.Context) (Status, error) {
	if err := ctxErr(ctx); err != nil {
		return Status{}, err
	}
	a.mu.Lock()
	status := Status{Running: a.started && a.cancel != nil, PID: a.runtime.PID, Port: a.runtime.Port, Version: a.runtime.Version, StartedAt: a.runtime.StartedAt}
	for _, j := range a.jobs {
		status.Jobs = append(status.Jobs, j.view)
	}
	a.mu.Unlock()
	sort.Slice(status.Jobs, func(i, j int) bool { return status.Jobs[i].ID < status.Jobs[j].ID })
	return status, nil
}

// JobDetail is one job with its connector run record, once the runner
// produced one. The record is credential-free and redacted by the ingestion
// host.
type JobDetail struct {
	View   JobView
	Run    domain.ConnectorRun
	HasRun bool
}

// JobDetails returns one job and its run record, if the job is known.
func (a *Agent) JobDetails(jobID string) (JobDetail, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j, ok := a.jobs[jobID]
	if !ok {
		return JobDetail{}, false
	}
	return JobDetail{View: j.view, Run: j.run, HasRun: j.hasRun}, true
}

// RecentJobs returns a snapshot of in-memory jobs ordered by start time and
// ID (newest last), bounded by maxJobHistory.
func (a *Agent) RecentJobs() []JobDetail {
	a.mu.Lock()
	out := make([]JobDetail, 0, len(a.jobs))
	for _, j := range a.jobs {
		out = append(out, JobDetail{View: j.view, Run: j.run, HasRun: j.hasRun})
	}
	a.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].View.StartedAt.Equal(out[j].View.StartedAt) {
			return out[i].View.ID < out[j].View.ID
		}
		return out[i].View.StartedAt.Before(out[j].View.StartedAt)
	})
	return out
}

// Stop requests graceful shutdown and waits until Run exits or ctx expires.
func (a *Agent) Stop(ctx context.Context) error {
	a.mu.Lock()
	cancel := a.cancel
	started := a.started
	a.mu.Unlock()
	if !started || cancel == nil {
		return nil
	}
	a.stopOnce.Do(cancel)
	select {
	case <-a.done:
		return nil
	case <-ctxDone(ctx):
		return ctxErr(ctx)
	}
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return context.Background().Done()
	}
	return ctx.Done()
}

func readRuntime(path string) (Runtime, error) {
	var runtime Runtime
	raw, err := os.ReadFile(path)
	if err != nil {
		return runtime, err
	}
	if err := json.Unmarshal(raw, &runtime); err != nil {
		return runtime, err
	}
	if runtime.PID <= 0 || runtime.Token == "" {
		return Runtime{}, errors.New("daemon: invalid runtime record")
	}
	return runtime, nil
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := securePath(tmpName, mode, false); err != nil {
		return err
	}
	if err := replaceFile(tmpName, path); err != nil {
		return err
	}
	return nil
}

// ReadStatus reads the runtime file without exposing its token.
func ReadStatus(ctx context.Context, dataDir string) (Status, error) {
	if err := ctxErr(ctx); err != nil {
		return Status{}, err
	}
	path := filepath.Join(dataDir, "agent", "runtime.json")
	runtime, err := readRuntime(path)
	if errors.Is(err, os.ErrNotExist) {
		return Status{Running: false}, nil
	}
	if err != nil {
		return Status{}, err
	}
	owned, err := runtimeOwnsLock(dataDir, runtime)
	if err != nil {
		return Status{}, err
	}
	if !owned || !pidAlive(runtime.PID) || validateBrainProcess(runtime.PID) != nil {
		return Status{Running: false}, nil
	}
	return Status{Running: true, PID: runtime.PID, Port: runtime.Port, Version: runtime.Version, StartedAt: runtime.StartedAt}, nil
}

// StopProcess validates that runtime PID belongs to this brain executable,
// then requests shutdown. A missing/stale agent is a safe no-op.
func StopProcess(ctx context.Context, dataDir string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	path := filepath.Join(dataDir, "agent", "runtime.json")
	runtime, err := readRuntime(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	owned, err := runtimeOwnsLock(dataDir, runtime)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	if !pidAlive(runtime.PID) {
		return nil
	}
	if err := validateBrainProcess(runtime.PID); err != nil {
		return fmt.Errorf("daemon: refusing to stop pid %d: %w", runtime.PID, err)
	}
	request := stopRequest{PID: runtime.PID, Token: runtime.Token}
	if err := writeJSONAtomic(filepath.Join(dataDir, "agent", "stop.json"), request, 0o600); err != nil {
		return fmt.Errorf("daemon: request agent shutdown: %w", err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for pidAlive(runtime.PID) {
		if err := ctxErr(ctx); err != nil {
			return err
		}
		if err := validateBrainProcess(runtime.PID); err != nil {
			return nil
		}
		select {
		case <-ctxDone(ctx):
			return ctxErr(ctx)
		case <-ticker.C:
		}
	}
	return nil
}

type stopRequest struct {
	PID   int    `json:"pid"`
	Token string `json:"token"`
}

func readStopRequest(path string) (stopRequest, error) {
	var request stopRequest
	raw, err := os.ReadFile(path)
	if err != nil {
		return request, err
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return request, err
	}
	if request.PID <= 0 || request.Token == "" {
		return stopRequest{}, errors.New("daemon: invalid stop request")
	}
	return request, nil
}

func (a *Agent) watchStop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
			request, err := readStopRequest(a.stopPath)
			if err != nil {
				continue
			}
			if request.PID != a.runtime.PID || request.Token != a.runtime.Token {
				continue
			}
			a.logger.Printf("graceful stop requested")
			a.mu.Lock()
			cancel := a.cancel
			a.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			return
		}
	}
}

type lockOwner struct {
	PID   int    `json:"pid"`
	Token string `json:"token"`
}

func runtimeOwnsLock(dataDir string, runtime Runtime) (bool, error) {
	lockPath := filepath.Join(dataDir, "agent", "agent.lock")
	lock, err := acquireAgentLock(lockPath)
	if err == nil {
		// No process currently holds the lock; a live PID alone is not enough
		// to establish that it is our background engine. Keep the lock while
		// cleaning stale files so a new owner cannot race the removal.
		current, readErr := readRuntime(filepath.Join(dataDir, "agent", "runtime.json"))
		if readErr == nil && current.Token == runtime.Token {
			_ = os.Remove(filepath.Join(dataDir, "agent", "runtime.json"))
			_ = os.Remove(filepath.Join(dataDir, "agent", "owner.json"))
			_ = os.Remove(filepath.Join(dataDir, "agent", "stop.json"))
		}
		return false, lock.Close()
	}
	if !errors.Is(err, errAgentLockBusy) {
		return false, err
	}
	owner, err := readLockOwner(filepath.Join(dataDir, "agent", "owner.json"))
	if err != nil {
		return false, err
	}
	return owner.PID == runtime.PID && owner.Token != "" && owner.Token == runtime.Token, nil
}

func readLockOwner(path string) (lockOwner, error) {
	var owner lockOwner
	raw, err := os.ReadFile(path)
	if err != nil {
		return owner, err
	}
	if err := json.Unmarshal(raw, &owner); err != nil {
		return owner, err
	}
	if owner.PID <= 0 || owner.Token == "" {
		return lockOwner{}, errors.New("daemon: invalid agent lock owner")
	}
	return owner, nil
}

func (a *Agent) watchParent(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.clock.After(time.Second):
			if !a.opts.PIDAlive(a.opts.ParentPID) {
				a.logger.Printf("parent process exited pid=%d", a.opts.ParentPID)
				a.mu.Lock()
				cancel := a.cancel
				a.mu.Unlock()
				if cancel != nil {
					cancel()
				}
				return
			}
		}
	}
}
