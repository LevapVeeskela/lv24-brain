package migrate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"modernc.org/sqlite"
)

// EngramSourceSystem is the source.system value the engram importer reports.
const EngramSourceSystem = "engram"

// engramObservation is one row of the engram observations table. Timestamps
// stay strings: engram writes "YYYY-MM-DD HH:MM:SS" in UTC and older exports
// may use RFC 3339.
type engramObservation struct {
	ID           int64
	SyncID       string
	SessionID    string
	Type         string
	Title        string
	Content      string
	Project      string
	Scope        string
	TopicKey     string
	ReviewAfter  string
	CreatedAt    string
	UpdatedAt    string
	DeletedAt    string
	SupersededBy string
}

type engramRelation struct {
	SourceID       string
	TargetID       string
	Relation       string
	JudgmentStatus string
	Reason         string
	Confidence     float64
	MarkedByKind   string
	MarkedByActor  string
	MarkedByModel  string
	CreatedAt      string
	UpdatedAt      string
	SupersededAt   string
}

type engramSession struct {
	ID        string
	Project   string
	Directory string
	StartedAt string
	Summary   string
}

type engramPrompt struct {
	ID        int64
	SyncID    string
	SessionID string
	Content   string
	Project   string
	CreatedAt string
}

type engramVersion struct {
	ObservationID int64
	Version       int
	Title         string
	Content       string
	CreatedAt     string
}

// engramSource is the read-only snapshot of one engram store.
type engramSource struct {
	Path         string
	Format       string
	Observations []engramObservation
	Relations    []engramRelation
	Sessions     []engramSession
	Prompts      []engramPrompt
	Versions     []engramVersion
}

// loadEngram reads an engram SQLite database or a JSON export. The SQLite file
// is opened through file:...?mode=ro&_pragma=query_only(1), the same pattern as
// the dbschema adapter, so an import can never mutate the source.
func loadEngram(ctx context.Context, sourcePath string) (engramSource, error) {
	path, err := filepath.Abs(strings.TrimSpace(sourcePath))
	if err != nil {
		return engramSource{}, fmt.Errorf("migrate: resolve engram source: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return engramSource{}, fmt.Errorf("migrate: engram source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return engramSource{}, fmt.Errorf("migrate: engram source %s is not a regular file", path)
	}
	head := make([]byte, 16)
	file, err := os.Open(path)
	if err != nil {
		return engramSource{}, fmt.Errorf("migrate: open engram source: %w", err)
	}
	_, readErr := file.Read(head)
	closeErr := file.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return engramSource{}, fmt.Errorf("migrate: read engram source header: %w", readErr)
	}
	if closeErr != nil {
		return engramSource{}, fmt.Errorf("migrate: close engram source: %w", closeErr)
	}
	if bytes.HasPrefix(head, []byte("SQLite format 3\x00")) {
		return loadEngramSQLite(ctx, path)
	}
	return loadEngramJSON(path)
}

func loadEngramSQLite(ctx context.Context, path string) (engramSource, error) {
	connector, err := sqlite.NewConnector("file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return engramSource{}, fmt.Errorf("migrate: open engram database read-only: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return engramSource{}, fmt.Errorf("migrate: open engram database read-only: %w (close the tool that holds it and retry)", err)
	}
	var queryOnly int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		return engramSource{}, fmt.Errorf("migrate: read engram query_only: %w", err)
	}
	if queryOnly != 1 {
		if _, err := db.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
			return engramSource{}, fmt.Errorf("migrate: set engram query_only: %w", err)
		}
		if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
			return engramSource{}, fmt.Errorf("migrate: refusing a writable engram connection")
		}
	}
	source := engramSource{Path: path, Format: "sqlite"}
	observations, err := readEngramObservations(ctx, db)
	if err != nil {
		return engramSource{}, err
	}
	source.Observations = observations
	if source.Relations, err = readEngramRelations(ctx, db); err != nil {
		return engramSource{}, err
	}
	if source.Sessions, err = readEngramSessions(ctx, db); err != nil {
		return engramSource{}, err
	}
	if source.Prompts, err = readEngramPrompts(ctx, db); err != nil {
		return engramSource{}, err
	}
	if source.Versions, err = readEngramVersions(ctx, db); err != nil {
		return engramSource{}, err
	}
	return source, nil
}

func tableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, fmt.Errorf("migrate: inspect engram table %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	columns := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			kind    string
			notNull int
			def     sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &kind, &notNull, &def, &pk); err != nil {
			return nil, fmt.Errorf("migrate: read engram table %s: %w", table, err)
		}
		columns[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read engram table %s: %w", table, err)
	}
	return columns, nil
}

func tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type IN ('table','view') AND name = ?", table).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("migrate: inspect engram schema: %w", err)
	}
	return count > 0, nil
}

func textColumn(columns map[string]bool, name string) string {
	if columns[name] {
		return "COALESCE(" + name + ", '')"
	}
	return "''"
}

func intColumn(columns map[string]bool, name string) string {
	if columns[name] {
		return "COALESCE(" + name + ", 0)"
	}
	return "0"
}

func realColumn(columns map[string]bool, name string) string {
	if columns[name] {
		return "COALESCE(" + name + ", 0)"
	}
	return "0"
}

func readEngramObservations(ctx context.Context, db *sql.DB) ([]engramObservation, error) {
	exists, err := tableExists(ctx, db, "observations")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("migrate: engram source is not an engram database: missing observations table")
	}
	columns, err := tableColumns(ctx, db, "observations")
	if err != nil {
		return nil, err
	}
	query := "SELECT " + intColumn(columns, "id") + ", " +
		textColumn(columns, "sync_id") + ", " +
		textColumn(columns, "session_id") + ", " +
		textColumn(columns, "type") + ", " +
		textColumn(columns, "title") + ", " +
		textColumn(columns, "content") + ", " +
		textColumn(columns, "project") + ", " +
		textColumn(columns, "scope") + ", " +
		textColumn(columns, "topic_key") + ", " +
		textColumn(columns, "review_after") + ", " +
		textColumn(columns, "created_at") + ", " +
		textColumn(columns, "updated_at") + ", " +
		textColumn(columns, "deleted_at") + ", " +
		textColumn(columns, "superseded_by_sync_id") +
		" FROM observations ORDER BY id"
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("migrate: read engram observations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	observations := make([]engramObservation, 0, 1024)
	for rows.Next() {
		var observation engramObservation
		if err := rows.Scan(&observation.ID, &observation.SyncID, &observation.SessionID,
			&observation.Type, &observation.Title, &observation.Content, &observation.Project,
			&observation.Scope, &observation.TopicKey, &observation.ReviewAfter,
			&observation.CreatedAt, &observation.UpdatedAt, &observation.DeletedAt,
			&observation.SupersededBy); err != nil {
			return nil, fmt.Errorf("migrate: read engram observation: %w", err)
		}
		if strings.TrimSpace(observation.DeletedAt) != "" {
			continue
		}
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read engram observations: %w", err)
	}
	return observations, nil
}

