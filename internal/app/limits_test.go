package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// limitsFixture is a local app with a real Markdown store, a SQLite index and
// the configured write limits. The project "p" is registered up front.
type limitsFixture struct {
	app     *app.App
	store   *fsstore.Store
	index   *sqliteindex.Index
	dataDir string
}

func newLimitsFixture(t *testing.T, limits config.LimitsConfig) *limitsFixture {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	cfg.Limits = limits
	ctx := context.Background()
	for _, id := range []string{"p"} {
		if err := store.PutProject(ctx, domain.Project{ID: id, Name: id, Kind: domain.ProjectKindRepository}); err != nil {
			t.Fatal(err)
		}
	}
	return &limitsFixture{
		app:     app.New(cfg, app.Dependencies{Store: store, Index: index}),
		store:   store,
		index:   index,
		dataDir: dataDir,
	}
}

func limitError(t *testing.T, err error) *domain.LimitError {
	t.Helper()
	var limitErr *domain.LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("error = %v, want *domain.LimitError", err)
	}
	return limitErr
}

func protectedError(t *testing.T, err error) *domain.ProtectedError {
	t.Helper()
	var protectedErr *domain.ProtectedError
	if !errors.As(err, &protectedErr) {
		t.Fatalf("error = %v, want *domain.ProtectedError", err)
	}
	return protectedErr
}

func anyRecordFiles(t *testing.T, dataDir, projectID string) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{"notes", "decisions", "attempts", "handoffs"} {
		found, err := filepath.Glob(filepath.Join(dataDir, "projects", projectID, dir, "*.md"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, found...)
	}
	return files
}

func TestCreateBodyLimitOnEveryKind(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		write func(*app.App) error
		field string
	}{
		{
			name: "note",
			write: func(a *app.App) error {
				_, err := a.Note(ctx, app.NoteInput{ProjectID: "p", Title: "n", Body: "0123456789"})
				return err
			},
			field: "note.body",
		},
		{
			name: "decision",
			write: func(a *app.App) error {
				_, err := a.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "d", Context: "0123456789"})
				return err
			},
			field: "decision.context",
		},
		{
			name: "attempt",
			write: func(a *app.App) error {
				_, err := a.Attempt(ctx, app.AttemptInput{ProjectID: "p", Title: "a", Hypothesis: "0123456789"})
				return err
			},
			field: "attempt.body",
		},
		{
			name: "handoff",
			write: func(a *app.App) error {
				_, err := a.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "0123456789"})
				return err
			},
			field: "handoff.body",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newLimitsFixture(t, config.LimitsConfig{MaxBodyBytes: 8})
			limitErr := limitError(t, tc.write(fixture.app))
			if limitErr.Field != tc.field || limitErr.ConfigKey != "limits.max_body_bytes" ||
				limitErr.Limit != 8 || limitErr.Actual != 10 || limitErr.Unit != "bytes" {
				t.Fatalf("limit error = %+v", limitErr)
			}
			if !errors.Is(limitErr, domain.ErrValidation) {
				t.Fatalf("LimitError does not unwrap to ErrValidation")
			}
			if files := anyRecordFiles(t, fixture.dataDir, "p"); len(files) != 0 {
				t.Fatalf("refused note left files: %v", files)
			}
		})
	}
}

func TestCreateTitleLimit(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		write func(*app.App) error
		field string
	}{
		{
			name: "note",
			write: func(a *app.App) error {
				_, err := a.Note(ctx, app.NoteInput{ProjectID: "p", Title: "12345", Body: "x"})
				return err
			},
			field: "note.title",
		},
		{
			name: "decision",
			write: func(a *app.App) error {
				_, err := a.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "12345"})
				return err
			},
			field: "decision.title",
		},
		{
			name: "attempt",
			write: func(a *app.App) error {
				_, err := a.Attempt(ctx, app.AttemptInput{ProjectID: "p", Title: "12345"})
				return err
			},
			field: "attempt.title",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newLimitsFixture(t, config.LimitsConfig{MaxTitleChars: 4})
			limitErr := limitError(t, tc.write(fixture.app))
			if limitErr.Field != tc.field || limitErr.ConfigKey != "limits.max_title_chars" ||
				limitErr.Limit != 4 || limitErr.Actual != 5 || limitErr.Unit != "chars" {
				t.Fatalf("limit error = %+v", limitErr)
			}
		})
	}
}

