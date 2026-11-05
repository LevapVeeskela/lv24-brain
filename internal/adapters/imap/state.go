package imap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const uidStateVersion = 1

// UIDState is the per-account, per-folder IMAP cursor file.
type UIDState struct {
	Version  int                     `json:"version"`
	Accounts map[string]AccountState `json:"accounts"`
}

// AccountState groups folder cursors for one account.
type AccountState struct {
	Folders map[string]FolderCursor `json:"folders"`
}

// FolderCursor is the last fetched UID and the UIDVALIDITY it belongs to.
type FolderCursor struct {
	UIDValidity uint32    `json:"uid_validity"`
	LastUID     uint32    `json:"last_uid"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// UIDStateStore loads and atomically saves the cursor file. An empty path
// keeps state in memory for one connector run only.
type UIDStateStore struct {
	path string
	mu   sync.Mutex
}

// NewUIDStateStore builds a cursor store. An empty path disables persistence.
func NewUIDStateStore(path string) *UIDStateStore {
	return &UIDStateStore{path: path}
}

// Load reads the cursor file. A missing file is an empty state.
func (s *UIDStateStore) Load() (UIDState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path == "" {
		return emptyUIDState(), nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyUIDState(), nil
		}
		return UIDState{}, fmt.Errorf("read imap state: %w", err)
	}
	var state UIDState
	if err := json.Unmarshal(raw, &state); err != nil {
		return UIDState{}, fmt.Errorf("parse imap state: %w", err)
	}
	if state.Version == 0 {
		state.Version = uidStateVersion
	}
	if state.Accounts == nil {
		state.Accounts = map[string]AccountState{}
	}
	for name, account := range state.Accounts {
		if account.Folders == nil {
			account.Folders = map[string]FolderCursor{}
			state.Accounts[name] = account
		}
	}
	return state, nil
}

// Save atomically writes the cursor file with owner-only permissions.
func (s *UIDStateStore) Save(state UIDState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path == "" {
		return nil
	}
	state.Version = uidStateVersion
	if state.Accounts == nil {
		state.Accounts = map[string]AccountState{}
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode imap state: %w", err)
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create imap state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".imap-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create imap state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set imap state permissions: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write imap state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close imap state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace imap state: %w", err)
	}
	return nil
}

func emptyUIDState() UIDState {
	return UIDState{Version: uidStateVersion, Accounts: map[string]AccountState{}}
}

func accountState(state UIDState, account string) AccountState {
	existing, ok := state.Accounts[account]
	if !ok || existing.Folders == nil {
		existing = AccountState{Folders: map[string]FolderCursor{}}
	}
	return existing
}

func setAccountState(state *UIDState, account string, value AccountState) {
	if state.Accounts == nil {
		state.Accounts = map[string]AccountState{}
	}
	state.Accounts[account] = value
}
