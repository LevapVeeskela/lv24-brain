// Package eval scores retrieval quality against authored golden fixtures.
//
// A fixture is one YAML file that describes a small synthetic project: the
// records that make up its corpus and the queries that must retrieve them.
// Run builds a temporary brain data directory from those records, reindexes it
// through the public app API, and scores the queries. Everything is local and
// deterministic: no network, no external service, no model.
package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// FixtureVersion is the only fixture schema this harness understands.
const FixtureVersion = 1

// Record kinds accepted in a fixture corpus.
const (
	KindDecision = "decision"
	KindAttempt  = "attempt"
	KindNote     = "note"
)

// DefaultCategory marks a task that measures ordinary retrieval.
const DefaultCategory = "retrieval"

// Fixture is one project corpus plus its tasks.
type Fixture struct {
	Version int      `yaml:"version"`
	Project string   `yaml:"project"`
	Notes   string   `yaml:"notes,omitempty"`
	Records []Record `yaml:"records"`
	Tasks   []Task   `yaml:"tasks"`

	// Path is the source file, filled by LoadDir.
	Path string `yaml:"-"`
}

// Record is one authored record. The fields mirror the domain records; a
// field that does not apply to the record kind is ignored.
type Record struct {
	ID        string `yaml:"id"`
	Kind      string `yaml:"kind"`
	Title     string `yaml:"title,omitempty"`
	Status    string `yaml:"status,omitempty"`
	CreatedAt string `yaml:"created_at"`

	// Decision fields.
	Context      string   `yaml:"context,omitempty"`
	Alternatives []string `yaml:"alternatives,omitempty"`
	Consequences []string `yaml:"consequences,omitempty"`
	Evidence     []string `yaml:"evidence,omitempty"`
	Paths        []string `yaml:"paths,omitempty"`

	// Attempt fields.
	Hypothesis string `yaml:"hypothesis,omitempty"`
	Result     string `yaml:"result,omitempty"`
	Reason     string `yaml:"reason,omitempty"`

	// Note fields.
	Body string   `yaml:"body,omitempty"`
	Tags []string `yaml:"tags,omitempty"`

	// Temporal validity fields. They mirror domain.Validity: valid_to ends a
	// superseded record without deleting it, so an as-of query can still see
	// the state that was current at that time.
	ValidFrom    string  `yaml:"valid_from,omitempty"`
	ValidTo      string  `yaml:"valid_to,omitempty"`
	SupersededBy string  `yaml:"superseded_by,omitempty"`
	Supersedes   string  `yaml:"supersedes,omitempty"`
	Confidence   float64 `yaml:"confidence,omitempty"`
}

// Task is one labelled query.
type Task struct {
	ID       string `yaml:"id"`
	Query    string `yaml:"query"`
	Category string `yaml:"category,omitempty"`
	// Expected lists the ids that answer the query. At least one is required.
	Expected []string `yaml:"expected"`
	// Forbidden lists obsolete ids that must not rank for a current-state
	// query. Any forbidden id in the top-k is a superseded leak.
	Forbidden []string `yaml:"forbidden,omitempty"`
	// AsOf asks for the state at a point in time. It is parsed and validated
	// now; executing it requires temporal search support (WB-1.1).
	AsOf  string `yaml:"as_of,omitempty"`
	Notes string `yaml:"notes,omitempty"`
}

// Corpus is a directory of fixtures loaded in file-name order.
type Corpus struct {
	Fixtures []Fixture
	// Digest is a SHA-256 over the fixture file names and bytes. It lets a
	// baseline record which labels produced it.
	Digest      string
	TaskCount   int
	RecordCount int
}

// LoadDir reads every .yaml and .yml file in dir as one fixture. Other files
// are ignored. Unknown YAML keys are rejected so a typo cannot silently drop a
// label.
func LoadDir(dir string) (Corpus, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Corpus{}, fmt.Errorf("eval: read fixtures: %w", err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".yaml", ".yml":
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	if len(files) == 0 {
		return Corpus{}, fmt.Errorf("eval: no *.yaml fixtures in %s", dir)
	}
	sort.Strings(files)
	corpus := Corpus{Fixtures: make([]Fixture, 0, len(files))}
	hasher := sha256.New()
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Corpus{}, fmt.Errorf("eval: read fixture %s: %w", path, err)
		}
		fixture, err := ParseFixture(raw)
		if err != nil {
			return Corpus{}, fmt.Errorf("eval: %s: %w", filepath.Base(path), err)
		}
		fixture.Path = path
		corpus.Fixtures = append(corpus.Fixtures, fixture)
		corpus.TaskCount += len(fixture.Tasks)
		corpus.RecordCount += len(fixture.Records)
		_, _ = hasher.Write([]byte(filepath.Base(path)))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write(raw)
		_, _ = hasher.Write([]byte{0})
	}
	corpus.Digest = hex.EncodeToString(hasher.Sum(nil))
	if err := corpus.Validate(); err != nil {
		return Corpus{}, err
	}
	return corpus, nil
}

