package migrate_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/migrate"
	_ "modernc.org/sqlite"
)

const engramFixtureSchema = `
CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  project TEXT NOT NULL,
  directory TEXT NOT NULL,
  started_at TEXT NOT NULL DEFAULT (datetime('now')),
  ended_at TEXT,
  summary TEXT
);
CREATE TABLE observations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sync_id TEXT,
  session_id TEXT NOT NULL,
  type TEXT NOT NULL,
  title TEXT NOT NULL,
  content TEXT NOT NULL,
  tool_name TEXT,
  project TEXT,
  scope TEXT NOT NULL DEFAULT 'project',
  topic_key TEXT,
  normalized_hash TEXT,
  revision_count INTEGER NOT NULL DEFAULT 1,
  duplicate_count INTEGER NOT NULL DEFAULT 1,
  last_seen_at TEXT,
  pinned BOOLEAN NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  deleted_at TEXT,
  review_after TEXT,
  expires_at TEXT,
  embedding BLOB,
  embedding_model TEXT,
  embedding_created_at TEXT,
  superseded_by_sync_id TEXT
);
CREATE TABLE memory_relations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sync_id TEXT NOT NULL UNIQUE,
  source_id TEXT,
  target_id TEXT,
  relation TEXT NOT NULL DEFAULT 'pending',
  reason TEXT,
  evidence TEXT,
  confidence REAL,
  judgment_status TEXT NOT NULL DEFAULT 'pending',
  marked_by_actor TEXT,
  marked_by_kind TEXT,
  marked_by_model TEXT,
  session_id TEXT,
  superseded_at TEXT,
  superseded_by_relation_id INTEGER,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE user_prompts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sync_id TEXT,
  session_id TEXT NOT NULL,
  content TEXT NOT NULL,
  project TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  source_inbox_id TEXT
);
CREATE TABLE observation_versions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  observation_id INTEGER NOT NULL,
  title TEXT,
  content TEXT NOT NULL,
  version INTEGER NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(observation_id, version)
);
CREATE TABLE sync_mutations (id INTEGER PRIMARY KEY AUTOINCREMENT, payload TEXT);
`

type fixtureObservation struct {
	syncID      string
	sessionID   string
	typ         string
	title       string
	content     string
	project     *string
	scope       string
	topicKey    string
	reviewAfter string
	createdAt   string
	updatedAt   string
}

func writeEngramFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engram.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(engramFixtureSchema); err != nil {
		t.Fatalf("create fixture schema: %v", err)
	}
	sessions := [][4]string{
		{"ses-lv24", "lv", `C:\0project\lv24`, "2026-10-01 00:00:00"},
		{"ses-main-ims", "main", `C:\0project\IMS`, "2026-10-02 00:00:00"},
		{"ses-main-lv24", "main", `C:\0project\lv24`, "2026-10-03 00:00:00"},
		{"ses-main-brotato", "main", `C:\0project\lv24-brotato\main`, "2026-10-04 00:00:00"},
		{"ses-veeskela", "veeskela", `C:\Users\Veeskela`, "2026-10-05 00:00:00"},
	}
	for _, session := range sessions {
		if _, err := db.Exec(`INSERT INTO sessions (id, project, directory, started_at) VALUES (?, ?, ?, ?)`,
			session[0], session[1], session[2], session[3]); err != nil {
			t.Fatalf("insert session %s: %v", session[0], err)
		}
	}
	quarantineProject := ""
	rows := []fixtureObservation{
		{syncID: "obs-arch", sessionID: "ses-lv24", typ: "architecture", title: "Architecture note A",
			content: "**What**: The arch body\n**Why**: Because B\n**Learned**: Learned C", project: ptr("lv"),
			topicKey: "Arch/One", reviewAfter: "2027-01-15 05:40:16",
			createdAt: "2026-11-01 10:00:00", updatedAt: "2026-11-01 11:00:00"},
		{syncID: "obs-decision", sessionID: "ses-lv24", typ: "decision", title: "Use SQLite",
			content: "**What**: We picked SQLite\n**Why**: It is embedded\n**Where**: internal/store.go\n**Learned**: Keep it small",
			project: ptr("lv"), createdAt: "2026-11-02 10:00:00", updatedAt: "2026-11-02 10:30:00"},
		{syncID: "obs-policy", sessionID: "ses-lv24", typ: "policy", title: "FE policy",
			content: "**What**: No inline styles\n**Why**: Consistency", project: ptr("lv24"),
			createdAt: "2026-11-02 12:00:00", updatedAt: "2026-11-02 12:00:00"},
		{syncID: "obs-sum-1", sessionID: "ses-lv24", typ: "session_summary", title: "Session summary: lv24-code",
			content: "GOAL: first session", project: ptr("lv24-code"),
			createdAt: "2026-11-01 09:00:00", updatedAt: "2026-11-01 09:00:00"},
		{syncID: "obs-sum-2", sessionID: "ses-lv24", typ: "session_summary", title: "Session summary: lv24-code",
			content: "GOAL: second session", project: ptr("lv24-code"),
			createdAt: "2026-11-02 09:00:00", updatedAt: "2026-11-02 09:00:00"},
		{syncID: "obs-sum-3", sessionID: "ses-lv24", typ: "session_summary", title: "Session summary: lv24-code",
			content: "GOAL: third session", project: ptr("lv24-code"),
			createdAt: "2026-11-03 09:00:00", updatedAt: "2026-11-03 09:00:00"},
		{syncID: "obs-main-ims", sessionID: "ses-main-ims", typ: "discovery", title: "IMS discovery",
			content: "IMS body", project: ptr("main"), createdAt: "2026-11-03 10:00:00", updatedAt: "2026-11-03 10:00:00"},
		{syncID: "obs-main-lv24", sessionID: "ses-main-lv24", typ: "discovery", title: "lv24 discovery",
			content: "lv24 body", project: ptr("main"), createdAt: "2026-11-03 11:00:00", updatedAt: "2026-11-03 11:00:00"},
		{syncID: "obs-main-brotato", sessionID: "ses-main-brotato", typ: "discovery", title: "brotato discovery",
			content: "brotato body", project: ptr("main"), createdAt: "2026-11-03 12:00:00", updatedAt: "2026-11-03 12:00:00"},
		{syncID: "obs-main-orphan", sessionID: "missing-session", typ: "discovery", title: "main orphan",
			content: "orphan body", project: ptr("main"), createdAt: "2026-11-03 13:00:00", updatedAt: "2026-11-03 13:00:00"},
		{syncID: "obs-inst", sessionID: "ses-lv24", typ: "bugfix", title: "instaagent bugfix",
			content: "instaagent body", project: ptr("inst"), createdAt: "2026-11-04 10:00:00", updatedAt: "2026-11-04 10:00:00"},
		{syncID: "obs-brotato", sessionID: "ses-lv24", typ: "discovery", title: "brotato retired",
			content: "brotato body", project: ptr("lv24-brotato"), createdAt: "2026-11-04 11:00:00", updatedAt: "2026-11-04 11:00:00"},
		{syncID: "obs-veeskela", sessionID: "ses-veeskela", typ: "preference", title: "Personal preference",
			content: "Personal body", project: ptr("veeskela"), createdAt: "2026-11-04 12:00:00", updatedAt: "2026-11-04 12:00:00"},
		{syncID: "obs-dup", sessionID: "ses-lv24", typ: "pattern", title: "project",
			content: "duplicate body", project: &quarantineProject, createdAt: "2026-11-05 10:00:00", updatedAt: "2026-11-05 10:00:00"},
		{syncID: "obs-dup", sessionID: "ses-lv24", typ: "pattern", title: "project",
			content: "duplicate body", project: &quarantineProject, createdAt: "2026-11-05 10:00:00", updatedAt: "2026-11-05 10:00:00"},
		{syncID: "obs-dup", sessionID: "ses-lv24", typ: "pattern", title: "project",
			content: "duplicate body", project: &quarantineProject, createdAt: "2026-11-05 10:00:00", updatedAt: "2026-11-05 10:00:00"},
		{syncID: "obs-backslash", sessionID: "ses-lv24", typ: "architecture", title: "Backslash project",
			content: "backslash body", project: ptr(`\`), createdAt: "2026-11-05 11:00:00", updatedAt: "2026-11-05 11:00:00"},
		{syncID: "obs-unknown", sessionID: "ses-lv24", typ: "architecture", title: "Unknown project",
			content: "unknown body", project: ptr("unknown"), createdAt: "2026-11-05 12:00:00", updatedAt: "2026-11-05 12:00:00"},
		{syncID: "obs-github", sessionID: "ses-lv24", typ: "architecture", title: "GitHub project",
			content: "github body", project: ptr(".github"), createdAt: "2026-11-05 13:00:00", updatedAt: "2026-11-05 13:00:00"},
		{syncID: "obs-bats", sessionID: "ses-lv24", typ: "config", title: "bats test",
			content: "bats body", project: ptr("bats"), createdAt: "2026-11-05 14:00:00", updatedAt: "2026-11-05 14:00:00"},
		{syncID: "obs-versioned", sessionID: "ses-lv24", typ: "discovery", title: "Versioned note",
			content: "body v2", project: ptr("lv24"), createdAt: "2026-11-06 10:00:00", updatedAt: "2026-11-06 11:00:00"},
		{syncID: "obs-rel-source", sessionID: "ses-lv24", typ: "discovery", title: "Relation source",
			content: "relation source body", project: ptr("lv24"), createdAt: "2026-11-07 10:00:00", updatedAt: "2026-11-07 10:00:00"},
		{syncID: "obs-rel-target", sessionID: "ses-lv24", typ: "discovery", title: "Relation target",
			content: "relation target body", project: ptr("lv24"), createdAt: "2026-11-07 11:00:00", updatedAt: "2026-11-07 11:00:00"},
		{syncID: "obs-rel-target2", sessionID: "ses-lv24", typ: "discovery", title: "Relation target two",
			content: "relation target two body", project: ptr("lv24"), createdAt: "2026-11-07 11:30:00", updatedAt: "2026-11-07 11:30:00"},
		{syncID: "obs-rel-cross", sessionID: "ses-lv24", typ: "discovery", title: "Relation cross project",
			content: "cross project body", project: ptr("lv24-code"), createdAt: "2026-11-07 12:00:00", updatedAt: "2026-11-07 12:00:00"},
		{syncID: "obs-ytk", sessionID: "ses-lv24", typ: "discovery", title: "YouTube knowledge",
			content: "ytk body", project: ptr("lv24-youtube-knowledge"), createdAt: "2026-11-08 10:00:00", updatedAt: "2026-11-08 10:00:00"},
		{syncID: "obs-admin", sessionID: "ses-lv24", typ: "feature", title: "Admin assistant feature",
			content: "admin body", project: ptr("admin-assistant"), createdAt: "2026-11-08 11:00:00", updatedAt: "2026-11-08 11:00:00"},
		{syncID: "obs-docs", sessionID: "ses-lv24", typ: "discovery", title: "Docs project",
			content: "docs body", project: ptr("lv24-brain-docs"), createdAt: "2026-11-08 12:00:00", updatedAt: "2026-11-08 12:00:00"},
		{syncID: "obs-scoped", sessionID: "ses-lv24", typ: "discovery", title: "Global note",
			content: "global body", project: ptr("lv24"), scope: "global",
			createdAt: "2026-11-08 13:00:00", updatedAt: "2026-11-08 13:00:00"},
	}
	for _, row := range rows {
		if _, err := db.Exec(`INSERT INTO observations
			(sync_id, session_id, type, title, content, project, scope, topic_key, review_after, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.syncID, row.sessionID, row.typ, row.title, row.content, row.project, nullableScope(row.scope),
			emptyToNil(row.topicKey), emptyToNil(row.reviewAfter), row.createdAt, row.updatedAt); err != nil {
			t.Fatalf("insert observation %s: %v", row.syncID, err)
		}
	}
	relations := []struct {
		syncID, source, target, relation, status, kind string
		confidence                                     float64
	}{
		{"rel-related", "obs-rel-source", "obs-rel-target", "related", "judged", "agent", 0.9},
		{"rel-pending", "obs-rel-source", "obs-rel-target", "related", "pending", "agent", 0.9},
		{"rel-orphan", "obs-rel-source", "obs-ghost", "compatible", "orphaned", "agent", 0.8},
		{"rel-cross", "obs-rel-source", "obs-rel-cross", "conflicts_with", "judged", "human", 0.7},
		{"rel-missing", "obs-rel-source", "obs-missing", "related", "judged", "agent", 0.5},
		{"rel-not", "obs-rel-source", "obs-rel-target2", "not_conflict", "judged", "agent", 1},
	}
	for _, relation := range relations {
		if _, err := db.Exec(`INSERT INTO memory_relations
			(sync_id, source_id, target_id, relation, judgment_status, marked_by_kind, confidence, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, '2026-11-07 13:00:00', '2026-11-07 13:00:00')`,
			relation.syncID, relation.source, relation.target, relation.relation, relation.status,
			relation.kind, relation.confidence); err != nil {
			t.Fatalf("insert relation %s: %v", relation.syncID, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO observation_versions (observation_id, title, content, version, created_at)
		VALUES (21, 'Versioned note', 'body v1', 1, '2026-11-06 09:00:00')`); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	prompts := []struct {
		syncID, session, content, project, at string
	}{
		{"prompt-1", "ses-lv24", "first prompt", "lv24", "2026-11-01 10:00:00"},
		{"prompt-2", "ses-lv24", "second prompt", "lv24", "2026-11-02 10:00:00"},
		{"prompt-test", "ses-lv24", "test prompt", `c:\users\veeskela\appdata\local\temp\qa\proj`, "2026-11-03 10:00:00"},
	}
	for _, prompt := range prompts {
		if _, err := db.Exec(`INSERT INTO user_prompts (sync_id, session_id, content, project, created_at)
			VALUES (?, ?, ?, ?, ?)`, prompt.syncID, prompt.session, prompt.content, prompt.project, prompt.at); err != nil {
			t.Fatalf("insert prompt %s: %v", prompt.syncID, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO sync_mutations (payload) VALUES ('never imported')`); err != nil {
		t.Fatalf("insert sync mutation: %v", err)
	}
	return path
}

func ptr(value string) *string { return &value }

func emptyToNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableScope(value string) string {
	if value == "" {
		return "project"
	}
	return value
}

func runFixture(t *testing.T, source, target string, mutate func(*migrate.EngramOptions)) (migrate.Result, error) {
	t.Helper()
	options := migrate.EngramOptions{
		SourcePath: source,
		TargetDir:  target,
		DryRun:     true,
		Now:        func() time.Time { return time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) },
	}
	if mutate != nil {
		mutate(&options)
	}
	return migrate.RunEngram(context.Background(), options)
}

func applyFixture(t *testing.T, source, target string, mutate func(*migrate.EngramOptions)) migrate.Result {
	t.Helper()
	result, err := runFixture(t, source, target, func(options *migrate.EngramOptions) {
		options.DryRun = false
		if mutate != nil {
			mutate(options)
		}
	})
	if err != nil {
		t.Fatalf("apply: %v\nfiles: %+v", err, result.Files)
	}
	return result
}

func registerFixtureProjects(t *testing.T, target string, projects []string) *fsstore.Store {
	t.Helper()
	store, err := fsstore.New(target)
	if err != nil {
		t.Fatalf("open target store: %v", err)
	}
	for _, project := range projects {
		if _, err := store.GetProject(context.Background(), project); err == nil {
			continue
		} else if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("get project %s: %v", project, err)
		}
		if err := store.PutProject(context.Background(), domain.Project{
			ID: project, Name: project, Kind: domain.ProjectKindOther,
		}); err != nil {
			t.Fatalf("register project %s: %v", project, err)
		}
	}
	return store
}

func TestEngramDryRunMapsEverythingAndWritesNothing(t *testing.T) {
	source := writeEngramFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	result, err := runFixture(t, source, target, nil)
	if err != nil {
		t.Fatalf("dry run: %v\n%+v", err, result.Files)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("dry run created the target: %v", err)
	}
	if result.TreeHashBefore != result.TreeHashAfter {
		t.Fatalf("dry-run tree hash changed: %s -> %s", result.TreeHashBefore, result.TreeHashAfter)
	}
	if result.Created == 0 || result.Files == nil {
		t.Fatalf("dry run planned no files: %+v", result)
	}
	if got := result.Imported + result.Collapsed + result.Quarantined + result.Skipped + result.Failed; got != result.Scanned {
		t.Fatalf("source rows not accounted for: scanned=%d accounted=%d (%+v)", result.Scanned, got, result)
	}
	if result.QuarantinedRows != 7 {
		t.Fatalf("quarantined rows = %d, want 7", result.QuarantinedRows)
	}
	if result.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (bats)", result.Skipped)
	}
	if result.Relations < 2 || result.RelationsSkipped < 3 {
		t.Fatalf("relations = %d skipped=%d", result.Relations, result.RelationsSkipped)
	}
	if len(result.ProjectMapping) == 0 {
		t.Fatal("project mapping report is empty")
	}
}

