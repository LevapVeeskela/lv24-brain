// Package agentcontrol wires the daemon's loopback control API to the local
// catalogues, source registry and run history. It is the composition layer
// between internal/daemon and internal/adapters/localapi; neither package
// imports the other's concrete wiring.
package agentcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/localapi"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/daemon"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Projects is the read-only project view exposed by the local API.
type Projects interface {
	ListProjects(context.Context) ([]domain.Project, error)
}

// Updater is the verified engine updater with the running build identity.
type Updater interface {
	ports.Updater
	CurrentVersion() string
}

// Options are the local/git services the control API exposes. Secrets are
// only written through the credential setter; they are never returned.
type Options struct {
	DataDir     string
	Connections ports.ConnectionStore
	Secrets     ports.SecretStore
	Registry    *connectorbuild.Registry
	Deps        connectorbuild.Deps
	Runs        ports.ConnectorRunStore
	Projects    Projects
	Updater     Updater
	Version     string
	Now         func() time.Time
	Agent       *daemon.Agent
}

// Control owns the loopback server for one agent process.
type Control struct {
	opts     Options
	settings *settingsStore

	mu     sync.Mutex
	server *localapi.Server
	done   chan struct{}
}

// New validates the control dependencies and prepares the private settings
// store. BindAgent must be called before the daemon starts.
func New(opts Options) (*Control, error) {
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, errors.New("agentcontrol: data directory is required")
	}
	if opts.Connections == nil || opts.Secrets == nil || opts.Registry == nil {
		return nil, errors.New("agentcontrol: connections, secrets and registry are required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := os.MkdirAll(filepath.Join(opts.DataDir, "agent"), 0o700); err != nil {
		return nil, fmt.Errorf("agentcontrol: create agent directory: %w", err)
	}
	return &Control{
		opts:     opts,
		settings: &settingsStore{path: filepath.Join(opts.DataDir, "agent", "settings.json")},
	}, nil
}

// BindAgent attaches the running agent used for run jobs and logs.
func (c *Control) BindAgent(agent *daemon.Agent) {
	c.mu.Lock()
	c.opts.Agent = agent
	c.mu.Unlock()
}

// Start implements daemon.Control: it binds an ephemeral loopback port and
// serves until ctx is canceled.
func (c *Control) Start(ctx context.Context, runtime daemon.Runtime) (int, error) {
	c.mu.Lock()
	if c.opts.Agent == nil {
		c.mu.Unlock()
		return 0, errors.New("agentcontrol: agent is not bound")
	}
	if c.server != nil {
		port := c.server.Port()
		c.mu.Unlock()
		return port, nil
	}
	agent := c.opts.Agent
	server, err := localapi.NewServer(localapi.Options{
		Token:       localapi.TokenFunc(func() string { return runtime.Token }),
		Port:        0,
		Version:     c.opts.Version,
		PID:         runtime.PID,
		StartedAt:   runtime.StartedAt,
		Now:         c.opts.Now,
		Heartbeat:   15 * time.Second,
		Status:      "running",
		Connections: c.opts.Connections,
		Sources: sourceRegistry{
			registry: c.opts.Registry,
			deps:     c.opts.Deps,
		},
		Credentials: credentialSetter{secrets: c.opts.Secrets},
		Jobs:        jobQueue{agent: agent, now: c.opts.Now},
		Runs:        runLister{agent: agent, store: c.opts.Runs},
		Schedules:   scheduleStore{connections: c.opts.Connections},
		Projects:    projectLister{projects: c.opts.Projects},
		Settings:    c.settings,
		Updater:     updater{updater: c.opts.Updater},
		Shutdown: localapi.ShutdownFunc(func(context.Context) error {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return agent.Stop(stopCtx)
		}),
	})
	if err != nil {
		c.mu.Unlock()
		return 0, err
	}
	c.server = server
	c.done = make(chan struct{})
	done := c.done
	c.mu.Unlock()

	go func() {
		defer close(done)
		_ = server.Serve(ctx)
	}()
	return server.Port(), nil
}

// Stop implements daemon.Control: it drains the listener and waits for the
// serving goroutine.
func (c *Control) Stop(ctx context.Context) error {
	c.mu.Lock()
	server, done := c.server, c.done
	c.mu.Unlock()
	if server == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := server.Shutdown(shutdownCtx)
	cancel()
	if err != nil {
		// SSE or a slow client may keep a request open; force the listener
		// closed so agent shutdown is not delayed by the local API.
		_ = server.HTTP.Close()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return err
}

// Port reports the bound port, or zero before Start.
func (c *Control) Port() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.server == nil {
		return 0
	}
	return c.server.Port()
}

// sourceRegistry adapts connectorbuild to the local API's checker contract.
type sourceRegistry struct {
	registry *connectorbuild.Registry
	deps     connectorbuild.Deps
}

