package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Codex fires Stop after every turn and SessionEnd (when it fires) without
// the last message, so the handoff is built in two steps: Stop saves the
// latest assistant message as a checkpoint, SessionEnd turns it into the
// handoff. Checkpoints left behind (SessionEnd missing) are flushed by the
// next `session start --hook codex` once they are older than
// staleCheckpointAge. Checkpoints live under the gitignored index/ directory,
// so they never sync.
const staleCheckpointAge = 15 * time.Minute

type codexCheckpoint struct {
	SessionID string    `json:"session_id"`
	Cwd       string    `json:"cwd"`
	Message   string    `json:"message"`
	At        time.Time `json:"at"`
}

var safeSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func checkpointDir(cfgPath string) (string, error) {
	cfg, err := loadRequiredConfig(cfgPath)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		return "", errors.New("codex checkpoints need local or git mode")
	}
	return filepath.Join(cfg.DataDir, "index", "sessions"), nil
}

func checkpointPath(dir, sessionID string) string {
	name := sessionID
	if !safeSessionID.MatchString(name) {
		sum := sha256.Sum256([]byte(sessionID))
		name = hex.EncodeToString(sum[:16])
	}
	return filepath.Join(dir, "codex-"+name+".json")
}

// codexSessionEnd handles both Codex events that run `session end`.
func codexSessionEnd(ctx context.Context, opts sessionOptions, stdout io.Writer) error {
	dir, err := checkpointDir(opts.cfgPath)
	if err != nil {
		return err
	}
	sessionID := strings.TrimSpace(opts.input.SessionID)
	if opts.input.HookEventName == "Stop" {
		message := strings.TrimSpace(opts.input.LastAssistantMessage)
		if sessionID == "" || message == "" {
			return nil
		}
		return saveCheckpoint(checkpointPath(dir, sessionID), codexCheckpoint{
			SessionID: sessionID, Cwd: opts.cwd, Message: message, At: time.Now().UTC(),
		})
	}
	// SessionEnd (or a manual run): flush this session's checkpoint.
	if sessionID == "" {
		return sessionEnd(ctx, opts, stdout)
	}
	return flushCheckpoint(ctx, opts, checkpointPath(dir, sessionID), stdout)
}

// flushStaleCodexCheckpoints writes handoffs for sessions whose SessionEnd
// never arrived. Errors are ignored: a session start must not fail on them.
func flushStaleCodexCheckpoints(ctx context.Context, opts sessionOptions) {
	dir, err := checkpointDir(opts.cfgPath)
	if err != nil {
		return
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "codex-*.json"))
	current := checkpointPath(dir, strings.TrimSpace(opts.input.SessionID))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || path == current || time.Since(info.ModTime()) < staleCheckpointAge {
			continue
		}
		_ = flushCheckpoint(ctx, opts, path, io.Discard)
	}
}

func flushCheckpoint(ctx context.Context, opts sessionOptions, path string, stdout io.Writer) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var checkpoint codexCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		_ = os.Remove(path)
		return err
	}
	flush := opts
	flush.input.LastAssistantMessage = checkpoint.Message
	if checkpoint.Cwd != "" {
		flush.cwd = checkpoint.Cwd
	}
	if err := sessionEnd(ctx, flush, stdout); err != nil {
		return err
	}
	return os.Remove(path)
}

func saveCheckpoint(path string, checkpoint codexCheckpoint) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