func TestEngramApplyMapsRecordsAndCollapsesSummaries(t *testing.T) {
	source := writeEngramFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	result := applyFixture(t, source, target, nil)
	store := registerFixtureProjects(t, target, result.Projects)
	ctx := context.Background()

	notes, err := store.ListNotes(ctx, "lv24")
	if err != nil {
		t.Fatalf("list lv24 notes: %v", err)
	}
	arch := findNote(t, notes, "Architecture note A")
	if arch.Subtype != "architecture" || arch.TopicKey != "arch/one" {
		t.Fatalf("classification = %+v", arch.Taxonomy)
	}
	if arch.Source == nil || arch.Source.ExternalID != "engram:obs-arch" || arch.Source.Session != "ses-lv24" {
		t.Fatalf("source = %+v", arch.Source)
	}
	if arch.ReviewAfter.IsZero() || arch.Provenance.Origin != domain.OriginImport || !arch.ReviewedState() {
		t.Fatalf("provenance/review = %+v %+v", arch.Provenance, arch.ReviewSchedule)
	}
	versioned := findNote(t, notes, "Versioned note")
	if versioned.Body != "body v2" || len(versioned.History) != 1 || versioned.History[0].Body != "body v1" {
		t.Fatalf("version history = %q %+v", versioned.Body, versioned.History)
	}
	scoped := findNote(t, notes, "Global note")
	if scoped.Scope != domain.ScopeGlobal {
		t.Fatalf("scope = %q", scoped.Scope)
	}

	codeNotes, err := store.ListNotes(ctx, "lv24-code")
	if err != nil {
		t.Fatalf("list lv24-code notes: %v", err)
	}
	summary := findNote(t, codeNotes, "Session summary: lv24-code")
	if summary.Subtype != "session_summary" || summary.Occurrences != 3 || summary.Body != "GOAL: third session" {
		t.Fatalf("summary = %+v body=%q", summary.Taxonomy, summary.Body)
	}
	if len(summary.History) != 2 {
		t.Fatalf("summary history = %+v", summary.History)
	}

	decisions, err := store.ListDecisions(ctx, "lv24")
	if err != nil {
		t.Fatalf("list decisions: %v", err)
	}
	decision := findDecision(t, decisions, "Use SQLite")
	if decision.Status != domain.DecisionAccepted {
		t.Fatalf("decision status = %q", decision.Status)
	}
	for _, want := range []string{"We picked SQLite", "It is embedded"} {
		if !strings.Contains(decision.Context, want) {
			t.Fatalf("decision context %q lacks %q", decision.Context, want)
		}
	}
	if len(decision.Consequences) == 0 || !strings.Contains(decision.Consequences[0], "Keep it small") {
		t.Fatalf("decision consequences = %+v", decision.Consequences)
	}
	if decision.Source == nil || decision.Source.Session != "ses-lv24" {
		t.Fatalf("decision source = %+v", decision.Source)
	}

	imsNotes, err := store.ListNotes(ctx, "IMS")
	if err != nil || findNote(t, imsNotes, "IMS discovery").ProjectID != "IMS" {
		t.Fatalf("IMS attribution: %v", err)
	}
	harvestNotes, err := store.ListNotes(ctx, "lv24-harvest")
	if err != nil {
		t.Fatalf("list lv24-harvest: %v", err)
	}
	findNote(t, harvestNotes, "brotato discovery")
	findNote(t, harvestNotes, "brotato retired")
	instaagentNotes, err := store.ListNotes(ctx, "instaagent")
	if err != nil || findNote(t, instaagentNotes, "instaagent bugfix").Subtype != "bugfix" {
		t.Fatalf("inst mapping: %v", err)
	}
	personalNotes, err := store.ListNotes(ctx, "personal")
	if err != nil {
		t.Fatalf("list personal: %v", err)
	}
	personal := findNote(t, personalNotes, "Personal preference")
	if personal.Scope != domain.ScopePersonal {
		t.Fatalf("personal scope = %q", personal.Scope)
	}

	quarantineNotes, err := store.ListNotes(ctx, migrate.QuarantineProject)
	if err != nil {
		t.Fatalf("list quarantine: %v", err)
	}
	quarantine := findNote(t, quarantineNotes, "Unknown project")
	if quarantine.Quarantined() != true || quarantine.Origin != domain.OriginImport {
		t.Fatalf("quarantine provenance = %+v", quarantine.Provenance)
	}
	if _, err := store.ListNotes(ctx, "bats"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("bats project was created: %v", err)
	}

	ytkNotes, err := store.ListNotes(ctx, "lv24-ytk")
	if err != nil || findNote(t, ytkNotes, "YouTube knowledge").ID == "" {
		t.Fatalf("lv24-youtube-knowledge mapping: %v", err)
	}
	adminNotes, err := store.ListNotes(ctx, "admin-assistant")
	if err != nil || findNote(t, adminNotes, "Admin assistant feature").ID == "" {
		t.Fatalf("admin-assistant mapping: %v", err)
	}
	docsNotes, err := store.ListNotes(ctx, "lv24-brain-docs")
	if err != nil || findNote(t, docsNotes, "Docs project").ID == "" {
		t.Fatalf("lv24-brain-docs mapping: %v", err)
	}

	relationNotes, err := store.ListNotes(ctx, "lv24")
	if err != nil {
		t.Fatalf("list relation notes: %v", err)
	}
	relationSource := findNote(t, relationNotes, "Relation source")
	if len(relationSource.Relations) != 2 {
		t.Fatalf("relations = %+v", relationSource.Relations)
	}
	verdicts := map[domain.RelationKind]bool{}
	for _, relation := range relationSource.Relations {
		verdicts[relation.Relation] = true
	}
	if !verdicts[domain.RelationRelatesTo] || !verdicts[domain.RelationNotConflict] {
		t.Fatalf("mapped verdicts = %+v", verdicts)
	}

	if result.Prompts != 0 {
		t.Fatalf("prompts imported without --include-prompts: %d", result.Prompts)
	}
	if _, err := os.Stat(filepath.Join(target, "projects", "lv24", "prompts")); !os.IsNotExist(err) {
		t.Fatalf("prompt zone was created without opt-in: %v", err)
	}
}

