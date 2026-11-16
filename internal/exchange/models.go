package exchange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	FormatName    = "lv24-exchange"
	FormatVersion = "1.0"
	EntryVersion  = 1
)

// Source is the store capability required by the exchange package.
type Source interface {
	ports.Store
	ListNotes(ctx context.Context, projectID string) ([]domain.Note, error)
	ListDecisions(ctx context.Context, projectID string) ([]domain.Decision, error)
	ListAttempts(ctx context.Context, projectID string) ([]domain.Attempt, error)
	ListHandoffs(ctx context.Context, projectID string) ([]domain.Handoff, error)
}

// FileEntry describes one file in the bundle.
type FileEntry struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	SHA256  string `json:"sha256"`
	Entries int    `json:"entries"`
}

// Manifest describes a bundle.
type Manifest struct {
	Format        string `json:"format"`
	FormatVersion string `json:"formatVersion"`
	Version       string `json:"version,omitempty"`
	Spec          string `json:"spec,omitempty"`
	Generator     struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Profile string `json:"profile"`
	} `json:"generator"`
	CreatedAt string `json:"createdAt"`
	Source    struct {
		System string `json:"system"`
	} `json:"source"`
	Scope struct {
		Projects []string `json:"projects"`
		Kinds    []string `json:"kinds"`
	} `json:"scope"`
	Layout struct {
		Compression string `json:"compression"`
	} `json:"layout"`
	Counts struct {
		Projects int `json:"projects"`
		Records  int `json:"records"`
	} `json:"counts"`
	Files []FileEntry `json:"files"`
}

// RecordEntry is one curated record line.
type RecordEntry struct {
	V              int                   `json:"v"`
	Key            string                `json:"key"`
	ProjectID      string                `json:"projectId"`
	Kind           string                `json:"kind"`
	ID             string                `json:"id"`
	Title          string                `json:"title,omitempty"`
	Body           string                `json:"body,omitempty"`
	Tags           []string              `json:"tags,omitempty"`
	Permalink      string                `json:"permalink,omitempty"`
	Source         *domain.SourceRef     `json:"source,omitempty"`
	Provenance     domain.Provenance     `json:"provenance,omitempty"`
	Validity       domain.Validity       `json:"validity,omitempty"`
	MergeInfo      domain.MergeInfo      `json:"mergeInfo,omitempty"`
	Taxonomy       domain.Taxonomy       `json:"taxonomy,omitempty"`
	TopicHistory   domain.TopicHistory   `json:"topicHistory,omitempty"`
	Relations      domain.RelationList   `json:"relations,omitempty"`
	ReviewSchedule domain.ReviewSchedule `json:"reviewSchedule,omitempty"`
	CreatedAt      time.Time             `json:"createdAt,omitzero"`
	UpdatedAt      time.Time             `json:"updatedAt,omitzero"`
	// Decision fields
	Status       domain.DecisionStatus `json:"status,omitempty"`
	Context      string                `json:"context,omitempty"`
	Alternatives []string              `json:"alternatives,omitempty"`
	Consequences []string              `json:"consequences,omitempty"`
	Evidence     []string              `json:"evidence,omitempty"`
	Paths        []string              `json:"paths,omitempty"`
	// Attempt fields
	Hypothesis string `json:"hypothesis,omitempty"`
	Result     string `json:"result,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// Handoff fields
	Summary    string `json:"summary,omitempty"`
	GitContext string `json:"gitContext,omitempty"`
	NextAction string `json:"nextAction,omitempty"`
	// State fields
	Goal          string    `json:"goal,omitempty"`
	Active        []string  `json:"active,omitempty"`
	Blocked       []string  `json:"blocked,omitempty"`
	OpenQuestions []string  `json:"openQuestions,omitempty"`
	FreshUntil    time.Time `json:"freshUntil,omitzero"`
	ContentHash   string    `json:"contentHash"`
}

// ProjectEntry is one project line.
type ProjectEntry struct {
	V             int                `json:"v"`
	Key           string             `json:"key,omitempty"`
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Paths         []string           `json:"paths,omitempty"`
	Aliases       []string           `json:"aliases,omitempty"`
	Kind          domain.ProjectKind `json:"kind"`
	RepoURL       string             `json:"repoUrl,omitempty"`
	DefaultBranch string             `json:"defaultBranch,omitempty"`
	CreatedAt     time.Time          `json:"createdAt,omitzero"`
	UpdatedAt     time.Time          `json:"updatedAt,omitzero"`
}

// TombstoneEntry placeholder.
type TombstoneEntry struct {
	V         int       `json:"v"`
	Entity    string    `json:"entity"`
	Key       string    `json:"key"`
	ProjectID string    `json:"projectId"`
	DeletedAt time.Time `json:"deletedAt,omitzero"`
}

// ImportReport summarizes an import.
type ImportReport struct {
	Created     int      `json:"created"`
	Unchanged   int      `json:"unchanged"`
	Conflicts   int      `json:"conflicts"`
	Quarantined int      `json:"quarantined"`
	Projects    int      `json:"projects"`
	ConflictIDs []string `json:"conflictIds,omitempty"`
}

// ExportOptions controls export.
type ExportOptions struct {
	Out     string
	Project string
	Since   time.Time
	JSON    bool
}

// ImportOptions controls import.
type ImportOptions struct {
	From           string
	DryRun         bool
	JSON           bool
	CreateProjects bool
}

// Lister interface alias for ports.
var _ = ports.Store(nil)

// helpers
func normalizeLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

func recordKey(projectID, kind, id string) string {
	h := sha256.Sum256([]byte(projectID + "\x00" + kind + "\x00" + id))
	return "sha256:" + hex.EncodeToString(h[:])
}

func contentHashOf(e RecordEntry) string {
	e.ContentHash = ""
	// normalize text fields
	e.Body = normalizeLF(e.Body)
	e.Title = normalizeLF(e.Title)
	b, _ := json.Marshal(e)
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// Project key
func projectKey(id string) string {
	h := sha256.Sum256([]byte(id))
	return "sha256:" + hex.EncodeToString(h[:])
}

func nowUTC() time.Time { return time.Now().UTC() }

// MarshalLine marshals a value to one JSON line.
func MarshalLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ValidateManifestVersion checks major version.
func ValidateManifestVersion(v string) error {
	if v == "" {
		return fmt.Errorf("missing format version")
	}
	major := v
	if i := strings.IndexByte(v, '.'); i >= 0 {
		major = v[:i]
	}
	if major != "1" {
		return fmt.Errorf("unsupported format version %s (want 1.x)", v)
	}
	return nil
}
