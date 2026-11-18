package agentcontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/localapi"
)

// Agent hook status mirrors `brain hooks status --agents` without importing the
// CLI package: the API reads the same hook files, trust baseline and liveness
// log that the command writes. It is read-only and never edits an agent config.

// hookStatusService reports the installed state of each agent host.
type hookStatusService struct {
	home    string
	dataDir string
}

func newHookStatusService(home, dataDir string) *hookStatusService {
	return &hookStatusService{home: home, dataDir: dataDir}
}

// hookStatusHosts lists the supported hosts in report order.
var hookStatusHosts = []string{"claude", "codex", "opencode"}

const (
	hookTrustFileName         = "agent-hooks.json"
	hookLogFileName           = "hooks.log"
	codexHookApprove          = "codex: re-approve the brain hooks in Codex (/hooks) - a changed command is not trusted"
	hookStatusInstalledDetail = "ok"
)

// hookResourcePath is the resolved agent config file for one host.
func hookResourcePath(home, host string) string {
	switch host {
	case "claude":
		return filepath.Join(home, ".claude", "settings.json")
	case "codex":
		return filepath.Join(home, ".codex", "hooks.json")
	case "opencode":
		opencodeDir := filepath.Join(home, ".config", "opencode")
		session := filepath.Join(opencodeDir, "plugins", "brain-session-end.js")
		if _, err := os.Stat(session); err == nil {
			return session
		}
		return filepath.Join(opencodeDir, "plugins", "brain-handoff.js")
	}
	return ""
}

// Hooks reports installed, trusted and last-run state per agent host.
func (s *hookStatusService) Hooks(ctx context.Context) ([]localapi.HookStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	home := strings.TrimSpace(s.home)
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = resolved
	}
	trust := loadHookTrust(s.dataDir)
	runs := readHookRuns(s.dataDir)
	statuses := make([]localapi.HookStatus, 0, len(hookStatusHosts))
	for _, host := range hookStatusHosts {
		statuses = append(statuses, hookHostStatus(home, host, trust, runs))
	}
	return statuses, nil
}

func hookHostStatus(home, host string, trust hookTrust, runs []hookRun) localapi.HookStatus {
	path := hookResourcePath(home, host)
	status := localapi.HookStatus{Host: host, Path: path}
	status.Installed, status.Detail = hookInstallState(path, host)
	if host == "codex" {
		status.TrustHash = hookFileHash(path, host)
		status.Trusted, status.Trust = hookTrustStatus(status.TrustHash, trust)
		if status.Trust != "approved" {
			status.TrustDrift = status.TrustHash != ""
			status.Approve = codexHookApprove
		}
	}
	if run := lastHookRun(runs, host); run != nil {
		formatted := run.Time.UTC().Format(time.RFC3339)
		status.LastRun = &formatted
		status.LastEvent = run.Event
		status.LastError = run.Error
	}
	return status
}

// hookInstallState reports whether a host file carries brain hooks and whether
// every expected event is present.
func hookInstallState(path, host string) (bool, string) {
	if host == "opencode" {
		return openCodePluginInstallState(path)
	}
	lines, err := hookCommandLines(path, host)
	if err != nil {
		return false, "unreadable: " + err.Error()
	}
	if len(lines) == 0 {
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			return false, "not installed: " + filepath.Base(path) + " not found"
		}
		return false, "no brain hooks in " + filepath.Base(path)
	}
	present := make(map[string]bool, len(lines))
	for _, line := range lines {
		event, _, _ := strings.Cut(line, "\x00")
		present[event] = true
	}
	for _, event := range hookExpectedEvents(host) {
		if !present[event] {
			return true, "incomplete (missing " + event + "); run brain agents sync --hooks"
		}
	}
	return true, hookStatusInstalledDetail
}

func hookExpectedEvents(host string) []string {
	switch host {
	case "claude":
		return []string{"SessionStart", "SessionEnd"}
	case "codex":
		return []string{"SessionStart", "Stop", "SessionEnd"}
	}
	return nil
}