func readEngramRelations(ctx context.Context, db *sql.DB) ([]engramRelation, error) {
	exists, err := tableExists(ctx, db, "memory_relations")
	if err != nil {
		return nil, err
	}
	if !exists {
		return []engramRelation{}, nil
	}
	columns, err := tableColumns(ctx, db, "memory_relations")
	if err != nil {
		return nil, err
	}
	query := "SELECT " + textColumn(columns, "source_id") + ", " +
		textColumn(columns, "target_id") + ", " +
		textColumn(columns, "relation") + ", " +
		textColumn(columns, "judgment_status") + ", " +
		textColumn(columns, "reason") + ", " +
		realColumn(columns, "confidence") + ", " +
		textColumn(columns, "marked_by_kind") + ", " +
		textColumn(columns, "marked_by_actor") + ", " +
		textColumn(columns, "marked_by_model") + ", " +
		textColumn(columns, "created_at") + ", " +
		textColumn(columns, "updated_at") + ", " +
		textColumn(columns, "superseded_at") +
		" FROM memory_relations ORDER BY id"
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("migrate: read engram relations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	relations := make([]engramRelation, 0, 256)
	for rows.Next() {
		var relation engramRelation
		if err := rows.Scan(&relation.SourceID, &relation.TargetID, &relation.Relation,
			&relation.JudgmentStatus, &relation.Reason, &relation.Confidence,
			&relation.MarkedByKind, &relation.MarkedByActor, &relation.MarkedByModel,
			&relation.CreatedAt, &relation.UpdatedAt, &relation.SupersededAt); err != nil {
			return nil, fmt.Errorf("migrate: read engram relation: %w", err)
		}
		relations = append(relations, relation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read engram relations: %w", err)
	}
	return relations, nil
}

func readEngramSessions(ctx context.Context, db *sql.DB) ([]engramSession, error) {
	exists, err := tableExists(ctx, db, "sessions")
	if err != nil {
		return nil, err
	}
	if !exists {
		return []engramSession{}, nil
	}
	columns, err := tableColumns(ctx, db, "sessions")
	if err != nil {
		return nil, err
	}
	query := "SELECT " + textColumn(columns, "id") + ", " +
		textColumn(columns, "project") + ", " +
		textColumn(columns, "directory") + ", " +
		textColumn(columns, "started_at") + ", " +
		textColumn(columns, "summary") +
		" FROM sessions ORDER BY started_at"
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("migrate: read engram sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	sessions := make([]engramSession, 0, 256)
	for rows.Next() {
		var session engramSession
		if err := rows.Scan(&session.ID, &session.Project, &session.Directory,
			&session.StartedAt, &session.Summary); err != nil {
			return nil, fmt.Errorf("migrate: read engram session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read engram sessions: %w", err)
	}
	return sessions, nil
}

func readEngramPrompts(ctx context.Context, db *sql.DB) ([]engramPrompt, error) {
	exists, err := tableExists(ctx, db, "user_prompts")
	if err != nil {
		return nil, err
	}
	if !exists {
		return []engramPrompt{}, nil
	}
	columns, err := tableColumns(ctx, db, "user_prompts")
	if err != nil {
		return nil, err
	}
	query := "SELECT " + intColumn(columns, "id") + ", " +
		textColumn(columns, "sync_id") + ", " +
		textColumn(columns, "session_id") + ", " +
		textColumn(columns, "content") + ", " +
		textColumn(columns, "project") + ", " +
		textColumn(columns, "created_at") +
		" FROM user_prompts ORDER BY id"
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("migrate: read engram prompts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	prompts := make([]engramPrompt, 0, 256)
	for rows.Next() {
		var prompt engramPrompt
		if err := rows.Scan(&prompt.ID, &prompt.SyncID, &prompt.SessionID, &prompt.Content,
			&prompt.Project, &prompt.CreatedAt); err != nil {
			return nil, fmt.Errorf("migrate: read engram prompt: %w", err)
		}
		prompts = append(prompts, prompt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read engram prompts: %w", err)
	}
	return prompts, nil
}

func readEngramVersions(ctx context.Context, db *sql.DB) ([]engramVersion, error) {
	exists, err := tableExists(ctx, db, "observation_versions")
	if err != nil {
		return nil, err
	}
	if !exists {
		return []engramVersion{}, nil
	}
	columns, err := tableColumns(ctx, db, "observation_versions")
	if err != nil {
		return nil, err
	}
	query := "SELECT " + intColumn(columns, "observation_id") + ", " +
		intColumn(columns, "version") + ", " +
		textColumn(columns, "title") + ", " +
		textColumn(columns, "content") + ", " +
		textColumn(columns, "created_at") +
		" FROM observation_versions ORDER BY observation_id, version"
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("migrate: read engram versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	versions := make([]engramVersion, 0, 32)
	for rows.Next() {
		var version engramVersion
		if err := rows.Scan(&version.ObservationID, &version.Version, &version.Title,
			&version.Content, &version.CreatedAt); err != nil {
			return nil, fmt.Errorf("migrate: read engram version: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read engram versions: %w", err)
	}
	return versions, nil
}

// engramJSONExport is the container accepted by the JSON adapter: either the
// tables at the top level or nested under "tables". An array at the top level
// is accepted as observations only.
type engramJSONExport struct {
	Observations []engramJSONObservation    `json:"observations"`
	Relations    []engramJSONRelation       `json:"relations"`
	MemoryRel    []engramJSONRelation       `json:"memory_relations"`
	Sessions     []engramJSONSession        `json:"sessions"`
	Prompts      []engramJSONPrompt         `json:"prompts"`
	UserPrompts  []engramJSONPrompt         `json:"user_prompts"`
	Versions     []engramJSONVersion        `json:"observation_versions"`
	Tables       map[string]json.RawMessage `json:"tables"`
}

type engramJSONObservation struct {
	ID           int64   `json:"id"`
	SyncID       string  `json:"sync_id"`
	SessionID    string  `json:"session_id"`
	Type         string  `json:"type"`
	Title        string  `json:"title"`
	Content      string  `json:"content"`
	Project      *string `json:"project"`
	Scope        string  `json:"scope"`
	TopicKey     string  `json:"topic_key"`
	ReviewAfter  string  `json:"review_after"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	DeletedAt    string  `json:"deleted_at"`
	SupersededBy string  `json:"superseded_by_sync_id"`
}

type engramJSONRelation struct {
	SourceID       string  `json:"source_id"`
	TargetID       string  `json:"target_id"`
	Relation       string  `json:"relation"`
	JudgmentStatus string  `json:"judgment_status"`
	Reason         string  `json:"reason"`
	Confidence     float64 `json:"confidence"`
	MarkedByKind   string  `json:"marked_by_kind"`
	MarkedByActor  string  `json:"marked_by_actor"`
	MarkedByModel  string  `json:"marked_by_model"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	SupersededAt   string  `json:"superseded_at"`
}

type engramJSONSession struct {
	ID        string `json:"id"`
	Project   string `json:"project"`
	Directory string `json:"directory"`
	StartedAt string `json:"started_at"`
	Summary   string `json:"summary"`
}

type engramJSONPrompt struct {
	ID        int64   `json:"id"`
	SyncID    string  `json:"sync_id"`
	SessionID string  `json:"session_id"`
	Content   string  `json:"content"`
	Project   *string `json:"project"`
	CreatedAt string  `json:"created_at"`
}

type engramJSONVersion struct {
	ObservationID int64  `json:"observation_id"`
	Version       int    `json:"version"`
	Title         string `json:"title"`
	Content       string `json:"content"`
	CreatedAt     string `json:"created_at"`
}

func loadEngramJSON(path string) (engramSource, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return engramSource{}, fmt.Errorf("migrate: read engram export: %w", err)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	raw = bytes.TrimSpace(raw)
	source := engramSource{Path: path, Format: "json"}
	if len(raw) == 0 {
		return engramSource{}, fmt.Errorf("migrate: engram export %s is empty", path)
	}
	if raw[0] == '[' {
		var observations []engramJSONObservation
		if err := json.Unmarshal(raw, &observations); err != nil {
			return engramSource{}, fmt.Errorf("migrate: parse engram export: %w", err)
		}
		source.Observations = convertJSONObservations(observations, nil)
		return source, nil
	}
	var export engramJSONExport
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&export); err != nil {
		return engramSource{}, fmt.Errorf("migrate: parse engram export: %w", err)
	}
	if len(export.Tables) > 0 {
		decode := func(name string, out any) error {
			value, ok := export.Tables[name]
			if !ok || len(value) == 0 || string(value) == "null" {
				return nil
			}
			return json.Unmarshal(value, out)
		}
		if err := decode("observations", &export.Observations); err != nil {
			return engramSource{}, fmt.Errorf("migrate: parse engram export table observations: %w", err)
		}
		if err := decode("memory_relations", &export.MemoryRel); err != nil {
			return engramSource{}, fmt.Errorf("migrate: parse engram export table memory_relations: %w", err)
		}
		if err := decode("relations", &export.Relations); err != nil {
			return engramSource{}, fmt.Errorf("migrate: parse engram export table relations: %w", err)
		}
		if err := decode("sessions", &export.Sessions); err != nil {
			return engramSource{}, fmt.Errorf("migrate: parse engram export table sessions: %w", err)
		}
		if err := decode("user_prompts", &export.UserPrompts); err != nil {
			return engramSource{}, fmt.Errorf("migrate: parse engram export table user_prompts: %w", err)
		}
		if err := decode("prompts", &export.Prompts); err != nil {
			return engramSource{}, fmt.Errorf("migrate: parse engram export table prompts: %w", err)
		}
		if err := decode("observation_versions", &export.Versions); err != nil {
			return engramSource{}, fmt.Errorf("migrate: parse engram export table observation_versions: %w", err)
		}
	}
	source.Observations = convertJSONObservations(export.Observations, export.Sessions)
	for _, relation := range append(export.Relations, export.MemoryRel...) {
		source.Relations = append(source.Relations, engramRelation{
			SourceID: relation.SourceID, TargetID: relation.TargetID,
			Relation: relation.Relation, JudgmentStatus: relation.JudgmentStatus,
			Reason: relation.Reason, Confidence: relation.Confidence,
			MarkedByKind: relation.MarkedByKind, MarkedByActor: relation.MarkedByActor,
			MarkedByModel: relation.MarkedByModel, CreatedAt: relation.CreatedAt,
			UpdatedAt: relation.UpdatedAt, SupersededAt: relation.SupersededAt,
		})
	}
	for _, session := range export.Sessions {
		source.Sessions = append(source.Sessions, engramSession{
			ID: session.ID, Project: session.Project, Directory: session.Directory,
			StartedAt: session.StartedAt, Summary: session.Summary,
		})
	}
	for _, prompt := range append(export.Prompts, export.UserPrompts...) {
		source.Prompts = append(source.Prompts, engramPrompt{
			ID: prompt.ID, SyncID: prompt.SyncID, SessionID: prompt.SessionID,
			Content: prompt.Content, Project: derefString(prompt.Project), CreatedAt: prompt.CreatedAt,
		})
	}
	for _, version := range export.Versions {
		source.Versions = append(source.Versions, engramVersion{
			ObservationID: version.ObservationID, Version: version.Version,
			Title: version.Title, Content: version.Content, CreatedAt: version.CreatedAt,
		})
	}
	if len(source.Observations) == 0 {
		return engramSource{}, fmt.Errorf("migrate: engram export %s has no observations", path)
	}
	return source, nil
}

func convertJSONObservations(observations []engramJSONObservation, sessions []engramJSONSession) []engramObservation {
	sessionProject := make(map[string]string, len(sessions))
	for _, session := range sessions {
		sessionProject[session.ID] = session.Project
	}
	out := make([]engramObservation, 0, len(observations))
	for _, observation := range observations {
		if strings.TrimSpace(observation.DeletedAt) != "" {
			continue
		}
		project := derefString(observation.Project)
		if project == "" {
			// `engram export` does not repeat the project on every observation;
			// it is carried by the session. A real database keeps NULL project
			// rows as NULL, so this fallback is only used for the JSON export.
			project = sessionProject[observation.SessionID]
		}
		out = append(out, engramObservation{
			ID: observation.ID, SyncID: observation.SyncID, SessionID: observation.SessionID,
			Type: observation.Type, Title: observation.Title, Content: observation.Content,
			Project: project, Scope: observation.Scope,
			TopicKey: observation.TopicKey, ReviewAfter: observation.ReviewAfter,
			CreatedAt: observation.CreatedAt, UpdatedAt: observation.UpdatedAt,
			SupersededBy: observation.SupersededBy,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
