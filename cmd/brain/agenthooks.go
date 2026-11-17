package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/integrations"
)

// agentHookBackupSuffix names the one-time backup written before the first
// modification of an existing agent config file.
const agentHookBackupSuffix = ".pre-brain-hooks"

// agentHookTimeout is the timeout in seconds written for every brain hook.
const agentHookTimeout = 10

// codexHooksNote tells the owner that Codex keeps new hooks disabled until
// they are trusted. The command never edits the Codex trust store.
const codexHooksNote = "codex: approve the new brain hooks in Codex (hooks are disabled until trusted)"

// codexReapproveNote is printed when the hook command changed, so the old
// approval no longer applies and Codex will skip the hooks until re-approved.
const codexReapproveNote = "codex: re-approve the brain hooks in Codex (/hooks) - a changed command is not trusted"

// agentHookHosts lists the supported agent hosts in report and install order.
var agentHookHosts = []string{"claude", "codex", "opencode"}

// hostFlag collects repeated --host values for "brain agents sync".
type hostFlag []string

func (h *hostFlag) String() string { return strings.Join(*h, ",") }

func (h *hostFlag) Set(value string) error {
	host := strings.ToLower(strings.TrimSpace(value))
	if !containsString(agentHookHosts, host) {
		return fmt.Errorf("unknown host %q (want claude, codex or opencode)", value)
	}
	if !containsString(*h, host) {
		*h = append(*h, host)
	}
	return nil
}

// agentHookSyncOptions carries one "brain agents sync --hooks" run.
type agentHookSyncOptions struct {
	home    string
	exe     string
	cfg     string
	dataDir string
	check   bool
	// hosts limits the targets and installs a host even when its config
	// directory is absent (an explicit selection).
	hosts []string
}

// agentHookBuildFunc returns the new file content for orig (nil when the file is
// missing) and whether it differs from orig. A build that reports no change
// must not return data.
type agentHookBuildFunc func(orig []byte) (data []byte, changed bool, err error)

// agentHookSpec is one brain hook: an event key and the "session <subcmd> --hook
// <agent>" command that runs under it.
type agentHookSpec struct {
	event  string
	subcmd string
	agent  string
}

// agentHookTarget is one agent config file that receives brain hooks.
type agentHookTarget struct {
	host string
	path string
	// dir must exist; otherwise the agent is not installed and the target is skipped.
	dir       string
	dirReason string
	// createIfMissing creates path when it is absent; otherwise a missing file is skipped.
	createIfMissing bool
	missingReason   string
	// specs are the JSON hook hosts (Claude Code, Codex); OpenCode uses build only.
	specs []agentHookSpec
	build agentHookBuildFunc
	// note is printed after a successful write.
	note string
}

// agentHookOutcome is the planned result for one hook target before any write.
type agentHookOutcome struct {
	status  string
	reason  string
	data    []byte
	perm    os.FileMode
	existed bool
	orig    []byte
}

// agentHookTargets lists the Claude Code, Codex and OpenCode files for a home
// directory. exe and cfg must be absolute paths.
func agentHookTargets(home, exe, cfg string) []agentHookTarget {
	claudeDir := filepath.Join(home, ".claude")
	codexDir := filepath.Join(home, ".codex")
	opencodeDir := filepath.Join(home, ".config", "opencode")
	claudeSpecs := []agentHookSpec{
		{event: "SessionStart", subcmd: "start", agent: "claude"},
		{event: "SessionEnd", subcmd: "end", agent: "claude"},
	}
	codexSpecs := []agentHookSpec{
		{event: "SessionStart", subcmd: "start", agent: "codex"},
		{event: "Stop", subcmd: "end", agent: "codex"},
		{event: "SessionEnd", subcmd: "end", agent: "codex"},
	}
	return []agentHookTarget{
		{
			host:            "claude",
			path:            filepath.Join(claudeDir, "settings.json"),
			dir:             claudeDir,
			dirReason:       "Claude Code not installed",
			createIfMissing: true,
			missingReason:   "Claude Code not installed: settings.json not found",
			specs:           claudeSpecs,
			build:           jsonHookBuild(exe, cfg, claudeSpecs),
		},
		{
			host:            "codex",
			path:            filepath.Join(codexDir, "hooks.json"),
			dir:             codexDir,
			dirReason:       "Codex not installed",
			createIfMissing: true,
			missingReason:   "Codex not installed: hooks.json not found",
			specs:           codexSpecs,
			build:           jsonHookBuild(exe, cfg, codexSpecs),
			note:            codexHooksNote,
		},
		{
			host:            "opencode",
			path:            openCodePluginPath(opencodeDir),
			dir:             opencodeDir,
			dirReason:       "OpenCode not installed",
			createIfMissing: true,
			missingReason:   "OpenCode not installed: plugin not found",
			build:           openCodePluginBuild(exe, cfg),
		},
	}
}

