package fsstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ContextAllowPath is the raw-context grant policy inside the data dir.
const ContextAllowPath = "context-allow.json"

// maxContextAllowBytes bounds context-allow.json so a damaged or hostile file
// cannot force an unbounded allocation.
const maxContextAllowBytes = 1 << 20

// ContextAllowStore reads the explicit raw-context grants. It never writes:
// the policy file is owner-managed.
type ContextAllowStore struct {
	path string
}

// contextAllowDocument mirrors domain.ContextGrants for strict decoding.
// MaxExcerpts is a pointer so an explicit zero can be rejected while an
// omitted value still selects the domain default.
type contextAllowDocument struct {
	SchemaVersion int                 `json:"schemaVersion"`
	Grants        []contextAllowGrant `json:"grants,omitempty"`
}

// contextAllowGrant mirrors domain.ContextGrant. Keep both in sync: decoding
// rejects fields this struct does not know.
type contextAllowGrant struct {
	Project     string            `json:"project"`
	Connection  string            `json:"connection"`
	Kinds       []domain.ItemKind `json:"kinds,omitempty"`
	MaxAgeDays  int               `json:"maxAgeDays,omitempty"`
	MaxExcerpts *int              `json:"maxExcerpts,omitempty"`
}

var _ ports.ContextGrantStore = (*ContextAllowStore)(nil)

// NewContextAllowStore opens dataDir/context-allow.json. A missing file is a
// default-deny policy, not an error.
func NewContextAllowStore(dataDir string) (*ContextAllowStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, domain.NewValidationError("contextAllow.dataDir", "is required")
	}
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("fsstore: resolve context-allow data dir: %w", err)
	}
	return &ContextAllowStore{path: filepath.Join(root, filepath.FromSlash(ContextAllowPath))}, nil
}

// ContextGrants loads the policy. The file is decoded strictly: unknown
// fields, trailing documents and invalid grants fail instead of silently
// widening or disabling the allow list.
func (s *ContextAllowStore) ContextGrants(ctx context.Context) (domain.ContextGrants, error) {
	if err := ctx.Err(); err != nil {
		return domain.ContextGrants{}, err
	}
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return domain.ContextGrants{SchemaVersion: domain.ContextAllowSchemaVersion}, nil
	}
	if err != nil {
		return domain.ContextGrants{}, fmt.Errorf("fsstore: read %s: %w", ContextAllowPath, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxContextAllowBytes+1))
	if err != nil {
		return domain.ContextGrants{}, fmt.Errorf("fsstore: read %s: %w", ContextAllowPath, err)
	}
	if len(data) > maxContextAllowBytes {
		return domain.ContextGrants{}, fmt.Errorf("fsstore: read %s: file exceeds %d bytes", ContextAllowPath, maxContextAllowBytes)
	}
	if err := ctx.Err(); err != nil {
		return domain.ContextGrants{}, err
	}
	return parseContextAllow(data)
}

func parseContextAllow(data []byte) (domain.ContextGrants, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document contextAllowDocument
	if err := decoder.Decode(&document); err != nil {
		return domain.ContextGrants{}, fmt.Errorf("fsstore: parse %s: %w", ContextAllowPath, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return domain.ContextGrants{}, fmt.Errorf("fsstore: parse %s: unexpected trailing data", ContextAllowPath)
	}
	if document.SchemaVersion == 0 {
		return domain.ContextGrants{}, fmt.Errorf("fsstore: %s: %w", ContextAllowPath,
			domain.NewValidationError("contextAllow.schemaVersion", "is required"))
	}
	grants, err := document.toDomain()
	if err != nil {
		return domain.ContextGrants{}, fmt.Errorf("fsstore: %s: %w", ContextAllowPath, err)
	}
	grants.Normalize()
	if err := grants.Validate(); err != nil {
		return domain.ContextGrants{}, fmt.Errorf("fsstore: %s: %w", ContextAllowPath, err)
	}
	return grants, nil
}

// toDomain converts the decoded document. An explicit maxExcerpts of zero is
// a typo, not a default: Normalize would otherwise turn it into the maximum.
func (d contextAllowDocument) toDomain() (domain.ContextGrants, error) {
	grants := make([]domain.ContextGrant, len(d.Grants))
	for i, source := range d.Grants {
		grant := domain.ContextGrant{
			Project:    source.Project,
			Connection: source.Connection,
			Kinds:      source.Kinds,
			MaxAgeDays: source.MaxAgeDays,
		}
		if source.MaxExcerpts != nil {
			if *source.MaxExcerpts == 0 {
				return domain.ContextGrants{}, domain.NewValidationError(
					fmt.Sprintf("contextAllow.grants[%d].maxExcerpts", i),
					fmt.Sprintf("must be between 1 and %d", domain.MaxContextExcerpts))
			}
			grant.MaxExcerpts = *source.MaxExcerpts
		}
		grants[i] = grant
	}
	return domain.ContextGrants{SchemaVersion: d.SchemaVersion, Grants: grants}, nil
}