func TestEngramRerunIsIdempotentAndConflictsNeverOverwrite(t *testing.T) {
	source := writeEngramFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	first := applyFixture(t, source, target, nil)
	second := applyFixture(t, source, target, nil)
	if second.Created != 0 || second.Conflicts != 0 {
		t.Fatalf("second run created=%d conflicts=%d", second.Created, second.Conflicts)
	}
	if second.Unchanged != len(second.Files) || second.Unchanged != len(first.Files) {
		t.Fatalf("second run unchanged=%d files=%d first=%d", second.Unchanged, len(second.Files), len(first.Files))
	}
	if second.TreeHashBefore != second.TreeHashAfter {
		t.Fatalf("idempotent rerun changed the tree: %s -> %s", second.TreeHashBefore, second.TreeHashAfter)
	}

	edited := filepath.Join(target, filepath.FromSlash(second.Files[0].TargetPath))
	if err := os.WriteFile(edited, []byte("edited by hand\n"), 0o644); err != nil {
		t.Fatalf("edit imported file: %v", err)
	}
	third, err := runFixture(t, source, target, func(options *migrate.EngramOptions) { options.DryRun = false })
	if err == nil {
		t.Fatal("third run with an edited target must report a conflict")
	}
	var incomplete *migrate.IncompleteError
	if !errors.As(err, &incomplete) || third.Conflicts != 1 {
		t.Fatalf("third run conflicts=%d err=%v", third.Conflicts, err)
	}
	raw, err := os.ReadFile(edited)
	if err != nil || string(raw) != "edited by hand\n" {
		t.Fatalf("conflict overwrote the edited file: %q %v", raw, err)
	}
}