// syncAgentHooks installs or refreshes the brain hooks. It returns 1 when a
// file failed, or when check is set and any file would change.
func syncAgentHooks(opts agentHookSyncOptions, stdout, stderr io.Writer) int {
	exit := 0
	selected := func(host string) bool {
		return len(opts.hosts) == 0 || containsString(opts.hosts, host)
	}
	trust := loadAgentHookTrust(opts.dataDir)
	trustChanged := false
	for _, target := range agentHookTargets(opts.home, opts.exe, opts.cfg) {
		if !selected(target.host) {
			fmt.Fprintf(stdout, "skipped (not selected: %s) %s\n", target.host, target.path)
			continue
		}
		out, err := planAgentHook(target, len(opts.hosts) > 0)
		if err != nil {
			fmt.Fprintf(stderr, "brain agents: hooks: %s: %v\n", target.path, err)
			exit = 1
			continue
		}
		switch out.status {
		case agentsSkipped:
			if opts.check {
				fmt.Fprintf(stdout, "not installed (%s) %s\n", out.reason, target.path)
			} else {
				fmt.Fprintf(stdout, "skipped (%s) %s\n", out.reason, target.path)
			}
			continue
		case agentsUnchanged:
			if opts.check {
				fmt.Fprintf(stdout, "installed %s\n", target.path)
			} else {
				fmt.Fprintf(stdout, "%s %s\n", agentsUnchanged, target.path)
			}
		}

		desired := agentHookTrustHash(target, opts.exe, opts.cfg)
		if target.host == "codex" {
			for _, warning := range codexTrustWarnings(target.path, desired, trust) {
				fmt.Fprintln(stderr, "brain agents: "+warning)
			}
		}
		if out.status != agentsUnchanged {
			if opts.check {
				exit = 1
				verb := "update"
				if !out.existed {
					verb = "create"
				}
				fmt.Fprintf(stdout, "would %s %s\n", verb, target.path)
				continue
			}
			if err := writeAgentHook(target.path, out); err != nil {
				fmt.Fprintf(stderr, "brain agents: hooks: %s: %v\n", target.path, err)
				exit = 1
				continue
			}
			fmt.Fprintf(stdout, "%s %s\n", agentsUpdated, target.path)
			if target.note != "" {
				fmt.Fprintln(stdout, target.note)
			}
		}
		if opts.check || opts.dataDir == "" || target.host == "opencode" || desired == "" {
			continue
		}
		if trust.Hosts[target.host].Hash != desired {
			trustChanged = true
		}
		trust.Hosts[target.host] = agentHookTrustRecord{
			Path:      target.path,
			Hash:      desired,
			UpdatedAt: time.Now().UTC(),
		}
	}
	if trustChanged {
		_ = saveAgentHookTrust(opts.dataDir, trust)
	}
	return exit
}

// planAgentHook computes the result for one target without writing anything.
// force skips the install-directory check for an explicitly selected host.
func planAgentHook(t agentHookTarget, force bool) (agentHookOutcome, error) {
	if !force {
		info, err := os.Stat(t.dir)
		switch {
		case errors.Is(err, os.ErrNotExist), err == nil && !info.IsDir():
			return agentHookOutcome{status: agentsSkipped, reason: t.dirReason}, nil
		case err != nil:
			return agentHookOutcome{}, err
		}
	}

	orig, err := os.ReadFile(t.path)
	existed := err == nil
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !t.createIfMissing {
			return agentHookOutcome{status: agentsSkipped, reason: t.missingReason}, nil
		}
	case err != nil:
		return agentHookOutcome{}, err
	}

	perm := os.FileMode(0o644)
	if existed {
		fi, err := os.Stat(t.path)
		if err != nil {
			return agentHookOutcome{}, err
		}
		perm = fi.Mode().Perm()
	}

	body, hasBOM := stripUTF8BOM(orig)
	data, changed, err := t.build(body)
	if err != nil {
		return agentHookOutcome{}, err
	}
	if !changed {
		return agentHookOutcome{status: agentsUnchanged}, nil
	}
	if hasBOM {
		data = append([]byte(agentsUTF8BOM), data...)
	}
	return agentHookOutcome{status: agentsUpdated, data: data, perm: perm, existed: existed, orig: orig}, nil
}

