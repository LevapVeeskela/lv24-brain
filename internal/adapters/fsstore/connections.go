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
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ConnectionsPath is the non-secret connection catalogue inside the data dir.
const ConnectionsPath = "connectors/connections.json"

const connectionSchemaVersion = 1

// ConnectionStore persists a sorted JSON array. Each entry carries its schema
// version alongside the Connection fields. Provider settings must be checked
// against a descriptor by the caller before being passed to this store.
type ConnectionStore struct {
	path string
}

type connectionRecord struct {
	SchemaVersion int `json:"schemaVersion"`
	domain.Connection
}

var _ ports.ConnectionStore = (*ConnectionStore)(nil)

// NewConnectionStore opens the local/git catalogue under dataDir. A missing
// file is an empty catalogue; an existing damaged file is never reset.
func NewConnectionStore(dataDir string) (*ConnectionStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, domain.NewValidationError("connections.dataDir", "is required")
	}
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("fsstore: resolve connections data dir: %w", err)
	}
	return &ConnectionStore{path: filepath.Join(root, filepath.FromSlash(ConnectionsPath))}, nil
}

// List returns fresh connection values ordered by ID.
func (s *ConnectionStore) List(ctx context.Context) ([]domain.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.load()
}

// Get returns domain.ErrNotFound for an unknown ID.
func (s *ConnectionStore) Get(ctx context.Context, id string) (domain.Connection, error) {
	if err := domain.ValidateConnectionID("connection.id", id); err != nil {
		return domain.Connection{}, err
	}
	connections, err := s.List(ctx)
	if err != nil {
		return domain.Connection{}, err
	}
	for _, connection := range connections {
		if connection.ID == id {
			return connection, nil
		}
	}
	return domain.Connection{}, fmt.Errorf("%w: connection %q", domain.ErrNotFound, id)
}

// Create rejects duplicate IDs and canonical identities with domain.ErrConflict.
func (s *ConnectionStore) Create(ctx context.Context, connection domain.Connection) error {
	if err := connection.Validate(); err != nil {
		return err
	}
	// The ID becomes connectors/<id>/ on disk: reject names that clash with
	// other connectors/ entries (runs) or Windows device names.
	if err := validateItemConnectionID(connection.ID); err != nil {
		return err
	}
	return s.change(ctx, func(connections []domain.Connection) ([]domain.Connection, error) {
		for _, existing := range connections {
			if existing.ID == connection.ID || existing.Identity() == connection.Identity() {
				return nil, fmt.Errorf("%w: connection %q duplicates %q", domain.ErrConflict, connection.ID, existing.ID)
			}
		}
		return append(connections, connection), nil
	})
}

// Update replaces mutable settings, keeping the canonical identity immutable.
func (s *ConnectionStore) Update(ctx context.Context, connection domain.Connection) error {
	if err := connection.Validate(); err != nil {
		return err
	}
	return s.change(ctx, func(connections []domain.Connection) ([]domain.Connection, error) {
		for i, existing := range connections {
			if existing.ID != connection.ID {
				continue
			}
			if existing.Identity() != connection.Identity() {
				return nil, fmt.Errorf("%w: connection %q identity is immutable", domain.ErrConflict, connection.ID)
			}
			connections[i] = connection
			return connections, nil
		}
		return nil, fmt.Errorf("%w: connection %q", domain.ErrNotFound, connection.ID)
	})
}