func (s sourceRegistry) Descriptor(provider string) (domain.SourceDescriptor, error) {
	entry, err := s.registry.Lookup(provider)
	if err != nil {
		return domain.SourceDescriptor{}, err
	}
	return entry.Descriptor, nil
}

func (s sourceRegistry) Checker(ctx context.Context, conn domain.Connection) (localapi.SourceChecker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, err := s.registry.Build(conn, s.deps)
	if err != nil {
		return nil, err
	}
	return sourceChecker{source: source}, nil
}

type sourceChecker struct{ source ports.Source }

func (s sourceChecker) Check(ctx context.Context) error { return s.source.Check(ctx) }

// credentialSetter writes conn/<id>/<field> secrets; the handler validates
// the field against the provider descriptor first.
type credentialSetter struct{ secrets ports.SecretStore }

func (s credentialSetter) Set(ctx context.Context, connectionID, field, value string) error {
	if strings.TrimSpace(value) == "" {
		return domain.NewValidationError("credential.value", "must not be empty")
	}
	key, err := domain.CredentialKey(connectionID, field)
	if err != nil {
		return err
	}
	return s.secrets.Set(ctx, key, value)
}

// jobQueue adapts the daemon queue to the local API's run options.
type jobQueue struct {
	agent *daemon.Agent
	now   func() time.Time
}

func (q jobQueue) Queue(ctx context.Context, connectionID string, opts localapi.RunOptions) (string, error) {
	runOpts := app.RunConnectionOptions{Limit: opts.Limit}
	switch {
	case opts.Days > 0 && opts.Since != nil:
		return "", domain.NewValidationError("run.options", "since and days cannot be combined")
	case opts.Since != nil:
		runOpts.Since = opts.Since
	case opts.Days > 0:
		since := q.now().UTC().AddDate(0, 0, -opts.Days)
		runOpts.Since = &since
	}
	jobID, err := q.agent.QueueConnectionOptions(ctx, connectionID, runOpts)
	if errors.Is(err, daemon.ErrJobActive) {
		return "", fmt.Errorf("%w: connection is already queued or running", domain.ErrConflict)
	}
	if err != nil {
		var validation *domain.ValidationError
		if errors.As(err, &validation) {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", domain.ErrConflict, err)
	}
	return jobID, nil
}

// runLister merges in-memory jobs with the persisted run history. Job IDs are
// valid run IDs while the process lives, so the desktop can tail a run it
// just queued.
type runLister struct {
	agent *daemon.Agent
	store ports.ConnectorRunStore
}

func (l runLister) ListRuns(ctx context.Context, limit int) ([]domain.ConnectorRun, error) {
	var persisted []domain.ConnectorRun
	if l.store != nil {
		runs, err := l.store.ListConnectorRuns(ctx, "", limit)
		if err != nil {
			return nil, err
		}
		persisted = runs
	}
	out := make([]domain.ConnectorRun, 0, len(persisted)+8)
	seen := make(map[string]bool, len(persisted))
	jobs := l.agent.RecentJobs()
	for i := len(jobs) - 1; i >= 0; i-- {
		run, ok := runFromJob(jobs[i])
		if !ok {
			continue
		}
		seen[run.ID] = true
		run.ID = jobs[i].View.ID
		out = append(out, run)
	}
	for _, run := range persisted {
		if !seen[run.ID] {
			out = append(out, run)
			seen[run.ID] = true
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (l runLister) GetRun(ctx context.Context, id string) (domain.ConnectorRun, error) {
	if detail, ok := l.agent.JobDetails(id); ok {
		if run, ok := runFromJob(detail); ok {
			run.ID = detail.View.ID
			return run, nil
		}
	}
	if l.store != nil {
		runs, err := l.store.ListConnectorRuns(ctx, "", 500)
		if err != nil {
			return domain.ConnectorRun{}, err
		}
		for _, run := range runs {
			if run.ID == id {
				return run, nil
			}
		}
	}
	return domain.ConnectorRun{}, domain.ErrNotFound
}

func (l runLister) OpenRunLog(ctx context.Context, id string) (localapi.RunLog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lines := make([]string, 0, 8)
	if detail, ok := l.agent.JobDetails(id); ok {
		lines = append(lines, fmt.Sprintf("job %s connection=%s status=%s", detail.View.ID, detail.View.ConnectionID, detail.View.Status))
		if !detail.View.StartedAt.IsZero() {
			lines = append(lines, fmt.Sprintf("started_at=%s", detail.View.StartedAt.UTC().Format(time.RFC3339)))
		}
		if !detail.View.FinishedAt.IsZero() {
			lines = append(lines, fmt.Sprintf("finished_at=%s", detail.View.FinishedAt.UTC().Format(time.RFC3339)))
		}
		if detail.View.Error != "" {
			lines = append(lines, "error="+detail.View.Error)
		}
		if detail.HasRun {
			lines = append(lines, runSummary(detail.Run)...)
		}
		return newRunLog(lines), nil
	}
	if l.store != nil {
		runs, err := l.store.ListConnectorRuns(ctx, "", 500)
		if err != nil {
			return nil, err
		}
		for _, run := range runs {
			if run.ID == id {
				return newRunLog(runSummary(run)), nil
			}
		}
	}
	return nil, domain.ErrNotFound
}

func runFromJob(detail daemon.JobDetail) (domain.ConnectorRun, bool) {
	if detail.HasRun {
		return detail.Run, true
	}
	if detail.View.Status != "queued" && detail.View.Status != "running" {
		return domain.ConnectorRun{}, false
	}
	run := domain.ConnectorRun{
		ID: detail.View.ID, ConnectionID: detail.View.ConnectionID,
		StartedAt: detail.View.StartedAt, Status: domain.ConnectorRunning,
	}
	return run, true
}

func runSummary(run domain.ConnectorRun) []string {
	lines := []string{
		fmt.Sprintf("run %s source=%s connection=%s status=%s", run.ID, run.Source, run.ConnectionID, run.Status),
		"started_at=" + run.StartedAt.UTC().Format(time.RFC3339),
	}
	if run.FinishedAt != nil {
		lines = append(lines, "finished_at="+run.FinishedAt.UTC().Format(time.RFC3339))
	}
	if run.Stats != nil {
		lines = append(lines, fmt.Sprintf("stats seen=%d upserted=%d unchanged=%d deleted=%d skipped=%d",
			run.Stats.Seen, run.Stats.Upserted, run.Stats.Unchanged, run.Stats.Deleted, run.Stats.Skipped))
	}
	if run.Error != "" {
		lines = append(lines, "error="+run.Error)
	}
	return lines
}

// runLog is a finite, already-redacted SSE producer.
type runLog struct {
	ch chan localapi.LogLine
}

func newRunLog(lines []string) *runLog {
	log := &runLog{ch: make(chan localapi.LogLine, len(lines))}
	for _, line := range lines {
		log.ch <- localapi.LogLine{Text: line}
	}
	close(log.ch)
	return log
}

func (l *runLog) Lines() <-chan localapi.LogLine { return l.ch }
func (l *runLog) Close() error                   { return nil }

// scheduleStore reads schedules from connection records and updates the
// catalogue; the daemon scheduler refreshes its state every minute.
type scheduleStore struct{ connections ports.ConnectionStore }

func (s scheduleStore) ListSchedules(ctx context.Context) ([]localapi.Schedule, error) {
	connections, err := s.connections.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]localapi.Schedule, 0, len(connections))
	for _, connection := range connections {
		if connection.Schedule == "" {
			continue
		}
		out = append(out, localapi.Schedule{ConnectionID: connection.ID, Expression: connection.Schedule})
	}
	return out, nil
}

func (s scheduleStore) SetSchedule(ctx context.Context, connectionID, expression string) error {
	connection, err := s.connections.Get(ctx, connectionID)
	if err != nil {
		return err
	}
	connection.Schedule = expression
	return s.connections.Update(ctx, connection)
}

// projectLister exposes id/name only.
type projectLister struct{ projects Projects }

func (p projectLister) ListProjects(ctx context.Context) ([]localapi.Project, error) {
	if p.projects == nil {
		return nil, nil
	}
	projects, err := p.projects.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]localapi.Project, 0, len(projects))
	for _, project := range projects {
		out = append(out, localapi.Project{ID: project.ID, Name: project.Name})
	}
	return out, nil
}

