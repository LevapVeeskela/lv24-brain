package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	path string
	// dir must exist; otherwise the agent is not installed and the target is skipped.
	dir       string
	dirReason string
	// createIfMissing creates path when it is absent; otherwise a missing file is skipped.
	createIfMissing bool
	missingReason   string
	build           agentHookBuildFunc
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
	return []agentHookTarget{
		{
			path:          filepath.Join(claudeDir, "settings.json"),
			dir:           claudeDir,
			dirReason:     "Claude Code not installed",
			missingReason: "Claude Code not installed: settings.json not found",
			build: jsonHookBuild(exe, cfg, []agentHookSpec{
				{event: "SessionStart", subcmd: "start", agent: "claude"},
				{event: "SessionEnd", subcmd: "end", agent: "claude"},
			}),
		},
		{
			path:            filepath.Join(codexDir, "hooks.json"),
			dir:             codexDir,
			dirReason:       "Codex not installed",
			createIfMissing: true,
			build: jsonHookBuild(exe, cfg, []agentHookSpec{
				{event: "SessionStart", subcmd: "start", agent: "codex"},
				{event: "Stop", subcmd: "end", agent: "codex"},
				{event: "SessionEnd", subcmd: "end", agent: "codex"},
			}),
			note: codexHooksNote,
		},
		{
			path:            openCodePluginPath(opencodeDir),
			dir:             opencodeDir,
			dirReason:       "OpenCode not installed",
			createIfMissing: true,
			build:           openCodePluginBuild(exe, cfg),
		},
	}
}

// syncAgentHooks installs or refreshes the brain hooks. It returns 1 when a
// file failed, or when check is set and any file would change.
func syncAgentHooks(home, exe, cfg string, check bool, stdout, stderr io.Writer) int {
	exit := 0
	for _, target := range agentHookTargets(home, exe, cfg) {
		out, err := planAgentHook(target)
		if err != nil {
			fmt.Fprintf(stderr, "brain agents: hooks: %s: %v\n", target.path, err)
			exit = 1
			continue
		}
		switch out.status {
		case agentsSkipped:
			fmt.Fprintf(stdout, "skipped (%s) %s\n", out.reason, target.path)
			continue
		case agentsUnchanged:
			fmt.Fprintf(stdout, "%s %s\n", agentsUnchanged, target.path)
			continue
		}
		if check {
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
	return exit
}

// planAgentHook computes the result for one target without writing anything.
func planAgentHook(t agentHookTarget) (agentHookOutcome, error) {
	info, err := os.Stat(t.dir)
	switch {
	case errors.Is(err, os.ErrNotExist), err == nil && !info.IsDir():
		return agentHookOutcome{status: agentsSkipped, reason: t.dirReason}, nil
	case err != nil:
		return agentHookOutcome{}, err
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