// Patch applies mutate while holding the store's write lock. Reading and
// writing under the same lock prevents concurrent PATCH-style updates from
// overwriting each other with stale snapshots.
func (s *ConnectionStore) Patch(ctx context.Context, id string, mutate func(*domain.Connection) error) (domain.Connection, error) {
	if err := domain.ValidateConnectionID("connection.id", id); err != nil {
		return domain.Connection{}, err
	}
	if mutate == nil {
		return domain.Connection{}, domain.NewValidationError("connection.patch", "mutate is required")
	}
	var patched domain.Connection
	err := s.change(ctx, func(connections []domain.Connection) ([]domain.Connection, error) {
		for i, existing := range connections {
			if existing.ID != id {
				continue
			}
			candidate := existing
			if err := mutate(&candidate); err != nil {
				return nil, err
			}
			if candidate.ID != id {
				return nil, fmt.Errorf("%w: connection %q id is immutable", domain.ErrConflict, id)
			}
			if existing.Identity() != candidate.Identity() {
				return nil, fmt.Errorf("%w: connection %q identity is immutable", domain.ErrConflict, id)
			}
			connections[i] = candidate
			patched = candidate
			return connections, nil
		}
		return nil, fmt.Errorf("%w: connection %q", domain.ErrNotFound, id)
	})
	if err != nil {
		return domain.Connection{}, err
	}
	return patched, nil
}

// Delete removes only configuration; payloads and credentials are caller-owned.
func (s *ConnectionStore) Delete(ctx context.Context, id string) error {
	if err := domain.ValidateConnectionID("connection.id", id); err != nil {
		return err
	}
	return s.change(ctx, func(connections []domain.Connection) ([]domain.Connection, error) {
		for i, connection := range connections {
			if connection.ID == id {
				return append(connections[:i], connections[i+1:]...), nil
			}
		}
		return nil, fmt.Errorf("%w: connection %q", domain.ErrNotFound, id)
	})
}

func (s *ConnectionStore) change(ctx context.Context, change func([]domain.Connection) ([]domain.Connection, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return withFileLock(ctx, s.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		connections, err := s.load()
		if err != nil {
			return err
		}
		connections, err = change(connections)
		if err != nil {
			return err
		}
		if err := validateConnections(connections); err != nil {
			return err
		}
		sortConnections(connections)
		records := make([]connectionRecord, len(connections))
		for i, connection := range connections {
			records[i] = connectionRecord{SchemaVersion: connectionSchemaVersion, Connection: connection}
		}
		data, err := json.MarshalIndent(records, "", "  ")
		if err != nil {
			return fmt.Errorf("fsstore: encode %s: %w", ConnectionsPath, err)
		}
		return writeFileAtomic(s.path, append(data, '\n'))
	})
}

func (s *ConnectionStore) load() ([]domain.Connection, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return []domain.Connection{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fsstore: read %s: %w", ConnectionsPath, err)
	}
	var records []connectionRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&records); err != nil {
		return nil, fmt.Errorf("fsstore: decode %s: %w", ConnectionsPath, err)
	}
	if records == nil {
		return nil, fmt.Errorf("fsstore: %s: expected a JSON array, not null", ConnectionsPath)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("fsstore: %s: unexpected trailing data", ConnectionsPath)
	}
	connections := make([]domain.Connection, len(records))
	for i, record := range records {
		if record.SchemaVersion != connectionSchemaVersion {
			return nil, fmt.Errorf("fsstore: %s: entry %d: unsupported schemaVersion %d", ConnectionsPath, i, record.SchemaVersion)
		}
		connections[i] = record.Connection
	}
	if err := validateConnections(connections); err != nil {
		return nil, fmt.Errorf("fsstore: %s: %w", ConnectionsPath, err)
	}
	sortConnections(connections)
	return connections, nil
}

func validateConnections(connections []domain.Connection) error {
	ids, identities := map[string]bool{}, map[string]bool{}
	for _, connection := range connections {
		if err := connection.Validate(); err != nil {
			return err
		}
		if ids[connection.ID] || identities[connection.Identity()] {
			return fmt.Errorf("%w: duplicate connection ID or identity at %q", domain.ErrConflict, connection.ID)
		}
		ids[connection.ID], identities[connection.Identity()] = true, true
	}
	return nil
}

func sortConnections(connections []domain.Connection) {
	sort.Slice(connections, func(i, j int) bool { return connections[i].ID < connections[j].ID })
}
