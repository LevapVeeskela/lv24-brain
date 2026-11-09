package fsstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	checkpointsDir       = "checkpoints"
	checkpointFileExt    = ".json"
	checkpointHashLength = 12
)

// CheckpointStore keeps opaque per-stream checkpoints as
// connectors/<connectionID>/checkpoints/<escaped stream>.json, each holding
// {"stream","version","data"} with data base64-encoded. Put replaces the file
// through a temporary file and rename, so a crash leaves the old or the new
// checkpoint, never a mix.
//
// Stream names are case-sensitive ("INBOX" and "inbox" are different
// streams) while Windows and macOS file systems usually are not, so the file
// name is the lowercased stream plus a short hash of the exact stream:
// "INBOX" -> "inbox-<12 hex>.json". Two streams therefore never share a file.
type CheckpointStore struct {
	store *Store
}

var _ ports.CheckpointStore = (*CheckpointStore)(nil)

// Checkpoints returns the checkpoint store of this data directory.
func (s *Store) Checkpoints() *CheckpointStore { return &CheckpointStore{store: s} }

type checkpointFile struct {
	Stream  string `json:"stream"`
	Version int    `json:"version"`
	Data    []byte `json:"data"`
}

// Get returns domain.ErrNotFound when the stream has no checkpoint.
func (s *CheckpointStore) Get(ctx context.Context, connectionID, stream string) (domain.Checkpoint, error) {
	if err := contextError(ctx); err != nil {
		return domain.Checkpoint{}, err
	}
	path, err := s.checkpointPath(connectionID, stream)
	if err != nil {
		return domain.Checkpoint{}, err
	}
	checkpoint, err := readCheckpointFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return domain.Checkpoint{}, notFound("checkpoint", connectionID+"/"+stream)
	}
	if err != nil {
		return domain.Checkpoint{}, err
	}
	if checkpoint.Stream != stream {
		return domain.Checkpoint{}, domain.NewValidationError("checkpoint.stream",
			fmt.Sprintf("%s holds stream %q, not %q", filepath.Base(path), checkpoint.Stream, stream))
	}
	return checkpoint, nil
}

// List returns every checkpoint of a connection ordered by stream. Files that
// do not parse or whose name does not match their stream are skipped.
func (s *CheckpointStore) List(ctx context.Context, connectionID string) ([]domain.Checkpoint, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	dir, err := s.checkpointsDirFor(connectionID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []domain.Checkpoint{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fsstore: list checkpoints of %s: %w", connectionID, err)
	}
	checkpoints := []domain.Checkpoint{}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || strings.HasPrefix(name, tempFilePrefix) || !strings.HasSuffix(name, checkpointFileExt) {
			continue
		}
		checkpoint, err := readCheckpointFile(filepath.Join(dir, name))
		if err != nil || checkpointFileName(checkpoint.Stream) != name {
			continue
		}
		checkpoints = append(checkpoints, checkpoint)
	}
	sort.Slice(checkpoints, func(i, j int) bool { return checkpoints[i].Stream < checkpoints[j].Stream })
	return checkpoints, nil
}

// Put replaces one checkpoint atomically; Data is stored byte-for-byte.
func (s *CheckpointStore) Put(ctx context.Context, connectionID string, checkpoint domain.Checkpoint) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	path, err := s.checkpointPath(connectionID, checkpoint.Stream)
	if err != nil {
		return err
	}
	data := checkpoint.Data
	if data == nil {
		data = []byte{}
	}
	raw, err := json.MarshalIndent(checkpointFile{Stream: checkpoint.Stream, Version: checkpoint.Version, Data: data}, "", "  ")
	if err != nil {
		return fmt.Errorf("fsstore: encode checkpoint: %w", err)
	}
	if err := s.store.Items().ensureConnectionIgnore(connectionID); err != nil {
		return err
	}
	return writeFileAtomic(path, append(raw, '\n'))
}

// Delete removes one stream's checkpoint; an empty stream removes all of the
// connection's checkpoints. Missing checkpoints are not an error.
func (s *CheckpointStore) Delete(ctx context.Context, connectionID, stream string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if stream == "" {
		dir, err := s.checkpointsDirFor(connectionID)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("fsstore: delete checkpoints of %s: %w", connectionID, err)
		}
		return nil
	}
	path, err := s.checkpointPath(connectionID, stream)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fsstore: delete checkpoint %s/%s: %w", connectionID, stream, err)
	}
	return nil
}

func (s *CheckpointStore) checkpointsDirFor(connectionID string) (string, error) {
	dir, err := s.store.connectionDir(connectionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, checkpointsDir), nil
}

func (s *CheckpointStore) checkpointPath(connectionID, stream string) (string, error) {
	if err := domain.ValidateStream("checkpoint.stream", stream); err != nil {
		return "", err
	}
	dir, err := s.checkpointsDirFor(connectionID)
	if err != nil {
		return "", err
	}
	return s.store.ensureInsideRoot(filepath.Join(dir, checkpointFileName(stream)))
}

// checkpointFileName is the case-collision-safe file name of a stream.
func checkpointFileName(stream string) string {
	sum := sha256.Sum256([]byte(stream))
	return strings.ToLower(stream) + "-" + hex.EncodeToString(sum[:])[:checkpointHashLength] + checkpointFileExt
}

func readCheckpointFile(path string) (domain.Checkpoint, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.Checkpoint{}, err
	}
	var file checkpointFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return domain.Checkpoint{}, fmt.Errorf("fsstore: decode checkpoint %s: %w", filepath.Base(path), err)
	}
	checkpoint := domain.Checkpoint{Stream: file.Stream, Version: file.Version, Data: file.Data}
	if err := checkpoint.Validate(); err != nil {
		return domain.Checkpoint{}, err
	}
	return checkpoint, nil
}