func TestCreateMetadataLimit(t *testing.T) {
	ctx := context.Background()
	t.Run("note tags", func(t *testing.T) {
		fixture := newLimitsFixture(t, config.LimitsConfig{MaxMetadataBytes: 5})
		_, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "n", Body: "x", Tags: []string{"0123456789"}})
		limitErr := limitError(t, err)
		if limitErr.Field != "note.tags" || limitErr.ConfigKey != "limits.max_metadata_bytes" ||
			limitErr.Limit != 5 || limitErr.Actual != 10 {
			t.Fatalf("limit error = %+v", limitErr)
		}
	})
	t.Run("decision lists", func(t *testing.T) {
		fixture := newLimitsFixture(t, config.LimitsConfig{MaxMetadataBytes: 5})
		_, err := fixture.app.Decision(ctx, app.DecisionInput{
			ProjectID:    "p",
			Title:        "d",
			Alternatives: []string{"a1", "a2", "a3"},
			Evidence:     []string{"evidence"},
		})
		limitErr := limitError(t, err)
		if limitErr.Field != "decision.metadata" || limitErr.ConfigKey != "limits.max_metadata_bytes" {
			t.Fatalf("limit error = %+v", limitErr)
		}
	})
}

func TestDefaultLimitsAllowCurrentWrites(t *testing.T) {
	ctx := context.Background()
	fixture := newLimitsFixture(t, config.LimitsConfig{})
	body := strings.Repeat("ordinary note text\n", 1024) // ~20 KiB, far below 1 MiB
	if _, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Defaults", Body: body, Tags: []string{"a", "b"}}); err != nil {
		t.Fatalf("default limits refused an ordinary write: %v", err)
	}
	if _, err := fixture.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Defaults", Context: body}); err != nil {
		t.Fatalf("default limits refused an ordinary decision: %v", err)
	}
}

func TestRefusedWriteLeavesNoIndexRow(t *testing.T) {
	ctx := context.Background()
	fixture := newLimitsFixture(t, config.LimitsConfig{MaxBodyBytes: 8})
	_, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "too big", Body: "0123456789"})
	limitError(t, err)
	if files := anyRecordFiles(t, fixture.dataDir, "p"); len(files) != 0 {
		t.Fatalf("refused write left files: %v", files)
	}
	notes, err := fixture.store.ListNotes(ctx, "p")
	if err != nil || len(notes) != 0 {
		t.Fatalf("ListNotes = %+v, %v", notes, err)
	}
	hits, err := fixture.app.Search(ctx, "0123456789", "p", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("refused write reached the index: %+v, %v", hits, err)
	}
}