// writeAgentHook backs up an existing file once, then replaces it atomically.
func writeAgentHook(path string, out agentHookOutcome) error {
	if out.existed {
		if err := backupAgentHookOnce(path, out.orig, out.perm); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return agentsWriteFile(path, out.data, out.perm)
}

// backupAgentHookOnce writes the original bytes next to the file unless a
// backup already exists, so the backup always holds the pre-brain content.
func backupAgentHookOnce(path string, orig []byte, perm os.FileMode) error {
	backup := path + agentHookBackupSuffix
	_, err := os.Stat(backup)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrNotExist):
		return os.WriteFile(backup, orig, perm)
	default:
		return err
	}
}

// jsonHookBuild returns a build function that ensures every spec has a brain
// command, keeping all other content and key order.
func jsonHookBuild(exe, cfg string, specs []agentHookSpec) agentHookBuildFunc {
	return func(orig []byte) ([]byte, bool, error) {
		root, err := parseJSONObject(orig)
		if err != nil {
			return nil, false, err
		}
		changed := false
		for _, spec := range specs {
			ok, err := upsertBrainHook(root, spec, brainHookCommand(exe, cfg, spec))
			if err != nil {
				return nil, false, err
			}
			changed = changed || ok
		}
		if !changed {
			return nil, false, nil
		}
		return encodeJSONObject(root), true, nil
	}
}

// brainHookCommand builds the hook command. The exe is always double-quoted;
// the config is quoted only when it contains whitespace.
func brainHookCommand(exe, cfg string, spec agentHookSpec) string {
	cfgArg := forwardSlashes(cfg)
	if strings.ContainsAny(cfgArg, " \t") {
		cfgArg = `"` + cfgArg + `"`
	}
	return fmt.Sprintf(`"%s" session %s --hook %s --config %s`,
		forwardSlashes(exe), spec.subcmd, spec.agent, cfgArg)
}

// agentHookMarker identifies a brain hook command regardless of exe and config paths.
func agentHookMarker(spec agentHookSpec) string {
	return " session " + spec.subcmd + " --hook " + spec.agent
}

// upsertBrainHook updates the brain command in place when one exists under
// the event, or appends a new brain group. It reports whether anything changed.
func upsertBrainHook(root *jObj, spec agentHookSpec, command string) (bool, error) {
	hooks, err := jsonObjectMember(root, "hooks")
	if err != nil {
		return false, err
	}
	groups, err := jsonArrayMember(hooks, spec.event)
	if err != nil {
		return false, err
	}
	marker := agentHookMarker(spec)
	for _, g := range groups {
		group, ok := g.(*jObj)
		if !ok {
			continue
		}
		inner, ok := group.vals["hooks"].(jArr)
		if !ok {
			continue
		}
		for _, h := range inner {
			hook, ok := h.(*jObj)
			if !ok {
				continue
			}
			cur, ok := hook.vals["command"].(jStr)
			if !ok || !strings.Contains(string(cur), marker) {
				continue
			}
			if string(cur) == command {
				return false, nil
			}
			hook.set("command", jStr(command))
			return true, nil
		}
	}
	hooks.set(spec.event, append(groups, newBrainHookGroup(command)))
	return true, nil
}

func newBrainHookGroup(command string) *jObj {
	hook := newJObj()
	hook.set("type", jStr("command"))
	hook.set("command", jStr(command))
	hook.set("timeout", jRaw(strconv.Itoa(agentHookTimeout)))
	group := newJObj()
	group.set("hooks", jArr{hook})
	return group
}

// openCodePluginPath prefers an existing brain-session-end.js plugin and
// otherwise uses brain-handoff.js.
func openCodePluginPath(opencodeDir string) string {
	session := filepath.Join(opencodeDir, "plugins", "brain-session-end.js")
	if _, err := os.Stat(session); err == nil {
		return session
	}
	return filepath.Join(opencodeDir, "plugins", "brain-handoff.js")
}

func openCodePluginBuild(exe, cfg string) agentHookBuildFunc {
	rendered := renderOpenCodePlugin(integrations.BrainHandoffPlugin,
		forwardSlashes(exe), forwardSlashes(cfg))
	return func(orig []byte) ([]byte, bool, error) {
		data := []byte(rendered)
		return data, !bytes.Equal(orig, data), nil
	}
}