// settingsStore persists non-secret UI settings in agent/settings.json.
type settingsStore struct {
	path string
	mu   sync.Mutex
}

func (s *settingsStore) GetSettings(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *settingsStore) PutSettings(ctx context.Context, values map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	settings, err := s.load()
	if err != nil {
		return err
	}
	for key, value := range values {
		settings[key] = value
	}
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".settings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
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
	return os.Rename(tmpName, s.path)
}

func (s *settingsStore) load() (map[string]string, error) {
	settings := map[string]string{}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, errors.New("agentcontrol: settings file is invalid")
	}
	if settings == nil {
		settings = map[string]string{}
	}
	return settings, nil
}

// updater applies a verified engine update after explicit confirmation.
type updater struct{ updater Updater }

func (u updater) Update(ctx context.Context) (localapi.UpdateResult, error) {
	if u.updater == nil {
		return localapi.UpdateResult{}, errors.New("update is unavailable")
	}
	release, err := u.updater.Check(ctx, "")
	if err != nil {
		return localapi.UpdateResult{}, err
	}
	result := localapi.UpdateResult{Available: true, Version: release.Version}
	if release.Version == u.updater.CurrentVersion() {
		result.Available = false
		return result, nil
	}
	artifact, err := u.updater.Download(ctx, release)
	if err != nil {
		return localapi.UpdateResult{}, err
	}
	if err := u.updater.Apply(ctx, artifact); err != nil {
		return localapi.UpdateResult{}, err
	}
	result.Applied = true
	return result, nil
}