func TestUpdateRefusesOversizedRecord(t *testing.T) {
	ctx := context.Background()
	fixture := newLimitsFixture(t, config.LimitsConfig{})
	created, err := fixture.app.Decision(ctx, app.DecisionInput{
		ProjectID: "p",
		Title:     "Long context",
		Context:   strings.Repeat("x", 300),
	})
	if err != nil {
		t.Fatal(err)
	}
	tiny := newAppWithLimitsOnDir(t, fixture.dataDir, config.LimitsConfig{MaxBodyBytes: 100})
	_, err = tiny.SetDecisionStatus(ctx, "p", created.Decision.ID, domain.DecisionAccepted)
	limitErr := limitError(t, err)
	if limitErr.Field != "decision.context" || limitErr.ConfigKey != "limits.max_body_bytes" {
		t.Fatalf("limit error = %+v", limitErr)
	}
	record, err := fixture.app.GetRecord(ctx, "p", created.Decision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Decision.Status != domain.DecisionProposed {
		t.Fatalf("refused update changed the status to %s", record.Decision.Status)
	}
}

func TestUpdateRefusesOversizedTitleAndMetadata(t *testing.T) {
	ctx := context.Background()
	fixture := newLimitsFixture(t, config.LimitsConfig{})
	decision, err := fixture.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: strings.Repeat("t", 50)})
	if err != nil {
		t.Fatal(err)
	}
	oldNote, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Tagged", Body: "x", Tags: []string{strings.Repeat("g", 50)}})
	if err != nil {
		t.Fatal(err)
	}
	newNote, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Replacement", Body: "y"})
	if err != nil {
		t.Fatal(err)
	}
	tiny := newAppWithLimitsOnDir(t, fixture.dataDir, config.LimitsConfig{MaxTitleChars: 10, MaxMetadataBytes: 10})
	_, err = tiny.SetDecisionStatus(ctx, "p", decision.Decision.ID, domain.DecisionAccepted)
	if limitErr := limitError(t, err); limitErr.Field != "decision.title" || limitErr.ConfigKey != "limits.max_title_chars" {
		t.Fatalf("title update error = %+v", limitErr)
	}
	_, err = tiny.Supersede(ctx, "p", oldNote.Note.ID, newNote.Note.ID)
	if limitErr := limitError(t, err); limitErr.Field != "note.tags" || limitErr.ConfigKey != "limits.max_metadata_bytes" {
		t.Fatalf("metadata update error = %+v", limitErr)
	}
}

func TestSupersedeRefusesOversizedRecordUpdate(t *testing.T) {
	ctx := context.Background()
	fixture := newLimitsFixture(t, config.LimitsConfig{})
	oldNote, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Old", Body: strings.Repeat("x", 300)})
	if err != nil {
		t.Fatal(err)
	}
	newNote, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "New", Body: "short"})
	if err != nil {
		t.Fatal(err)
	}
	tiny := newAppWithLimitsOnDir(t, fixture.dataDir, config.LimitsConfig{MaxBodyBytes: 100})
	_, err = tiny.Supersede(ctx, "p", oldNote.Note.ID, newNote.Note.ID)
	limitErr := limitError(t, err)
	if limitErr.Field != "note.body" || limitErr.ConfigKey != "limits.max_body_bytes" {
		t.Fatalf("limit error = %+v", limitErr)
	}
	record, err := fixture.app.GetRecord(ctx, "p", oldNote.Note.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Validity().SupersededBy != "" {
		t.Fatalf("refused supersede linked the note: %+v", record.Validity())
	}
}

func TestUpdateUnderTheLimitSucceeds(t *testing.T) {
	ctx := context.Background()
	fixture := newLimitsFixture(t, config.LimitsConfig{MaxBodyBytes: 100})
	created, err := fixture.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Short", Context: "fits"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.SetDecisionStatus(ctx, "p", created.Decision.ID, domain.DecisionAccepted); err != nil {
		t.Fatalf("status update under the limit: %v", err)
	}
}

// newAppWithLimitsOnDir opens a second app over an existing data directory,
// which simulates a configuration change without touching the records.
func newAppWithLimitsOnDir(t *testing.T, dataDir string, limits config.LimitsConfig) *app.App {
	t.Helper()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	cfg.Limits = limits
	return app.New(cfg, app.Dependencies{Store: store})
}