// ParseFixture decodes one YAML fixture. The decoder rejects unknown fields
// and a second YAML document.
func ParseFixture(raw []byte) (Fixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var fixture Fixture
	if err := decoder.Decode(&fixture); err != nil {
		if errors.Is(err, io.EOF) {
			return Fixture{}, errors.New("empty fixture")
		}
		return Fixture{}, fmt.Errorf("decode fixture: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Fixture{}, errors.New("fixture must contain exactly one YAML document")
	}
	return fixture, nil
}

// Validate checks the schema and every cross-reference. The rules are strict on
// purpose: a fixture that names a missing record would score a permanent,
// unexplained miss.
func (c Corpus) Validate() error {
	if len(c.Fixtures) == 0 {
		return errors.New("eval: corpus is empty")
	}
	projects := make(map[string]string, len(c.Fixtures))
	taskIDs := make(map[string]string)
	for index := range c.Fixtures {
		fixture := &c.Fixtures[index]
		where := fixture.Path
		if where == "" {
			where = fmt.Sprintf("fixture %d", index+1)
		} else {
			where = filepath.Base(where)
		}
		if err := fixture.validate(where, projects, taskIDs); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fixture) validate(where string, projects, taskIDs map[string]string) error {
	prefix := func(format string, args ...any) error {
		return fmt.Errorf("eval: %s: %s", where, fmt.Sprintf(format, args...))
	}
	if f.Version != FixtureVersion {
		return prefix("version = %d, want %d", f.Version, FixtureVersion)
	}
	project := strings.TrimSpace(f.Project)
	if err := validateSlug("project", project); err != nil {
		return prefix("%v", err)
	}
	if previous, duplicate := projects[project]; duplicate {
		return prefix("project %q is already defined by %s", project, previous)
	}
	projects[project] = where

	if len(f.Records) == 0 {
		return prefix("project %q has no records", project)
	}
	if len(f.Tasks) == 0 {
		return prefix("project %q has no tasks", project)
	}
	ids := make(map[string]Record, len(f.Records))
	for index, record := range f.Records {
		if err := record.validate(); err != nil {
			return prefix("record %d (%q): %v", index+1, record.ID, err)
		}
		if _, duplicate := ids[record.ID]; duplicate {
			return prefix("record id %q is defined twice", record.ID)
		}
		ids[record.ID] = record
	}
	for _, record := range f.Records {
		if record.SupersededBy != "" {
			if _, ok := ids[record.SupersededBy]; !ok {
				return prefix("record %q: superseded_by id %q is not a record of this project", record.ID, record.SupersededBy)
			}
		}
		if record.Supersedes != "" {
			if _, ok := ids[record.Supersedes]; !ok {
				return prefix("record %q: supersedes id %q is not a record of this project", record.ID, record.Supersedes)
			}
		}
	}
	for index, task := range f.Tasks {
		if err := task.validate(ids); err != nil {
			return prefix("task %d (%q): %v", index+1, task.ID, err)
		}
		if previous, duplicate := taskIDs[task.ID]; duplicate {
			return prefix("task id %q is already used by %s", task.ID, previous)
		}
		taskIDs[task.ID] = where
	}
	return nil
}

func (r Record) validate() error {
	if err := validateSlug("id", r.ID); err != nil {
		return err
	}
	switch r.Kind {
	case KindDecision:
		if err := requireText("title", r.Title); err != nil {
			return err
		}
		status := strings.TrimSpace(r.Status)
		if status == "" {
			status = string(domain.DecisionAccepted)
		}
		if !domain.DecisionStatus(status).Valid() {
			return fmt.Errorf("status %q is not proposed, accepted, rejected or superseded", r.Status)
		}
		if _, err := domain.NormalizePaths(r.Paths); err != nil {
			return err
		}
	case KindAttempt:
		if err := requireText("title", r.Title); err != nil {
			return err
		}
	case KindNote:
		if err := requireText("body", r.Body); err != nil {
			return err
		}
	default:
		return fmt.Errorf("kind %q is not decision, attempt or note", r.Kind)
	}
	if err := requireText("created_at", r.CreatedAt); err != nil {
		return err
	}
	createdAt, err := r.CreatedTime()
	if err != nil {
		return fmt.Errorf("created_at %q is not RFC 3339: %w", r.CreatedAt, err)
	}
	validity, err := r.Validity()
	if err != nil {
		return err
	}
	if err := validity.Validate("validity", createdAt, r.ID); err != nil {
		return err
	}
	return nil
}

func (t Task) validate(records map[string]Record) error {
	if err := validateSlug("task id", t.ID); err != nil {
		return err
	}
	if err := requireText("query", t.Query); err != nil {
		return err
	}
	if len(t.Expected) == 0 {
		return errors.New("expected needs at least one record id")
	}
	seen := make(map[string]struct{}, len(t.Expected))
	for _, id := range t.Expected {
		if _, ok := records[id]; !ok {
			return fmt.Errorf("expected id %q is not a record of this project", id)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("expected id %q is listed twice", id)
		}
		seen[id] = struct{}{}
	}
	forbidden := make(map[string]struct{}, len(t.Forbidden))
	for _, id := range t.Forbidden {
		if _, ok := records[id]; !ok {
			return fmt.Errorf("forbidden id %q is not a record of this project", id)
		}
		if _, duplicate := forbidden[id]; duplicate {
			return fmt.Errorf("forbidden id %q is listed twice", id)
		}
		if _, overlap := seen[id]; overlap {
			return fmt.Errorf("id %q is both expected and forbidden", id)
		}
		forbidden[id] = struct{}{}
	}
	if _, err := t.AsOfTime(); err != nil {
		return err
	}
	return nil
}

// CategoryName returns the task category, defaulting to "retrieval".
func (t Task) CategoryName() string {
	if category := strings.TrimSpace(t.Category); category != "" {
		return category
	}
	return DefaultCategory
}

// CreatedTime parses the authored creation time.
func (r Record) CreatedTime() (time.Time, error) {
	return time.Parse(time.RFC3339, strings.TrimSpace(r.CreatedAt))
}

// DecisionStatus returns the effective status of a decision record.
func (r Record) DecisionStatus() domain.DecisionStatus {
	if status := strings.TrimSpace(r.Status); status != "" {
		return domain.DecisionStatus(status)
	}
	return domain.DecisionAccepted
}

// Validity parses the optional temporal fields.
func (r Record) Validity() (domain.Validity, error) {
	validity := domain.Validity{
		SupersededBy: strings.TrimSpace(r.SupersededBy),
		Supersedes:   strings.TrimSpace(r.Supersedes),
		Confidence:   r.Confidence,
	}
	var err error
	if validity.ValidFrom, err = parseOptionalTime("valid_from", r.ValidFrom); err != nil {
		return domain.Validity{}, err
	}
	if validity.ValidTo, err = parseOptionalTime("valid_to", r.ValidTo); err != nil {
		return domain.Validity{}, err
	}
	return validity, nil
}

// AsOfTime parses the optional as-of instant of a task.
func (t Task) AsOfTime() (time.Time, error) {
	return parseOptionalTime("as_of", t.AsOf)
}

func parseOptionalTime(field, raw string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %q is not RFC 3339: %w", field, raw, err)
	}
	return parsed, nil
}

func requireText(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	return nil
}

// validateSlug rejects ids that cannot become a file name and would otherwise
// fail deep inside the store with a less useful error.
func validateSlug(field, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("%s is required", field)
	}
	if trimmed != value {
		return fmt.Errorf("%s %q has surrounding whitespace", field, value)
	}
	if len(trimmed) > 200 {
		return fmt.Errorf("%s is longer than 200 bytes", field)
	}
	if trimmed == "." || trimmed == ".." {
		return fmt.Errorf("%s %q is not usable as a file name", field, value)
	}
	if strings.ContainsAny(trimmed, "/\\") {
		return fmt.Errorf("%s %q must not contain a path separator", field, value)
	}
	return nil
}