func TestEngramApplyAndRollbackRestoreTheTree(t *testing.T) {
	source := writeEngramFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(filepath.Join(target, "projects", "curated", "notes"), 0o755); err != nil {
		t.Fatalf("prepare target: %v", err)
	}
	curated := filepath.Join(target, "projects", "curated", "notes", "2026-11-01.md")
	if err := os.WriteFile(curated, []byte("---\nproject_id: curated\nkind: note\nupdated_at: 2026-11-01T00:00:00Z\nschema_version: 1\ndate: 2026-11-01\n---\n\n## Curated note\n\nkeep me\n"), 0o644); err != nil {
		t.Fatalf("write curated note: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "registry.json"), []byte(`{"version":1,"projects":[{"id":"curated","name":"curated"}]}`), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "brain.yaml"), []byte("mode: local\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	before := snapshotTree(t, target)

	backup, err := migrate.BeginImportBackup(target, source, filepath.Join(target, "brain.yaml"), time.Now())
	if err != nil {
		t.Fatalf("begin backup: %v", err)
	}
	result, err := runFixture(t, source, target, func(options *migrate.EngramOptions) { options.DryRun = false })
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := backup.Finish(result); err != nil {
		t.Fatalf("finish backup: %v", err)
	}
	manifest, err := migrate.FindLatestManifest(target)
	if err != nil {
		t.Fatalf("find manifest: %v", err)
	}
	rollback, err := migrate.Rollback(context.Background(), migrate.RollbackOptions{ManifestPath: manifest})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rollback.Skipped != 0 {
		t.Fatalf("rollback skipped %d entries: %+v", rollback.Skipped, rollback.Entries)
	}
	after := snapshotTree(t, target)
	if len(before) != len(after) {
		t.Fatalf("tree has %d entries after rollback, want %d", len(after), len(before))
	}
	for path, content := range before {
		if after[path] != content {
			t.Fatalf("%s differs after rollback", path)
		}
	}
}

func TestEngramPromptsAreOptInAndLocalOnly(t *testing.T) {
	source := writeEngramFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	if _, err := runFixture(t, source, target, func(options *migrate.EngramOptions) {
		options.IncludePrompts = true
	}); err == nil {
		t.Fatal("include-prompts without prompts.enabled must fail")
	}

	result := applyFixture(t, source, target, func(options *migrate.EngramOptions) {
		options.IncludePrompts = true
		options.PromptsEnabled = true
	})
	if result.Prompts != 2 || result.PromptsSkipped != 1 {
		t.Fatalf("prompts=%d skipped=%d, want 2/1", result.Prompts, result.PromptsSkipped)
	}
	promptFiles, err := filepath.Glob(filepath.Join(target, "projects", "lv24", "prompts", "*.md"))
	if err != nil || len(promptFiles) != 2 {
		t.Fatalf("prompt files = %v, %v", promptFiles, err)
	}
	raw, err := os.ReadFile(promptFiles[0])
	if err != nil {
		t.Fatalf("read prompt file: %v", err)
	}
	secondRaw, err := os.ReadFile(promptFiles[1])
	if err != nil {
		t.Fatalf("read second prompt file: %v", err)
	}
	text := string(raw) + string(secondRaw)
	if !strings.Contains(text, "visibility: local") || !strings.Contains(text, "first prompt") || !strings.Contains(text, "second prompt") {
		t.Fatalf("prompt file content:\n%s", text)
	}
	ignore, err := os.ReadFile(filepath.Join(target, "projects", "lv24", "prompts", ".gitignore"))
	if err != nil || strings.TrimSpace(string(ignore)) != "*" {
		t.Fatalf("prompt gitignore = %q, %v", ignore, err)
	}
	if strings.Contains(text, "test prompt") {
		t.Fatalf("test-path prompt was imported:\n%s", text)
	}
}

func TestEngramProjectMapOverrideAndJSONExport(t *testing.T) {
	source := writeEngramFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	projectMap := migrate.DefaultProjectMap()
	projectMap.Map["lv"] = "custom-project"
	projectMap.Map["inst"] = "custom-project"
	result := applyFixture(t, source, target, func(options *migrate.EngramOptions) {
		options.ProjectMap = projectMap
	})
	store := registerFixtureProjects(t, target, result.Projects)
	notes, err := store.ListNotes(context.Background(), "custom-project")
	if err != nil {
		t.Fatalf("list custom-project: %v", err)
	}
	findNote(t, notes, "Architecture note A")
	findNote(t, notes, "instaagent bugfix")

	exportPath := filepath.Join(t.TempDir(), "engram-export.json")
	export := map[string]any{
		"observations": []map[string]any{
			{
				"id": 1, "sync_id": "obs-export", "session_id": "ses-export", "type": "discovery",
				"title": "Export note", "content": "export body",
				"created_at": "2026-11-09 10:00:00", "updated_at": "2026-11-09 10:00:00",
			},
		},
		// `engram export` carries the project on the session, not on the
		// observation; the importer must join it.
		"sessions": []map[string]any{{"id": "ses-export", "project": "lv", "directory": `C:\0project\lv24`}},
	}
	raw, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	if err := os.WriteFile(exportPath, raw, 0o644); err != nil {
		t.Fatalf("write export: %v", err)
	}
	jsonTarget := filepath.Join(t.TempDir(), "json-data")
	jsonResult := applyFixture(t, exportPath, jsonTarget, nil)
	if jsonResult.Scanned != 1 || jsonResult.Created == 0 {
		t.Fatalf("JSON export import: %+v", jsonResult)
	}
	jsonStore := registerFixtureProjects(t, jsonTarget, jsonResult.Projects)
	jsonNotes, err := jsonStore.ListNotes(context.Background(), "lv24")
	if err != nil {
		t.Fatalf("list JSON notes: %v", err)
	}
	findNote(t, jsonNotes, "Export note")
}

func findNote(t *testing.T, notes []domain.Note, title string) domain.Note {
	t.Helper()
	for _, note := range notes {
		if note.Title == title {
			return note
		}
	}
	t.Fatalf("note %q not found in %d notes", title, len(notes))
	return domain.Note{}
}

func findDecision(t *testing.T, decisions []domain.Decision, title string) domain.Decision {
	t.Helper()
	for _, decision := range decisions {
		if decision.Title == title {
			return decision
		}
	}
	t.Fatalf("decision %q not found in %d decisions", title, len(decisions))
	return domain.Decision{}
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == "backups" {
				return filepath.SkipDir
			}
			snapshot[filepath.ToSlash(rel)] = "dir"
			return nil
		}
		raw, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		snapshot[filepath.ToSlash(rel)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snapshot
}