func TestProtectedProjectRefusesEveryKind(t *testing.T) {
	ctx := context.Background()
	fixture := newLimitsFixture(t, config.LimitsConfig{ProtectedProjects: []string{"global"}})
	writes := []func() error{
		func() error {
			_, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "global", Body: "x", CreateProject: true})
			return err
		},
		func() error {
			_, err := fixture.app.Decision(ctx, app.DecisionInput{ProjectID: "global", Title: "d", CreateProject: true})
			return err
		},
		func() error {
			_, err := fixture.app.Attempt(ctx, app.AttemptInput{ProjectID: "global", Title: "a", CreateProject: true})
			return err
		},
		func() error {
			_, err := fixture.app.Handoff(ctx, app.HandoffInput{ProjectID: "global", Summary: "s", CreateProject: true})
			return err
		},
	}
	for i, write := range writes {
		protectedErr := protectedError(t, write())
		if protectedErr.Kind != "project" || protectedErr.ConfigKey != "limits.protected_projects" ||
			protectedErr.ProjectID != "global" {
			t.Fatalf("write %d protected error = %+v", i, protectedErr)
		}
		if !errors.Is(protectedErr, domain.ErrForbidden) {
			t.Fatalf("ProtectedError does not unwrap to ErrForbidden")
		}
	}
	if files := anyRecordFiles(t, fixture.dataDir, "global"); len(files) != 0 {
		t.Fatalf("protected project received files: %v", files)
	}
	// The refusal happens before ensureProject, so --create did not register it.
	if _, err := fixture.store.GetProject(ctx, "global"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("refused write registered the project: %v", err)
	}
}

func TestProtectedGlobRefusesWrite(t *testing.T) {
	ctx := context.Background()
	fixture := newLimitsFixture(t, config.LimitsConfig{ProtectedGlobs: []string{"projects/templates/**"}})
	_, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "templates", Title: "blocked", Body: "x", CreateProject: true})
	protectedErr := protectedError(t, err)
	if protectedErr.Kind != "glob" || protectedErr.ConfigKey != "limits.protected_globs" ||
		protectedErr.Pattern != "projects/templates/**" || !strings.Contains(protectedErr.Path, "projects/templates/notes/") {
		t.Fatalf("protected error = %+v", protectedErr)
	}
	if files := anyRecordFiles(t, fixture.dataDir, "templates"); len(files) != 0 {
		t.Fatalf("protected glob received files: %v", files)
	}
	if _, err := fixture.store.GetProject(ctx, "templates"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("refused write registered the project: %v", err)
	}
}

func TestProtectedGlobMatchesProjectID(t *testing.T) {
	ctx := context.Background()
	fixture := newLimitsFixture(t, config.LimitsConfig{ProtectedGlobs: []string{"templates"}})
	_, err := fixture.app.Decision(ctx, app.DecisionInput{ProjectID: "templates", Title: "blocked", CreateProject: true})
	protectedErr := protectedError(t, err)
	if protectedErr.Kind != "glob" || protectedErr.Pattern != "templates" || protectedErr.Path != "templates" {
		t.Fatalf("protected error = %+v", protectedErr)
	}
}

func TestAllowProtectedIsTheExplicitOverride(t *testing.T) {
	ctx := context.Background()
	fixture := newLimitsFixture(t, config.LimitsConfig{
		ProtectedProjects: []string{"global"},
		ProtectedGlobs:    []string{"projects/templates/**"},
	})
	if _, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "global", Title: "n", Body: "x", CreateProject: true, AllowProtected: true}); err != nil {
		t.Fatalf("--allow-protected note: %v", err)
	}
	if _, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "templates", Title: "n", Body: "x", CreateProject: true, AllowProtected: true}); err != nil {
		t.Fatalf("--allow-protected glob note: %v", err)
	}
}