// renderOpenCodePlugin substitutes the placeholders with values escaped for a
// JavaScript double-quoted string.
func renderOpenCodePlugin(tmpl, bin, cfg string) string {
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return strings.NewReplacer(
		"__BRAIN_BIN__", esc.Replace(bin),
		"__BRAIN_CONFIG__", esc.Replace(cfg),
	).Replace(tmpl)
}

func stripUTF8BOM(data []byte) ([]byte, bool) {
	if bytes.HasPrefix(data, []byte(agentsUTF8BOM)) {
		return data[len(agentsUTF8BOM):], true
	}
	return data, false
}

// jObj is a JSON object that remembers key order. Keys are unique.
type jObj struct {
	keys []string
	vals map[string]any
}

// jArr is a JSON array. Elements are *jObj, jArr, jStr or jRaw.
type jArr []any

// jStr is a decoded JSON string.
type jStr string

// jRaw is a JSON number, bool or null kept in its source text.
type jRaw string

func newJObj() *jObj {
	return &jObj{vals: map[string]any{}}
}

func (o *jObj) set(key string, v any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

func jsonObjectMember(obj *jObj, key string) (*jObj, error) {
	v, ok := obj.vals[key]
	if !ok {
		child := newJObj()
		obj.set(key, child)
		return child, nil
	}
	child, ok := v.(*jObj)
	if !ok {
		return nil, fmt.Errorf("%q is not a JSON object", key)
	}
	return child, nil
}

func jsonArrayMember(obj *jObj, key string) (jArr, error) {
	v, ok := obj.vals[key]
	if !ok {
		arr := jArr{}
		obj.set(key, arr)
		return arr, nil
	}
	arr, ok := v.(jArr)
	if !ok {
		return nil, fmt.Errorf("%q is not a JSON array", key)
	}
	return arr, nil
}

// parseJSONObject decodes a JSON document whose top-level value is an object.
// Empty input is an empty object.
func parseJSONObject(data []byte) (*jObj, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return newJObj(), nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeJSONValue(dec)
	if err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	obj, ok := v.(*jObj)
	if !ok {
		return nil, errors.New("parse JSON: top-level value is not an object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse JSON: unexpected data after top-level value")
	}
	return obj, nil
}

func decodeJSONValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj, err := decodeJSONObject(dec)
			if err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr, err := decodeJSONArray(dec)
			if err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected token %v", t)
	case string:
		return jStr(t), nil
	case json.Number:
		return jRaw(t.String()), nil
	case bool:
		return jRaw(strconv.FormatBool(t)), nil
	case nil:
		return jRaw("null"), nil
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

func decodeJSONObject(dec *json.Decoder) (*jObj, error) {
	obj := newJObj()
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("object key %v is not a string", tok)
		}
		val, err := decodeJSONValue(dec)
		if err != nil {
			return nil, err
		}
		obj.set(key, val)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return obj, nil
}