// openCodePluginInstallState checks the plugin file the same way the CLI does:
// it exists, belongs to brain and wires a session end hook.
func openCodePluginInstallState(path string) (bool, string) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "not installed: plugin not found"
	}
	if err != nil {
		return false, "unreadable: " + err.Error()
	}
	text := string(data)
	switch {
	case !strings.Contains(text, "brain"):
		return false, "not a brain plugin"
	case !strings.Contains(text, `"end"`) || !strings.Contains(text, "session"):
		return false, "plugin has no session end hook"
	}
	return true, hookStatusInstalledDetail
}

// hookCommandLines returns "event\x00command" for every brain hook of one host,
// matching the CLI's installation check.
func hookCommandLines(path, host string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	rawHooks, ok := root["hooks"]
	if !ok {
		return nil, nil
	}
	var hooks map[string][]struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(rawHooks, &hooks); err != nil {
		return nil, err
	}
	events := make([]string, 0, len(hooks))
	for event := range hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	var lines []string
	for _, event := range events {
		for _, group := range hooks[event] {
			for _, hook := range group.Hooks {
				if !strings.Contains(hook.Command, " --hook "+host) {
					continue
				}
				lines = append(lines, event+"\x00"+hook.Command)
			}
		}
	}
	return lines, nil
}

// hookFileHash fingerprints the brain commands of one host. It matches the
// format the sync command stores in index/agent-hooks.json.
func hookFileHash(path, host string) string {
	lines, err := hookCommandLines(path, host)
	if err != nil || len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// hookTrust is the stored approval baseline of one data directory.
type hookTrust struct {
	Hosts map[string]struct {
		Hash string `json:"hash"`
	} `json:"hosts"`
}

func loadHookTrust(dataDir string) hookTrust {
	trust := hookTrust{Hosts: map[string]struct {
		Hash string `json:"hash"`
	}{}}
	if strings.TrimSpace(dataDir) == "" {
		return trust
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, "index", hookTrustFileName))
	if err != nil {
		return trust
	}
	var loaded hookTrust
	if json.Unmarshal(raw, &loaded) != nil || loaded.Hosts == nil {
		return trust
	}
	return loaded
}

// hookTrustStatus maps the stored baseline and the on-disk hash to the trust
// column; only Codex keeps a trust store.
func hookTrustStatus(onDisk string, trust hookTrust) (*bool, string) {
	approved, denied := true, false
	switch {
	case onDisk == "":
		return &denied, "not installed"
	case trust.Hosts["codex"].Hash == "":
		return &denied, "unknown (no baseline; run brain agents sync --hooks)"
	case trust.Hosts["codex"].Hash == onDisk:
		return &approved, "approved"
	default:
		return &denied, "drift"
	}
}

// hookRun is one parsed line of index/hooks.log.
type hookRun struct {
	Time  time.Time `json:"time"`
	Host  string    `json:"host"`
	Event string    `json:"event"`
	Error string    `json:"error,omitempty"`
}

// readHookRuns reads the liveness log best effort, oldest first.
func readHookRuns(dataDir string) []hookRun {
	if strings.TrimSpace(dataDir) == "" {
		return nil
	}
	path := filepath.Join(dataDir, "index", hookLogFileName)
	runs := readHookRunsFile(path + ".1")
	return append(runs, readHookRunsFile(path)...)
}

func readHookRunsFile(path string) []hookRun {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var runs []hookRun
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var run hookRun
		if json.Unmarshal(line, &run) != nil || run.Host == "" {
			continue
		}
		runs = append(runs, run)
	}
	return runs
}

// lastHookRun returns the newest log entry of one host.
func lastHookRun(runs []hookRun, host string) *hookRun {
	var last *hookRun
	for i := range runs {
		if runs[i].Host != host {
			continue
		}
		if last == nil || !runs[i].Time.Before(last.Time) {
			last = &runs[i]
		}
	}
	return last
}