func TestProtectedProjectRefusesUpdate(t *testing.T) {
	ctx := context.Background()
	fixture := newLimitsFixture(t, config.LimitsConfig{})
	created, err := fixture.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Keep", Context: "x"})
	if err != nil {
		t.Fatal(err)
	}
	guarded := newAppWithLimitsOnDir(t, fixture.dataDir, config.LimitsConfig{ProtectedProjects: []string{"p"}})
	_, err = guarded.SetDecisionStatus(ctx, "p", created.Decision.ID, domain.DecisionAccepted)
	protectedError(t, err)
	record, err := fixture.app.GetRecord(ctx, "p", created.Decision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Decision.Status != domain.DecisionProposed {
		t.Fatalf("refused update changed the status to %s", record.Decision.Status)
	}
}

func TestPromoteRefusesProtectedProject(t *testing.T) {
	ctx := context.Background()
	base := newPromoteFixture(t, domain.PublishShared)
	base.add(t, promoteClosedIssue("issue-closed", "Serialize writes"))
	base.digest(t)
	proposal := base.proposal(t, "issue-closed")

	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.Limits.ProtectedProjects = []string{"demo"}
	guarded := app.New(cfg, app.Dependencies{
		Store:       base.store,
		Connections: base.connections,
		Items:       base.items,
		Proposals:   base.store,
		Now:         func() time.Time { return base.clock },
	})
	_, err := guarded.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID})
	protectedError(t, err)
	decisions, err := base.store.ListDecisions(ctx, "demo")
	if err != nil || len(decisions) != 0 {
		t.Fatalf("refused promotion created decisions: %+v, %v", decisions, err)
	}
	stillPending, err := base.store.Get(ctx, "demo", proposal.ID)
	if err != nil || stillPending.StatusOrPending() != domain.ProposalPending {
		t.Fatalf("refused promotion changed the proposal: %+v, %v", stillPending, err)
	}
	if _, err := guarded.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID, AllowProtected: true}); err != nil {
		t.Fatalf("--allow-protected promotion: %v", err)
	}
}

func TestDoctorLimitsCheckAndLegacyReport(t *testing.T) {
	ctx := context.Background()
	fixture := newLimitsFixture(t, config.LimitsConfig{})
	created, err := fixture.app.Decision(ctx, app.DecisionInput{
		ProjectID: "p",
		Title:     "Legacy",
		Context:   strings.Repeat("x", 300),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := fixture.app.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	limitsCheck := limitsDoctorCheck(t, report, "limits")
	if limitsCheck.Status != "ok" {
		t.Fatalf("limits check = %+v", limitsCheck)
	}
	if !strings.Contains(limitsCheck.Message, "1048576") || !strings.Contains(limitsCheck.Message, "200") {
		t.Fatalf("limits check does not report the defaults: %+v", limitsCheck)
	}
	if recordCheck := limitsDoctorCheck(t, report, "record-limits"); recordCheck.Status != "ok" {
		t.Fatalf("record-limits check = %+v", recordCheck)
	}

	tiny := newAppWithLimitsOnDir(t, fixture.dataDir, config.LimitsConfig{MaxBodyBytes: 100})
	report, err = tiny.Doctor(ctx, app.DoctorOptions{Fix: true})
	if err != nil {
		t.Fatal(err)
	}
	if small := limitsDoctorCheck(t, report, "limits"); small.Status != "warn" || small.Code != "limits-small" {
		t.Fatalf("small limits check = %+v", small)
	}
	recordCheck := limitsDoctorCheck(t, report, "record-limits")
	if recordCheck.Status != "warn" || recordCheck.Code != "record-limits" ||
		!strings.Contains(recordCheck.Message, created.Decision.ID) ||
		!strings.Contains(recordCheck.Message, "limits.max_body_bytes") {
		t.Fatalf("legacy record check = %+v", recordCheck)
	}

	// doctor --fix must not rewrite a record body; the legacy finding stays.
	path := onlyRecordFile(t, fixture.dataDir, "p", "decisions")
	before := readFile(t, path)
	report, err = tiny.Doctor(ctx, app.DoctorOptions{Fix: true})
	if err != nil {
		t.Fatal(err)
	}
	if after := readFile(t, path); after != before {
		t.Fatalf("doctor --fix rewrote a record body")
	}
	if recordCheck := limitsDoctorCheck(t, report, "record-limits"); recordCheck.Status != "warn" {
		t.Fatalf("doctor --fix cleared a report-only finding: %+v", recordCheck)
	}
}

func limitsDoctorCheck(t *testing.T, report app.DoctorReport, name string) app.DoctorCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("no %s check in %+v", name, report.Checks)
	return app.DoctorCheck{}
}

func onlyRecordFile(t *testing.T, dataDir, projectID, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dataDir, "projects", projectID, dir, "*.md"))
	if err != nil || len(files) != 1 {
		t.Fatalf("files in %s = %v, %v", dir, files, err)
	}
	return files[0]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