func decodeJSONArray(dec *json.Decoder) (jArr, error) {
	arr := jArr{}
	for dec.More() {
		val, err := decodeJSONValue(dec)
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return arr, nil
}

// encodeJSONObject renders the object with 2-space indentation and a final newline.
func encodeJSONObject(obj *jObj) []byte {
	var buf bytes.Buffer
	writeJSONValue(&buf, obj, 0)
	buf.WriteByte('\n')
	return buf.Bytes()
}

func writeJSONValue(buf *bytes.Buffer, v any, depth int) {
	switch t := v.(type) {
	case *jObj:
		writeJSONContainer(buf, depth, "{", "}", len(t.keys), func(i int) {
			buf.WriteString(quoteJSON(t.keys[i]))
			buf.WriteString(": ")
			writeJSONValue(buf, t.vals[t.keys[i]], depth+1)
		})
	case jArr:
		writeJSONContainer(buf, depth, "[", "]", len(t), func(i int) {
			writeJSONValue(buf, t[i], depth+1)
		})
	case jStr:
		buf.WriteString(quoteJSON(string(t)))
	case jRaw:
		buf.WriteString(string(t))
	}
}

func writeJSONContainer(buf *bytes.Buffer, depth int, open, end string, n int, item func(i int)) {
	if n == 0 {
		buf.WriteString(open + end)
		return
	}
	buf.WriteString(open + "\n")
	for i := 0; i < n; i++ {
		if i > 0 {
			buf.WriteString(",\n")
		}
		buf.WriteString(strings.Repeat("  ", depth+1))
		item(i)
	}
	buf.WriteString("\n" + strings.Repeat("  ", depth) + end)
}

// quoteJSON encodes a string without HTML escaping, so existing text stays readable.
func quoteJSON(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoding a string cannot fail.
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

// forwardSlashes writes a path with "/" separators on every OS (hook files
// written on Linux may name Windows paths, which filepath.ToSlash keeps).
func forwardSlashes(path string) string {
	return strings.ReplaceAll(path, `\`, "/")
}

// agentHookTrustHash fingerprints the brain commands a hook host would run.
// Codex stores its approval per command, so a changed executable or config
// path invalidates the old approval. OpenCode has no trust store.
func agentHookTrustHash(t agentHookTarget, exe, cfg string) string {
	if t.host == "opencode" || len(t.specs) == 0 {
		return ""
	}
	lines := make([]string, 0, len(t.specs))
	for _, spec := range t.specs {
		lines = append(lines, spec.event+"\x00"+brainHookCommand(exe, cfg, spec))
	}
	return hashHookCommandLines(lines)
}

// hashHookCommandLines returns a stable sha256 over the sorted command set.
func hashHookCommandLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// shortTrustHash renders the first 8 hex digits of a stored trust hash.
func shortTrustHash(hash string) string {
	hexPart := strings.TrimPrefix(hash, "sha256:")
	if len(hexPart) > 8 {
		hexPart = hexPart[:8]
	}
	if hexPart == "" {
		return "unrecorded"
	}
	return "sha256:" + hexPart
}

// codexTrustWarnings reports approval drift for the Codex hook file: a hook
// file edited outside brain, or a new brain command (moved executable or
// config) that Codex has not approved yet.
func codexTrustWarnings(path, desired string, trust agentHookTrustState) []string {
	previous := trust.Hosts["codex"].Hash
	if previous == "" {
		return nil
	}
	onDisk, _ := agentHookFileHash(path, "codex")
	if onDisk != "" && onDisk != previous {
		return []string{fmt.Sprintf(
			"codex: %s changed outside brain (trust hash %s -> %s); %s",
			filepath.Base(path), shortTrustHash(previous), shortTrustHash(onDisk), codexReapproveNote)}
	}
	if desired != "" && desired != previous {
		return []string{fmt.Sprintf(
			"codex: brain hook command changed, so the old approval no longer applies (trust hash %s -> %s); %s",
			shortTrustHash(previous), shortTrustHash(desired), codexReapproveNote)}
	}
	return nil
}

// agentHookFileHash fingerprints the brain hook commands currently stored in a
// JSON hook file, or "" when the file has none.
func agentHookFileHash(path, host string) (string, error) {
	lines, err := agentHookCommandLinesInFile(path, host)
	if err != nil {
		return "", err
	}
	return hashHookCommandLines(lines), nil
}

// agentHookCommandLinesInFile returns "event\x00command" for every brain hook of
// one host in a JSON hook file (Claude Code settings.json or Codex hooks.json).
func agentHookCommandLinesInFile(path, host string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	root, err := parseJSONObject(data)
	if err != nil {
		return nil, err
	}
	hooks, ok := root.vals["hooks"].(*jObj)
	if !ok {
		return nil, nil
	}
	var lines []string
	for _, event := range hooks.keys {
		groups, ok := hooks.vals[event].(jArr)
		if !ok {
			continue
		}
		for _, g := range groups {
			group, ok := g.(*jObj)
			if !ok {
				continue
			}
			inner, ok := group.vals["hooks"].(jArr)
			if !ok {
				continue
			}
			for _, h := range inner {
				hook, ok := h.(*jObj)
				if !ok {
					continue
				}
				command, ok := hook.vals["command"].(jStr)
				if !ok || !strings.Contains(string(command), " --hook "+host) {
					continue
				}
				lines = append(lines, event+"\x00"+string(command))
			}
		}
	}
	return lines, nil
}

// agentHookStateVersion is the schema of index/agent-hooks.json.
const agentHookStateVersion = 1

// agentHookTrustRecord is the last command set brain wrote for one host.
type agentHookTrustRecord struct {
	Path      string    `json:"path"`
	Hash      string    `json:"hash"`
	UpdatedAt time.Time `json:"updated_at"`
}

// agentHookTrustState records the command fingerprints per agent host. It lives
// under the gitignored index/ directory: approvals are per machine.
type agentHookTrustState struct {
	Version int                             `json:"version"`
	Hosts   map[string]agentHookTrustRecord `json:"hosts"`
}

func agentHookTrustPath(dataDir string) string {
	return filepath.Join(dataDir, "index", "agent-hooks.json")
}

// loadAgentHookTrust reads the state best effort: a missing or broken file
// simply means no baseline is known.
func loadAgentHookTrust(dataDir string) agentHookTrustState {
	state := agentHookTrustState{Version: agentHookStateVersion, Hosts: map[string]agentHookTrustRecord{}}
	if strings.TrimSpace(dataDir) == "" {
		return state
	}
	raw, err := os.ReadFile(agentHookTrustPath(dataDir))
	if err != nil {
		return state
	}
	var loaded agentHookTrustState
	if json.Unmarshal(raw, &loaded) != nil || loaded.Hosts == nil {
		return state
	}
	state.Hosts = loaded.Hosts
	return state
}

// saveAgentHookTrust writes the state atomically. Failures are the caller's
// to ignore: a missing baseline never blocks an install.
func saveAgentHookTrust(dataDir string, state agentHookTrustState) error {
	if strings.TrimSpace(dataDir) == "" {
		return nil
	}
	path := agentHookTrustPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	state.Version = agentHookStateVersion
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// agentHookStatusOptions carries one "brain hooks status --agents" run.
type agentHookStatusOptions struct {
	home    string
	exe     string
	cfg     string
	dataDir string
	asJSON  bool
}

// agentHooksStatusReport is the payload of "brain hooks status --agents --json".
type agentHooksStatusReport struct {
	DataDir string                `json:"data_dir"`
	Log     string                `json:"log"`
	Hosts   []agentHookHostStatus `json:"hosts"`
}

// agentHookHostStatus is one agent host as reported by hooks status.
type agentHookHostStatus struct {
	Host       string  `json:"host"`
	Path       string  `json:"path"`
	Installed  bool    `json:"installed"`
	Detail     string  `json:"detail"`
	Trusted    *bool   `json:"trusted"`
	Trust      string  `json:"trust,omitempty"`
	TrustHash  string  `json:"trust_hash,omitempty"`
	TrustDrift bool    `json:"trust_drift,omitempty"`
	Approve    string  `json:"approve,omitempty"`
	LastRun    *string `json:"last_run"`
	LastEvent  string  `json:"last_event,omitempty"`
	LastError  string  `json:"last_error,omitempty"`
}

// runHooksStatusAgents resolves the config and home, then reports the agent
// hooks. It is the "--agents" branch of "brain hooks status".
func runHooksStatusAgents(home, cfgPath string, asJSON bool, stdout, stderr io.Writer) int {
	cfg, err := loadRequiredConfig(cfgPath)
	if err != nil {
		return reportError(stderr, "hooks status", err)
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		return reportError(stderr, "hooks status", errors.New("agent hook status needs local or git mode"))
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return reportError(stderr, "hooks status", err)
	}
	if strings.TrimSpace(home) == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return reportError(stderr, "hooks status", err)
		}
	}
	if home, err = filepath.Abs(home); err != nil {
		return reportError(stderr, "hooks status", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return reportError(stderr, "hooks status", err)
	}
	cfgAbs, err := filepath.Abs(cfgPath)
	if err != nil {
		return reportError(stderr, "hooks status", err)
	}
	return runAgentHooksStatus(agentHookStatusOptions{
		home:    home,
		exe:     exe,
		cfg:     cfgAbs,
		dataDir: dataDir,
		asJSON:  asJSON,
	}, stdout, stderr)
}

// runAgentHooksStatus reports installed, trusted and last-run state per host.
func runAgentHooksStatus(opts agentHookStatusOptions, stdout, stderr io.Writer) int {
	trust := loadAgentHookTrust(opts.dataDir)
	runs := readHookLog(opts.dataDir)
	report := agentHooksStatusReport{
		DataDir: opts.dataDir,
		Log:     hookLogPath(opts.dataDir),
		Hosts:   make([]agentHookHostStatus, 0, len(agentHookHosts)),
	}
	for _, target := range agentHookTargets(opts.home, opts.exe, opts.cfg) {
		status := agentHookHostStatus{Host: target.host, Path: target.path}
		status.Installed, status.Detail = agentHookInstallState(target, opts.exe, opts.cfg)
		if target.host == "codex" {
			status.TrustHash, _ = agentHookFileHash(target.path, "codex")
			status.Trusted, status.Trust = agentHookTrustStatus(status.TrustHash, trust)
			if status.Trust != "approved" {
				status.TrustDrift = status.TrustHash != ""
				status.Approve = codexReapproveNote
			}
		}
		if run, errText, event := lastHookRun(runs, target.host); run != nil {
			formatted := run.UTC().Format(time.RFC3339)
			status.LastRun = &formatted
			status.LastError = errText
			status.LastEvent = event
		}
		report.Hosts = append(report.Hosts, status)
	}

	if opts.asJSON {
		if err := writeJSON(stdout, report); err != nil {
			return reportError(stderr, "hooks status", err)
		}
		return 0
	}
	writeAgentHooksStatusText(stdout, report)
	return 0
}

// agentHookTrustStatus maps the stored baseline and the on-disk hash to the
// trust column.
func agentHookTrustStatus(onDisk string, trust agentHookTrustState) (*bool, string) {
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

// agentHookInstallState reports whether a host carries the brain hooks and
// whether their commands still match this binary and config.
func agentHookInstallState(t agentHookTarget, exe, cfg string) (bool, string) {
	if t.host == "opencode" {
		return openCodePluginInstallState(t.path)
	}
	lines, err := agentHookCommandLinesInFile(t.path, t.host)
	if err != nil {
		return false, "unreadable: " + err.Error()
	}
	if len(lines) == 0 {
		return false, "no brain hooks in " + filepath.Base(t.path)
	}
	for _, spec := range t.specs {
		found := false
		for _, line := range lines {
			event, command, _ := strings.Cut(line, "\x00")
			if event != spec.event {
				continue
			}
			found = true
			if command != brainHookCommand(exe, cfg, spec) {
				return true, "stale command (binary or config moved); run brain agents sync --hooks"
			}
		}
		if !found {
			return true, "incomplete (missing " + spec.event + "); run brain agents sync --hooks"
		}
	}
	return true, "ok"
}

func openCodePluginInstallState(path string) (bool, string) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "plugin not found"
	}
	if err != nil {
		return false, "unreadable: " + err.Error()
	}
	text := string(data)
	if !strings.Contains(text, "brain") {
		return false, "not a brain plugin"
	}
	if !strings.Contains(text, `"end"`) || !strings.Contains(text, "session") {
		return false, "plugin has no session end hook"
	}
	return true, "ok"
}

// writeAgentHooksStatusText prints one line per host, then the approval hint.
func writeAgentHooksStatusText(w io.Writer, report agentHooksStatusReport) {
	location := report.DataDir
	if location == "" {
		location = "no data directory"
	}
	fmt.Fprintf(w, "hook hosts (data %s)\n", location)
	needsApproval := false
	for _, host := range report.Hosts {
		state := "not installed (" + host.Detail + ")"
		if host.Installed {
			state = "installed"
			if host.Detail != "ok" {
				state += " (" + host.Detail + ")"
			}
		}
		fields := []string{fmt.Sprintf("%-9s", host.Host), state}
		switch host.Trust {
		case "approved", "drift":
			fields = append(fields, "trust "+host.Trust+" "+shortTrustHash(host.TrustHash))
			if host.Trust == "drift" {
				needsApproval = true
			}
		case "":
		default:
			fields = append(fields, "trust "+host.Trust)
			needsApproval = true
		}
		if host.LastRun == nil {
			fields = append(fields, "last run never")
		} else {
			fields = append(fields, "last run "+*host.LastRun)
			if host.LastEvent != "" {
				fields = append(fields, "("+host.LastEvent+")")
			}
		}
		if host.LastError != "" {
			fields = append(fields, "last error: "+host.LastError)
		}
		fields = append(fields, host.Path)
		fmt.Fprintln(w, strings.Join(fields, "  "))
	}
	if needsApproval {
		fmt.Fprintln(w, codexReapproveNote)
	}
}

// lastHookRun returns the newest log entry of one host, its newest error and
// that entry's event.
func lastHookRun(entries []hookLogEntry, host string) (*time.Time, string, string) {
	var last *hookLogEntry
	lastError := ""
	for i := range entries {
		if entries[i].Host != host {
			continue
		}
		last = &entries[i]
		if entries[i].Error != "" {
			lastError = entries[i].Error
		}
	}
	if last == nil {
		return nil, "", ""
	}
	at := last.Time
	return &at, lastError, last.Event
}

// hookLogEntry is one line of index/hooks.log: a single hook invocation.
type hookLogEntry struct {
	Time       time.Time `json:"time"`
	Host       string    `json:"host"`
	Event      string    `json:"event"`
	Machine    string    `json:"machine,omitempty"`
	Session    string    `json:"session,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	Exit       int       `json:"exit"`
	Error      string    `json:"error,omitempty"`
}

const (
	// hookLogMaxBytes rotates the liveness log before it grows past this size.
	hookLogMaxBytes = 2 << 20
	// hookLogErrLimit bounds the captured first stderr line.
	hookLogErrLimit = 300
)

// hookLogPath is the per-machine liveness log under the gitignored index/.
func hookLogPath(dataDir string) string {
	if strings.TrimSpace(dataDir) == "" {
		return ""
	}
	return filepath.Join(dataDir, "index", "hooks.log")
}

// hookLogDisabled reports BRAIN_HOOK_OFF=1 (the kill switch for hook logging).
func hookLogDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BRAIN_HOOK_OFF"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// hookLogMachine is the short host name recorded with each run.
func hookLogMachine() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		if r < 128 && (r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	if b.Len() > 64 {
		return b.String()[:64]
	}
	return b.String()
}

// hookSecretPatterns redact credential-looking text before it reaches the log.
var hookSecretPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{
		regexp.MustCompile(`(?i)\b(token|secret|password|passwd|api[_-]?key|authorization|bearer)([ \t]*[:=][ \t]*)([^\s"']+)`),
		"${1}${2}<redacted>",
	},
	{
		regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{8,}|glpat-[A-Za-z0-9_-]{8,}|sk-[A-Za-z0-9_-]{8,}|xox[baprs]-[A-Za-z0-9-]{8,})\b`),
		"<redacted>",
	},
}

// redactHookError keeps the first stderr line, drops credential values and
// bounds the length. The log must never grow a secret.
func redactHookError(text string) string {
	line := strings.TrimSpace(text)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	for _, secret := range hookSecretPatterns {
		line = secret.pattern.ReplaceAllString(line, secret.replacement)
	}
	if runes := []rune(line); len(runes) > hookLogErrLimit {
		line = string(runes[:hookLogErrLimit])
	}
	return line
}

// appendHookLog appends one JSON line. Every failure is ignored: hook logging
// must never break an agent session.
func appendHookLog(dataDir string, entry hookLogEntry) {
	path := hookLogPath(dataDir)
	if path == "" || hookLogDisabled() {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	rotateHookLog(path)
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	entry.Error = redactHookError(entry.Error)
	if entry.Session != "" {
		entry.Session = truncatePlain(entry.Session, 64)
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(raw, '\n'))
	_ = file.Close()
}

// rotateHookLog renames an oversized log once (hooks.log.1) so appends stay cheap.
func rotateHookLog(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < hookLogMaxBytes {
		return
	}
	rotated := path + ".1"
	_ = os.Remove(rotated)
	_ = os.Rename(path, rotated)
}

// readHookLog reads the liveness log best effort, oldest first: the rotated
// hooks.log.1 followed by the current file. A missing log is not an error.
func readHookLog(dataDir string) []hookLogEntry {
	path := hookLogPath(dataDir)
	if path == "" {
		return nil
	}
	entries := readHookLogFile(path + ".1")
	return append(entries, readHookLogFile(path)...)
}

func readHookLogFile(path string) []hookLogEntry {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []hookLogEntry
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry hookLogEntry
		if json.Unmarshal(line, &entry) != nil || entry.Host == "" {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// truncatePlain shortens text without adding an ellipsis (log values).
func truncatePlain(text string, limit int) string {
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit])
	}
	return text
}

// firstLineWriter passes stderr through and keeps the first line for the hook
// liveness log, so the agent still sees the full text.
type firstLineWriter struct {
	w   io.Writer
	buf bytes.Buffer
}

// firstLineCaptureLimit bounds how much stderr is inspected for the log.
const firstLineCaptureLimit = 4096

func (f *firstLineWriter) Write(p []byte) (int, error) {
	if room := firstLineCaptureLimit - f.buf.Len(); room > 0 {
		if len(p) > room {
			f.buf.Write(p[:room])
		} else {
			f.buf.Write(p)
		}
	}
	return f.w.Write(p)
}

// firstLine returns the first non-empty captured line.
func (f *firstLineWriter) firstLine() string {
	for _, line := range strings.Split(f.buf.String(), "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
