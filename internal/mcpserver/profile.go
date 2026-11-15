package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// buildTools assembles the tool list of the profile. The native 15 are always
// built first, so an alias can run the native handler by name even though the
// profile does not list it.
func (s *Server) buildTools() {
	s.nativeTools = s.nativeToolList()
	s.nativeByName = make(map[string]tool, len(s.nativeTools))
	for _, t := range s.nativeTools {
		s.nativeByName[t.Name] = t
	}
	switch s.profile.Name {
	case ProfileLV24Legacy:
		tools := make([]tool, 0, len(s.nativeTools)+len(legacyAliasSpecs()))
		tools = append(tools, s.nativeTools...)
		for _, spec := range legacyAliasSpecs() {
			tools = append(tools, s.aliasTool(spec))
		}
		s.allTools = tools
	case ProfileEngram:
		s.allTools = s.aliasProfileTools(engramAliasSpecs())
	case ProfileCodegraph:
		s.allTools = s.stubProfileTools(codegraphStubTools())
	case ProfileGraphify:
		s.allTools = s.stubProfileTools(graphifyStubTools())
	default:
		s.allTools = s.nativeTools
	}
	s.tools = s.applyPolicy(s.allTools)
}

// toolList returns the effective tool list of the profile. It is kept as the
// single accessor tests and diagnostics use, so no caller has to know whether a
// profile is the native surface or a migration vocabulary.
func (s *Server) toolList() []tool {
	if s.allTools == nil {
		s.buildTools()
	}
	return s.allTools
}

// aliasProfileTools renders a migration vocabulary and appends the native
// introspection tool: brain_capabilities has no alias and is the one native
// name every profile keeps.
func (s *Server) aliasProfileTools(specs []aliasSpec) []tool {
	tools := make([]tool, 0, len(specs)+1)
	for _, spec := range specs {
		tools = append(tools, s.aliasTool(spec))
	}
	if capabilities, ok := s.nativeByName["brain_capabilities"]; ok {
		tools = append(tools, capabilities)
	}
	return tools
}

// stubProfileTools renders the honest code-graph placeholders plus the native
// introspection tool.
func (s *Server) stubProfileTools(stubs []tool) []tool {
	tools := make([]tool, 0, len(stubs)+1)
	tools = append(tools, stubs...)
	if capabilities, ok := s.nativeByName["brain_capabilities"]; ok {
		tools = append(tools, capabilities)
	}
	return tools
}

// aliasSpec is the internal form of one legacy name: protocol metadata, the
// translation that produces native arguments, and the grade that decides
// whether the call is exact, approximate or refused.
type aliasSpec struct {
	AliasSpec
	Title        string
	Description  string
	Kind         string
	InputSchema  map[string]any
	OutputSchema map[string]any
	ReadActions  []string
	translate    func(aliasArgs) (nativeArgs, error)
	handler      func(context.Context, *Server, aliasArgs) (toolResult, error)
	augment      func(toolResult) toolResult
}

// aliasTool converts one aliasSpec into a tool. The handler always runs
// runAlias, so strict mode and the compatibility envelope cannot be forgotten
// on a new alias.
func (s *Server) aliasTool(spec aliasSpec) tool {
	ann := aliasAnnotations(spec.Kind, spec.Title)
	out := spec.OutputSchema
	if out == nil {
		if target, ok := s.nativeByName[spec.Target]; ok {
			out = target.OutputSchema
		}
	}
	if out == nil {
		out = outputObject(map[string]any{})
	}
	t := tool{
		Name:         spec.Legacy,
		Description:  spec.Description,
		Annotations:  ann,
		InputSchema:  spec.InputSchema,
		OutputSchema: out,
		ReadActions:  spec.ReadActions,
		Legacy:       true,
		Deprecated:   s.profile.Deprecated,
		Target:       spec.Target,
		Grade:        spec.Grade,
		Unsupported:  spec.Unsupported,
		Note:         spec.Note,
		Reason:       spec.Reason,
		Remediation:  spec.Remediation,
		run: func(ctx context.Context, raw json.RawMessage) (toolResult, error) {
			return s.runAlias(ctx, spec, raw)
		},
	}
	if target, ok := s.nativeByName[spec.Target]; ok {
		t.Limits = target.Limits
		if target.confirm != nil {
			t.confirm = func(raw json.RawMessage) *confirmation {
				args, err := decodeAliasArgs(raw)
				if err != nil {
					return nil
				}
				translated, err := spec.translate(args)
				if err != nil {
					return nil
				}
				return target.confirm(marshalNativeArgs(translated))
			}
		}
	}
	return t
}

// aliasAnnotations maps the declared write posture to the four hints. A mixed
// alias whose read actions are harmless is still destructive at the tool level,
// and names its read actions in x-lv24-read-actions.
func aliasAnnotations(kind, title string) Annotations {
	switch kind {
	case "read":
		return readOnlyAnnotations(title)
	case "destructive":
		return mutatingAnnotations(title)
	default:
		return appendOnlyAnnotations(title)
	}
}

// runAlias is the single dispatch point of every migration alias: decode the
// legacy arguments, apply the profile policy, translate, run the native tool
// and attach the compatibility envelope.
func (s *Server) runAlias(ctx context.Context, spec aliasSpec, raw json.RawMessage) (toolResult, error) {
	args, err := decodeAliasArgs(raw)
	if err != nil {
		return toolResult{}, err
	}
	ignored := aliasIgnoredFields(spec, args)
	if spec.Grade == GradeUnsupported {
		return toolResult{}, refusalUnsupported(spec.Reason, spec.Remediation)
	}
	if s.profile.Strict && spec.Grade == GradeApproximate {
		return toolResult{}, refusalUnsupported(
			spec.Legacy+" is an approximate alias (grade C); --strict serves only exact translations",
			spec.Target+" without --strict")
	}
	if s.profile.Strict && len(ignored) > 0 {
		return toolResult{}, refusalInvalidWith(
			spec.Legacy+": unsupported argument(s) "+strings.Join(ignored, ", "),
			"call "+spec.Target+" without those fields, or run without --strict to have them reported instead")
	}
	// Validate the project spellings once, before any translation: a project
	// field of the wrong type must never fall back to the working directory.
	if _, err := projectFromArgs(args); err != nil {
		return toolResult{}, err
	}
	var out toolResult
	if spec.handler != nil {
		out, err = spec.handler(ctx, s, args)
	} else {
		native, translateErr := spec.translate(args)
		if translateErr != nil {
			return toolResult{}, translateErr
		}
		out, err = s.runNative(ctx, spec.Target, native)
	}
	if err != nil {
		return toolResult{}, err
	}
	if spec.augment != nil {
		out = spec.augment(out)
	}
	out.Meta = compatMeta(spec, s.profile, ignored)
	return out, nil
}

// runNative runs the native tool named by an alias target. The target is
// always present in a valid profile; a missing one is a typed refusal instead
// of a panic.
func (s *Server) runNative(ctx context.Context, name string, args nativeArgs) (toolResult, error) {
	target, ok := s.nativeByName[name]
	if !ok {
		return toolResult{}, refusalUnsupported("native tool "+name+" is not available", "brain_capabilities")
	}
	return target.run(ctx, marshalNativeArgs(args))
}

// compatMeta builds _meta["lv24.brain/compat"]. A grade-C call always says
// degraded and names its gap; dropped legacy arguments are always echoed.
func compatMeta(spec aliasSpec, profile Profile, ignored []string) map[string]any {
	compat := map[string]any{
		"legacy": spec.Legacy,
		"grade":  spec.Grade,
		"target": spec.Target,
	}
	if profile.Name == ProfileLV24Legacy {
		compat["deprecated"] = true
	}
	if spec.Grade == GradeApproximate {
		compat["degraded"] = true
		compat["note"] = spec.Note
	}
	if len(ignored) > 0 {
		compat["ignored_fields"] = ignored
	}
	return map[string]any{"lv24.brain/compat": compat}
}

// aliasIgnoredFields reports the arguments the translation drops: fields the
// schema does not declare and declared fields the alias documents as
// unsupported. Strict mode turns this report into a refusal.
func aliasIgnoredFields(spec aliasSpec, args aliasArgs) []string {
	known := map[string]bool{}
	if properties, ok := spec.InputSchema["properties"].(map[string]any); ok {
		for key := range properties {
			known[key] = true
		}
	}
	seen := map[string]bool{}
	ignored := make([]string, 0)
	for key := range args {
		if !known[key] {
			if !seen[key] {
				ignored = append(ignored, key)
				seen[key] = true
			}
		}
	}
	for _, key := range spec.Unsupported {
		if _, ok := args[key]; ok && !seen[key] {
			ignored = append(ignored, key)
			seen[key] = true
		}
	}
	sort.Strings(ignored)
	return ignored
}

// nativeAliasFor names the migration replacement of a native tool called
// against a vocabulary that does not list it.
func nativeAliasFor(profile, native string) (string, bool) {
	if profile != ProfileEngram && profile != ProfileCodegraph && profile != ProfileGraphify {
		return "", false
	}
	if profile != ProfileEngram {
		return "brain mcp --profile=lv24 (the native vocabulary)", true
	}
	replacements := map[string]string{
		"brain_note":     "mem_save",
		"brain_search":   "mem_search",
		"brain_open":     "mem_context",
		"brain_record":   "mem_get_observation",
		"brain_handoff":  "mem_session_summary",
		"brain_review":   "mem_review",
		"brain_judge":    "mem_judge",
		"brain_projects": "mem_list_projects",
		"brain_prompt":   "mem_save_prompt",
	}
	if alias, ok := replacements[native]; ok {
		return alias, true
	}
	return "brain mcp --profile=lv24 (the native vocabulary)", true
}

// aliasArgs is one legacy argument object. Values stay as raw JSON so a
// translation can pass a value through without changing its JSON form.
type aliasArgs map[string]json.RawMessage

func decodeAliasArgs(raw json.RawMessage) (aliasArgs, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return aliasArgs{}, nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &args); err != nil {
		return nil, refusalInvalid("invalid arguments: " + err.Error())
	}
	if args == nil {
		args = aliasArgs{}
	}
	return args, nil
}

func (a aliasArgs) raw(key string) (json.RawMessage, bool) {
	value, ok := a[key]
	if !ok {
		return nil, false
	}
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, false
	}
	return value, true
}

func (a aliasArgs) text(key string) (string, bool, error) {
	raw, ok := a.raw(key)
	if !ok {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, refusalInvalidf("%s must be a string", key)
	}
	return value, true, nil
}

func (a aliasArgs) firstText(keys ...string) (string, string, bool, error) {
	for _, key := range keys {
		value, ok, err := a.text(key)
		if err != nil {
			return "", key, true, err
		}
		if ok && strings.TrimSpace(value) != "" {
			return value, key, true, nil
		}
	}
	return "", "", false, nil
}

func (a aliasArgs) integer(key string) (int, bool, error) {
	raw, ok := a.raw(key)
	if !ok {
		return 0, false, nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, true, refusalInvalidf("%s must be an integer", key)
	}
	return value, true, nil
}

func (a aliasArgs) boolean(key string) (bool, bool, error) {
	raw, ok := a.raw(key)
	if !ok {
		return false, false, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, true, refusalInvalidf("%s must be a boolean", key)
	}
	return value, true, nil
}

func (a aliasArgs) list(key string) ([]string, bool, error) {
	raw, ok := a.raw(key)
	if !ok {
		return nil, false, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, true, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, true, nil
	}
	return nil, true, refusalInvalidf("%s must be a string or an array of strings", key)
}

// projectFromArgs resolves the engram spellings of a project selector. A
// directory is only a selector, never a project name: the registry still has
// to know it.
func projectFromArgs(a aliasArgs) (string, error) {
	for _, key := range []string{"project", "project_id", "directory", "cwd"} {
		value, ok, err := a.text(key)
		if err != nil {
			return "", err
		}
		if ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", nil
}

// nativeArgs is one native tool argument object under construction.
type nativeArgs map[string]json.RawMessage

func newNativeArgs() nativeArgs { return nativeArgs{} }

func (m nativeArgs) set(key string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	m[key] = data
}

func (m nativeArgs) setText(key, value string) {
	if strings.TrimSpace(value) != "" {
		m.set(key, value)
	}
}

func (m nativeArgs) setList(key string, values []string) {
	if len(values) > 0 {
		m.set(key, values)
	}
}

func marshalNativeArgs(args nativeArgs) json.RawMessage {
	if args == nil {
		return json.RawMessage(`{}`)
	}
	data, err := json.Marshal(args)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}

// addStructuredFields merges fields into a structured result, so an alias can
// emit a legacy field name next to the native one without renaming it away.
func addStructuredFields(out toolResult, fields map[string]any) toolResult {
	data, err := json.Marshal(out.Structured)
	if err != nil {
		return out
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return out
	}
	for key, value := range fields {
		object[key] = value
	}
	out.Structured = object
	return out
}

func firstLine(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(trimmed, "\n", 2)[0])
	if runes := []rune(line); len(runes) > 120 {
		line = string(runes[:120])
	}
	return line
}

// legacyVerdict maps the engram verdict vocabulary to ours. An unknown value
// is passed through, so the native validator names the allowed set.
func legacyVerdict(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "related":
		return string(domain.RelationRelatesTo)
	case "conflict", "conflicting":
		return string(domain.RelationConflictsWith)
	case "unrelated":
		return string(domain.RelationNotConflict)
	case "duplicate":
		return string(domain.RelationCompatible)
	default:
		return value
	}
}

// judgmentPair splits the deterministic judgment id this server exposes:
// "<source>-><target>". Engram's opaque integer row ids are not addressable.
func judgmentPair(id string) (string, string, bool) {
	index := strings.Index(id, "->")
	if index <= 0 || index+2 >= len(id) {
		return "", "", false
	}
	return strings.TrimSpace(id[:index]), strings.TrimSpace(id[index+2:]), true
}

// ---------------------------------------------------------------------------
// Legacy 0.16 bridge (7 names)

func legacyAliasSpecs() []aliasSpec {
	project := func() map[string]any {
		return map[string]any{"project": stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")}
	}
	return []aliasSpec{
		{
			AliasSpec: AliasSpec{Legacy: "brain_state_get", Target: "brain_state", Action: "get", Grade: GradeExact,
				Note: profileLegacyReason},
			Title:       "Read project state (0.16)",
			Description: "0.16 name: calls brain_state(action=get). Kept so a client written against 0.16 keeps working.",
			Kind:        "read",
			InputSchema: legacySchema(nil, project()),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("action", "get")
				setProjectFromAlias(native, a)
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{Legacy: "brain_state_set", Target: "brain_state", Action: "set", Grade: GradeExact,
				Note: profileLegacyReason},
			Title:       "Update project state (0.16)",
			Description: "0.16 name: calls brain_state(action=set). Only the given fields change.",
			Kind:        "destructive",
			InputSchema: legacySchema(nil, map[string]any{
				"project":         project()["project"],
				"goal":            stringProp("Project goal."),
				"next":            stringProp("The next action."),
				"active":          stringArrayProp("Active work items (replaces the list)."),
				"blocked":         stringArrayProp("Blockers (replaces the list)."),
				"open_questions":  stringArrayProp("Open questions (replaces the list)."),
				"fresh_for_hours": intProp("Mark the state current for this many hours; 0 keeps the stored value.", 0),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("action", "set")
				setProjectFromAlias(native, a)
				if value, ok, err := a.text("goal"); err != nil {
					return nil, err
				} else if ok {
					native.set("goal", value)
				}
				if value, ok, err := a.text("next"); err != nil {
					return nil, err
				} else if ok {
					native.set("next", value)
				}
				for _, key := range []string{"active", "blocked", "open_questions"} {
					if values, ok, err := a.list(key); err != nil {
						return nil, err
					} else if ok {
						native.set(key, values)
					}
				}
				if hours, ok, err := a.integer("fresh_for_hours"); err != nil {
					return nil, err
				} else if ok {
					native.set("fresh_for_hours", hours)
				}
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{Legacy: "brain_journal", Target: "brain_catchup", Action: "journal", Grade: GradeExact,
				Note: profileLegacyReason},
			Title:       "Read the activity journal (0.16)",
			Description: "0.16 name: calls brain_catchup(mode=journal); limit and cursor map one to one.",
			Kind:        "read",
			InputSchema: legacySchema(nil, map[string]any{
				"project": project()["project"],
				"limit":   intProp("Page size; 0 selects the default of 20.", 0),
				"cursor":  cursorProp(),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("mode", "journal")
				setProjectFromAlias(native, a)
				if limit, ok, err := a.integer("limit"); err != nil {
					return nil, err
				} else if ok {
					native.set("limit", limit)
				}
				if cursor, ok, err := a.text("cursor"); err != nil {
					return nil, err
				} else if ok {
					native.set("cursor", cursor)
				}
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{Legacy: "brain_records", Target: "brain_record", Action: "list", Grade: GradeExact,
				Note: profileLegacyReason},
			Title:       "List records (0.16)",
			Description: "0.16 name: calls brain_record(action=list). Filters and output are unchanged.",
			Kind:        "read",
			InputSchema: legacySchema(nil, map[string]any{
				"project":    project()["project"],
				"kinds":      stringArrayProp("Keep only these kinds: note, decision, attempt, handoff."),
				"since_days": intProp("Keep records created or updated in the last N days; 0 keeps all.", 0),
				"status":     enumProp("Keep decisions with this status.", decisionStatuses),
				"limit":      intProp("Page size; 0 selects the default of 50.", 0),
				"cursor":     cursorProp(),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("action", "list")
				setProjectFromAlias(native, a)
				if kinds, ok, err := a.list("kinds"); err != nil {
					return nil, err
				} else if ok {
					native.set("kinds", kinds)
				}
				if days, ok, err := a.integer("since_days"); err != nil {
					return nil, err
				} else if ok {
					native.set("since_days", days)
				}
				if status, ok, err := a.text("status"); err != nil {
					return nil, err
				} else if ok {
					native.set("status", status)
				}
				if limit, ok, err := a.integer("limit"); err != nil {
					return nil, err
				} else if ok {
					native.set("limit", limit)
				}
				if cursor, ok, err := a.text("cursor"); err != nil {
					return nil, err
				} else if ok {
					native.set("cursor", cursor)
				}
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{Legacy: "brain_relations", Target: "brain_record", Action: "relations", Grade: GradeExact,
				Note: profileLegacyReason},
			Title:       "List relations (0.16)",
			Description: "0.16 name: calls brain_record(action=relations); the filters map one to one.",
			Kind:        "read",
			InputSchema: legacySchema(nil, map[string]any{
				"project":  project()["project"],
				"id":       stringProp("List the relations the record with this id carries."),
				"target":   stringProp("List the relations that point at this record."),
				"status":   enumProp("Keep one status: pending, accepted, rejected.", domain.RelationStatusNames()),
				"relation": enumProp("Keep one relation verb.", relationVerbNames()),
				"limit":    intProp("Page size; 0 selects the default of 50.", 0),
				"cursor":   cursorProp(),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("action", "relations")
				setProjectFromAlias(native, a)
				for _, key := range []string{"id", "target", "status", "relation"} {
					if value, ok, err := a.text(key); err != nil {
						return nil, err
					} else if ok {
						native.set(key, value)
					}
				}
				if limit, ok, err := a.integer("limit"); err != nil {
					return nil, err
				} else if ok {
					native.set("limit", limit)
				}
				if cursor, ok, err := a.text("cursor"); err != nil {
					return nil, err
				} else if ok {
					native.set("cursor", cursor)
				}
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{Legacy: "brain_review_record", Target: "brain_judge", Action: "review", Grade: GradeExact,
				Note: profileLegacyReason},
			Title:       "Review records (0.16)",
			Description: "0.16 name: calls brain_judge(action=review); all=true becomes ids:[\"*\"].",
			Kind:        "destructive",
			InputSchema: legacySchema(nil, map[string]any{
				"project": project()["project"],
				"id":      stringProp("Record id to review. Omit with all=true."),
				"all":     boolProp("Review every unreviewed record of the project."),
				"trust":   enumProp("Optional trust level of the reviewed records.", trustLevels),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("action", "review")
				setProjectFromAlias(native, a)
				if id, ok, err := a.text("id"); err != nil {
					return nil, err
				} else if ok {
					native.set("id", id)
				}
				if all, ok, err := a.boolean("all"); err != nil {
					return nil, err
				} else if ok && all {
					native.set("ids", []string{"*"})
				}
				if trust, ok, err := a.text("trust"); err != nil {
					return nil, err
				} else if ok {
					native.set("trust", trust)
				}
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{Legacy: "brain_decision_status", Target: "brain_judge", Action: "accept|reject|supersede", Grade: GradeEnvelope,
				Note: "status=proposed is refused: the 0.17 lifecycle only accepts, rejects or supersedes; record a new decision instead."},
			Title:       "Change decision status (0.16)",
			Description: "0.16 name: status accepted/rejected map to brain_judge(action=accept|reject); superseded_by maps to supersede.",
			Kind:        "destructive",
			InputSchema: legacySchema(nil, map[string]any{
				"project":       project()["project"],
				"id":            stringProp("Decision id."),
				"status":        enumProp("New status.", decisionStatuses),
				"superseded_by": stringProp("Id of the live decision that replaces this one."),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				id, err := requireAliasText(a, "id")
				if err != nil {
					return nil, err
				}
				supersededBy, _, err := a.text("superseded_by")
				if err != nil {
					return nil, err
				}
				status, _, err := a.text("status")
				if err != nil {
					return nil, err
				}
				native := newNativeArgs()
				native.set("id", id)
				setProjectFromAlias(native, a)
				switch {
				case strings.TrimSpace(supersededBy) != "":
					native.set("action", "supersede")
					native.set("to", supersededBy)
				case strings.EqualFold(status, "accepted"):
					native.set("action", "accept")
				case strings.EqualFold(status, "rejected"):
					native.set("action", "reject")
				case strings.EqualFold(status, "superseded"):
					return nil, refusalInvalidWith("status=superseded needs superseded_by",
						"brain_judge(action=supersede) with to=<new decision id>")
				case strings.EqualFold(status, "proposed"):
					return nil, refusalUnsupported(
						"a decision cannot be moved back to proposed in 0.17",
						"record a new decision with brain_decision; the old one stays superseded")
				default:
					return nil, refusalInvalidf("unknown status %q (proposed, accepted, rejected, superseded)", status)
				}
				return native, nil
			},
		},
	}
}

func setProjectFromAlias(native nativeArgs, a aliasArgs) {
	if project, err := projectFromArgs(a); err == nil {
		native.setText("project", project)
	}
}

func requireAliasText(a aliasArgs, key string) (string, error) {
	value, ok, err := a.text(key)
	if err != nil {
		return "", err
	}
	if !ok || strings.TrimSpace(value) == "" {
		return "", refusalInvalid(key + " is required")
	}
	return strings.TrimSpace(value), nil
}

// ---------------------------------------------------------------------------
// engram profile (27 names)

func engramAliasSpecs() []aliasSpec {
	return []aliasSpec{
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_save", Target: "brain_note", Grade: GradeEnvelope,
				Unsupported: []string{"session_id", "project_choice_reason", "recovery_token", "org"},
				Note:        "type routes to brain_decision/brain_attempt; content/observation becomes text; session_id and the other dropped fields are reported",
			},
			Title:       "Save an observation",
			Description: "Save an observation. type=decision or policy records a decision; attempt or failed_attempt records an attempt; every other type is a note subtype.",
			Kind:        "append",
			InputSchema: engramSchema(nil, map[string]any{
				"content":               stringProp("Observation text."),
				"observation":           stringProp("Alias of content."),
				"type":                  stringProp("Observation type; decision and policy become decisions, attempt and failed_attempt become attempts."),
				"scope":                 enumProp("Record scope: project, personal or global.", recordScopes),
				"topic_key":             stringProp("Stable topic key; a write with an existing key evolves that note."),
				"title":                 stringProp("Optional title."),
				"source":                stringProp("Optional external id the observation came from."),
				"session_id":            stringProp("Dropped: sessions are not modelled."),
				"project_choice_reason": stringProp("Dropped: project resolution is the registry."),
				"recovery_token":        stringProp("Dropped: no recovery protocol."),
				"org":                   stringProp("Dropped: one local brain per machine."),
			}),
			handler: saveAlias,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_search", Target: "brain_search", Grade: GradeEnvelope,
				Note: "match_mode maps to the native all/any mode; response_format=compact maps to compact=true; no trigram tokenizer",
			},
			Title:       "Search memory",
			Description: "Full-text search over the brain. match_mode=all requires every term, any accepts one; response_format=compact returns previews only.",
			Kind:        "read",
			InputSchema: engramSchema([]string{"query"}, map[string]any{
				"query":           stringProp("Words to search for."),
				"match_mode":      enumProp("all or any.", []string{"all", "any"}),
				"response_format": enumProp("compact or full.", []string{"compact", "full"}),
				"all_projects":    boolProp("Search every project explicitly."),
				"scope":           enumProp("Keep only this record scope.", recordScopes),
				"types":           stringArrayProp("Keep only these kinds."),
				"type":            stringProp("Keep only this kind."),
				"since":           stringProp("Only documents updated since this instant."),
				"limit":           intProp("Page size; 0 selects the default.", 0),
			}),
			translate: translateSearch,
			augment:   withSearchLegacyFields,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_search_prompts", Target: "brain_prompt", Grade: GradeEnvelope,
				Note: "prompts are local-only and never appear in brain_search",
			},
			Title:       "Search stored prompts",
			Description: "Search the opt-in local prompt zone. The prompt zone must be enabled in brain.yaml.",
			Kind:        "read",
			InputSchema: engramSchema(nil, map[string]any{
				"query": stringProp("Case-insensitive words to keep; omit for the newest prompts."),
				"limit": intProp("Maximum prompts returned; at most 50.", 0),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("action", "search")
				setProjectFromAlias(native, a)
				if query, ok, err := a.text("query"); err != nil {
					return nil, err
				} else if ok {
					native.set("query", query)
				}
				if limit, ok, err := a.integer("limit"); err != nil {
					return nil, err
				} else if ok {
					native.set("limit", limit)
				}
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_context", Target: "brain_open", Grade: GradeEnvelope,
				Unsupported: []string{"session_id"},
				Note:        "curated brief instead of a recency brief; max_bytes is treated as runes and capped at 32000",
			},
			Title:       "Load session context",
			Description: "Load the curated project brief with state, the latest handoff and recent records.",
			Kind:        "read",
			InputSchema: engramSchema(nil, map[string]any{
				"max_bytes":          intProp("Summary budget in bytes, treated as runes; capped at 32000.", 0),
				"include_unreviewed": boolProp("Include quarantined records."),
				"session_id":         stringProp("Dropped: sessions are not modelled."),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("include", []string{"state", "handoff", "recent"})
				setProjectFromAlias(native, a)
				if budget, ok, err := a.integer("max_bytes"); err != nil {
					return nil, err
				} else if ok {
					if budget > 32000 {
						budget = 32000
					}
					if budget > 0 {
						native.set("budget", budget)
					}
				}
				if include, ok, err := a.boolean("include_unreviewed"); err != nil {
					return nil, err
				} else if ok {
					native.set("include_unreviewed", include)
				}
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_get_observation", Target: "brain_record", Grade: GradeExact,
				Note: "accepts <project>/<record> ids; integer engram ids are NOT_FOUND with a search hint",
			},
			Title:       "Read one observation",
			Description: "Read one record in full. The id may be a native record id or <project>/<record>.",
			Kind:        "read",
			InputSchema: engramSchema([]string{"id"}, map[string]any{
				"id": stringProp("Record id or <project>/<record>."),
			}),
			translate: translateGetObservation,
			augment:   withObservationLegacyFields,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_timeline", Target: "brain_catchup", Grade: GradeApproximate,
				Note: "previews of at most 160 characters; before/after are capped at 5 neighbours per side",
			},
			Title:       "Record timeline",
			Description: "Place one record among its neighbours in the same project and kind. Neighbours are previews, not full bodies.",
			Kind:        "read",
			InputSchema: engramSchema([]string{"record_id"}, map[string]any{
				"record_id": stringProp("Record to centre the timeline on."),
				"before":    intProp("Neighbours before the record, capped at 5.", 0),
				"after":     intProp("Neighbours after the record, capped at 5.", 0),
				"limit":     intProp("Neighbours per side, capped at 5.", 0),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				recordID, err := requireAliasText(a, "record_id")
				if err != nil {
					return nil, err
				}
				native := newNativeArgs()
				native.set("mode", "timeline")
				native.set("record_id", recordID)
				setProjectFromAlias(native, a)
				window := 0
				for _, key := range []string{"before", "after", "limit"} {
					value, ok, err := a.integer(key)
					if err != nil {
						return nil, err
					}
					if ok && value > window {
						window = value
					}
				}
				if window > 5 {
					window = 5
				}
				if window > 0 {
					native.set("limit", window)
				}
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_update", Target: "brain_note", Grade: GradeApproximate,
				Unsupported: []string{"find", "replace"},
				Note:        "no in-place edit: a new note under topic_key supersedes the old body, which stays under History; find/replace are dropped",
			},
			Title:       "Evolve an observation",
			Description: "Evolve an observation by appending a note under a topic key. The previous body stays under History; find/replace editing is not available.",
			Kind:        "append",
			InputSchema: engramSchema(nil, map[string]any{
				"id":          stringProp("Record id to evolve; its title supplies the topic key when topic_key is omitted."),
				"content":     stringProp("New observation text."),
				"observation": stringProp("Alias of content."),
				"topic_key":   stringProp("Stable topic key to evolve."),
				"title":       stringProp("Optional title."),
				"find":        stringProp("Not available: no in-place edit."),
				"replace":     stringProp("Not available: no in-place edit."),
			}),
			handler: updateAlias,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_delete", Target: "", Grade: GradeUnsupported,
				Reason:      "no MCP delete in 0.17; the Markdown truth is append-only",
				Remediation: "brain record trash <id> (CLI, reversible); hard delete is never available",
				Unsupported: []string{"hard_delete"},
			},
			Title:       "Delete an observation (refused)",
			Description: "Refused: removal is a reversible CLI operation. The MCP surface never deletes a record.",
			Kind:        "destructive",
			InputSchema: engramSchema([]string{"id"}, map[string]any{
				"id":          stringProp("Record id."),
				"hard_delete": boolProp("Never available."),
			}),
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_save_prompt", Target: "brain_prompt", Grade: GradeEnvelope,
				Note: "stored local-only; session_id becomes the session provenance field",
			},
			Title:       "Save a prompt",
			Description: "Store a user prompt in the opt-in local prompt zone. Prompts are never committed or indexed.",
			Kind:        "append",
			InputSchema: engramSchema([]string{"content"}, map[string]any{
				"content":    stringProp("Prompt text."),
				"session_id": stringProp("Session id recorded with the prompt."),
				"session":    stringProp("Alias of session_id."),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				content, err := requireAliasText(a, "content")
				if err != nil {
					return nil, err
				}
				native := newNativeArgs()
				native.set("action", "save")
				native.set("content", content)
				setProjectFromAlias(native, a)
				if session, _, _, err := a.firstText("session_id", "session"); err != nil {
					return nil, err
				} else if session != "" {
					native.set("session", session)
				}
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_pin", Target: "brain_judge", Grade: GradeApproximate,
				Note: "pinning needs additive frontmatter and a ranking weight; the native call refuses it in 0.17",
			},
			Title:       "Pin an observation",
			Description: "Pin a record. Pinning is not exposed in 0.17: the native call refuses with the CLI path.",
			Kind:        "destructive",
			InputSchema: engramSchema([]string{"id"}, map[string]any{"id": stringProp("Record id.")}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				id, err := requireAliasText(a, "id")
				if err != nil {
					return nil, err
				}
				native := newNativeArgs()
				native.set("action", "pin")
				native.set("id", id)
				setProjectFromAlias(native, a)
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_unpin", Target: "brain_judge", Grade: GradeApproximate,
				Note: "pinning needs additive frontmatter and a ranking weight; the native call refuses it in 0.17",
			},
			Title:       "Unpin an observation",
			Description: "Unpin a record. Pinning is not exposed in 0.17: the native call refuses with the CLI path.",
			Kind:        "destructive",
			InputSchema: engramSchema([]string{"id"}, map[string]any{"id": stringProp("Record id.")}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				id, err := requireAliasText(a, "id")
				if err != nil {
					return nil, err
				}
				native := newNativeArgs()
				native.set("action", "unpin")
				native.set("id", id)
				setProjectFromAlias(native, a)
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_session_start", Target: "", Grade: GradeApproximate,
				Note: "sessions are not modelled: returns a synthetic <project>/<date> handle and writes nothing",
			},
			Title:       "Start a session (synthetic)",
			Description: "Return a synthetic session handle. No session is stored; --strict refuses this approximate alias.",
			Kind:        "read",
			InputSchema: engramSchema(nil, map[string]any{}),
			handler:     sessionStartAlias,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_session_end", Target: "brain_handoff", Grade: GradeApproximate,
				Note: "the session handle is synthetic; a non-empty summary writes a handoff, an empty one writes nothing",
			},
			Title:       "End a session (synthetic)",
			Description: "Close a synthetic session. A non-empty summary is forwarded to brain_handoff.",
			Kind:        "append",
			InputSchema: engramSchema(nil, map[string]any{
				"session_id": stringProp("Synthetic session handle."),
				"summary":    stringProp("What the session accomplished."),
			}),
			handler: sessionEndAlias,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_session_summary", Target: "brain_handoff", Grade: GradeEnvelope,
				Unsupported: []string{"session_id"},
				Note:        "the template is parsed into handoff sections; session_id is dropped",
			},
			Title:       "Record a session summary",
			Description: "Record a handoff from the Goal/Instructions/Discoveries/Accomplished/Next/Relevant-files template.",
			Kind:        "append",
			InputSchema: engramSchema(nil, map[string]any{
				"summary":        stringProp("Full summary, instead of or next to the sections."),
				"goal":           stringProp("Goal section."),
				"instructions":   stringProp("Instructions section."),
				"discoveries":    stringProp("Discoveries section."),
				"accomplished":   stringProp("Accomplished section."),
				"next_steps":     stringProp("Next steps section."),
				"relevant_files": stringArrayProp("Relevant files."),
				"session_id":     stringProp("Dropped: sessions are not modelled."),
			}),
			handler: summaryAlias,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_session_finalize", Target: "brain_handoff", Grade: GradeEnvelope,
				Unsupported: []string{"session_id"},
				Note:        "idempotent by project and summary: replaying the latest identical handoff returns its id",
			},
			Title:       "Finalize a session",
			Description: "Record the final handoff. An identical latest handoff is returned instead of written twice.",
			Kind:        "append",
			InputSchema: engramSchema(nil, map[string]any{
				"summary":        stringProp("Full summary, instead of or next to the sections."),
				"goal":           stringProp("Goal section."),
				"instructions":   stringProp("Instructions section."),
				"discoveries":    stringProp("Discoveries section."),
				"accomplished":   stringProp("Accomplished section."),
				"next_steps":     stringProp("Next steps section."),
				"relevant_files": stringArrayProp("Relevant files."),
				"session_id":     stringProp("Dropped: sessions are not modelled."),
			}),
			handler: finalizeAlias,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_capture_passive", Target: "brain_note", Grade: GradeApproximate,
				Unsupported: []string{"session_id"},
				Note:        "stores the raw text as a capture note; no learnings extraction and no dedupe",
			},
			Title:       "Capture text passively",
			Description: "Store raw text as a note with subtype capture. No extraction or dedupe is performed.",
			Kind:        "append",
			InputSchema: engramSchema(nil, map[string]any{
				"content":     stringProp("Text to store."),
				"observation": stringProp("Alias of content."),
				"source":      stringProp("Optional source label."),
				"session_id":  stringProp("Dropped: sessions are not modelled."),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				content, _, ok, err := a.firstText("content", "observation")
				if err != nil {
					return nil, err
				}
				if !ok {
					return nil, refusalInvalid("content is required")
				}
				native := newNativeArgs()
				native.set("action", "record")
				native.set("text", content)
				native.set("subtype", "capture")
				source := "passive"
				if value, ok, err := a.text("source"); err != nil {
					return nil, err
				} else if ok && strings.TrimSpace(value) != "" {
					source = value
				}
				native.set("source", source)
				setProjectFromAlias(native, a)
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_review", Target: "brain_review", Grade: GradeEnvelope,
				Note: "action=list reads the due queue; mark_reviewed calls brain_judge(action=review)",
			},
			Title:       "Review memory",
			Description: "action=list lists the due queue; action=mark_reviewed clears quarantine for id, observation_id or all.",
			Kind:        "destructive",
			ReadActions: []string{"list"},
			InputSchema: engramSchema(nil, map[string]any{
				"action":         enumProp("list or mark_reviewed.", []string{"list", "mark_reviewed"}),
				"id":             stringProp("Record id to review."),
				"observation_id": stringProp("Alias of id."),
				"all":            boolProp("Review every unreviewed record."),
				"trust":          enumProp("Optional trust level.", trustLevels),
				"due_before":     stringProp("With action=list, keep items due at or before this instant."),
			}),
			handler: reviewAlias,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_suggest_topic_key", Target: "brain_note", Grade: GradeEnvelope,
				Note: "the slug heuristic is ours; nothing is written",
			},
			Title:       "Suggest a topic key",
			Description: "Compute the topic key a title or text would produce, without writing.",
			Kind:        "read",
			InputSchema: engramSchema(nil, map[string]any{
				"title":       stringProp("Title to slug."),
				"content":     stringProp("Text to slug when title is absent."),
				"observation": stringProp("Alias of content."),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("action", "suggest_topic")
				if title, ok, err := a.text("title"); err != nil {
					return nil, err
				} else if ok {
					native.set("title", title)
				}
				if content, _, ok, err := a.firstText("content", "observation"); err != nil {
					return nil, err
				} else if ok {
					native.set("text", content)
				}
				setProjectFromAlias(native, a)
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_judge", Target: "brain_judge", Grade: GradeEnvelope,
				Note: "related maps to relates_to; judgment_id is the deterministic <source>-><target> pair",
			},
			Title:       "Judge a relation",
			Description: "Record a verdict on a pair. judgment_id may be the <source>-><target> pair this server exposes.",
			Kind:        "destructive",
			InputSchema: engramSchema(nil, map[string]any{
				"judgment_id": stringProp("Pending judgment id in <source>-><target> form."),
				"from":        stringProp("Record id that carries the relation."),
				"to":          stringProp("Related record id."),
				"verdict":     stringProp("Verdict; related, conflict and unrelated are mapped to the native vocabulary."),
				"reason":      stringProp("Why the pair is related, conflicting or unrelated."),
				"confidence":  numberProp("Optional confidence between 0 and 1."),
			}),
			translate: translateJudge,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_judge_many", Target: "brain_judge", Grade: GradeEnvelope,
				Note: "all-or-nothing, at most 32 judgments; per-item judgment_id uses the <source>-><target> pair",
			},
			Title:       "Judge several relations",
			Description: "Record up to 32 verdicts. Every item is validated before the first write.",
			Kind:        "destructive",
			InputSchema: engramSchema([]string{"judgments"}, map[string]any{
				"judgments": arrayProp("Verdicts, at most 32.", objectProp("One verdict: judgment_id or from/to, verdict, reason, confidence.")),
			}),
			translate: translateJudgeMany,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_compare", Target: "brain_judge", Grade: GradeEnvelope,
				Note: "memory_id_a/b map to from/to; reasoning maps to reason",
			},
			Title:       "Compare two observations",
			Description: "Record one verdict that compares two records.",
			Kind:        "destructive",
			InputSchema: engramSchema(nil, map[string]any{
				"memory_id_a": stringProp("First record id."),
				"memory_id_b": stringProp("Second record id."),
				"reasoning":   stringProp("Why the two are related or not."),
				"verdict":     stringProp("Optional verdict; relates_to is the default."),
			}),
			translate: translateCompare,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_consolidate", Target: "brain_judge", Grade: GradeEnvelope,
				Unsupported: []string{"confirm", "fingerprint", "phase", "strategy"},
				Note:        "two-phase consolidation is refused by the engine in 0.17; sources stay intact",
			},
			Title:       "Consolidate observations",
			Description: "Consolidation is not exposed in 0.17: the native call refuses and points at the CLI supersede path.",
			Kind:        "destructive",
			InputSchema: engramSchema(nil, map[string]any{
				"ids":         stringArrayProp("Source record ids."),
				"memory_ids":  stringArrayProp("Alias of ids."),
				"phase":       enumProp("plan or apply.", []string{"plan", "apply"}),
				"confirm":     boolProp("Dropped: consolidation is refused."),
				"fingerprint": stringProp("Dropped: consolidation is refused."),
				"strategy":    stringProp("Dropped: consolidation is refused."),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("action", "consolidate")
				setProjectFromAlias(native, a)
				if ids, _, err := a.firstList("ids", "memory_ids"); err != nil {
					return nil, err
				} else if len(ids) > 0 {
					native.set("ids", ids)
				}
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_stats", Target: "brain_open", Grade: GradeEnvelope,
				Note: "counts only; no timings or database sizes",
			},
			Title:       "Memory statistics",
			Description: "Count records by kind and project. No timing or storage measurements are reported.",
			Kind:        "read",
			InputSchema: engramSchema(nil, map[string]any{}),
			handler:     statsAlias,
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_doctor", Target: "brain_review", Grade: GradeEnvelope,
				Unsupported: []string{"project"},
				Note:        "different checks (engine, data, index, orphans, locks), same severity and next shape; project is dropped",
			},
			Title:       "Run compatibility checks",
			Description: "Run the engine, data, index and lock checks. The doctor checks the whole data directory and takes no project.",
			Kind:        "read",
			InputSchema: engramSchema(nil, map[string]any{
				"project": stringProp("Dropped: doctor checks the whole data directory."),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("action", "doctor")
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_current_project", Target: "brain_projects", Grade: GradeExact,
				Note: "adds source (registry, alias or path); a path is never turned into a project name",
			},
			Title:       "Resolve the current project",
			Description: "Resolve the project of the working directory or a registered checkout path, and say how it matched.",
			Kind:        "read",
			InputSchema: engramSchema(nil, map[string]any{
				"directory": stringProp("Registered checkout path."),
				"cwd":       stringProp("Alias of directory."),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("action", "current")
				setProjectFromAlias(native, a)
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_list_projects", Target: "brain_projects", Grade: GradeExact,
				Note:        "same fields plus kind",
				Unsupported: []string{"include_stats"},
			},
			Title:       "List projects",
			Description: "List registered projects with their kind, checkout paths and repository URL.",
			Kind:        "read",
			InputSchema: engramSchema(nil, map[string]any{
				"include_stats": boolProp("Dropped: use mem_stats."),
			}),
			translate: func(a aliasArgs) (nativeArgs, error) {
				native := newNativeArgs()
				native.set("action", "list")
				return native, nil
			},
		},
		{
			AliasSpec: AliasSpec{
				Legacy: "mem_merge_projects", Target: "", Grade: GradeUnsupported,
				Reason:      "no project merge in 0.17",
				Remediation: "brain import --from engram --map-project <from>=<to>, or brain project set <id> --alias <old-id> for a rename",
			},
			Title:       "Merge projects (refused)",
			Description: "Refused: project identity is not merged over MCP in 0.17.",
			Kind:        "destructive",
			InputSchema: engramSchema(nil, map[string]any{
				"from":   stringProp("Source project."),
				"source": stringProp("Alias of from."),
				"to":     stringProp("Target project."),
				"target": stringProp("Alias of to."),
			}),
		},
	}
}

// engramSchema adds the common engram project spellings to a property map.
// additionalProperties stays true: an unknown legacy field is reported, not
// rejected, so an old client keeps working outside strict mode.
func engramSchema(required []string, properties map[string]any) map[string]any {
	common := map[string]any{
		"project":    stringProp("Registered project id or alias. Omit to use the project of the working directory."),
		"project_id": stringProp("engram spelling of project."),
		"directory":  stringProp("Registered checkout path; never turned into a project name."),
		"cwd":        stringProp("Alias of directory."),
	}
	for key, value := range common {
		if _, ok := properties[key]; !ok {
			properties[key] = value
		}
	}
	return legacySchema(required, properties)
}

// legacySchema is an input schema that documents the known legacy fields but
// accepts others, so aliasIgnoredFields can report them.
func legacySchema(required []string, properties map[string]any) map[string]any {
	schema := objectSchema(required, properties)
	schema["additionalProperties"] = true
	return schema
}

func (a aliasArgs) firstList(keys ...string) ([]string, string, error) {
	for _, key := range keys {
		values, ok, err := a.list(key)
		if err != nil {
			return nil, key, err
		}
		if ok && len(values) > 0 {
			return values, key, nil
		}
	}
	return nil, "", nil
}

// saveAlias routes mem_save by type, exactly as the report's rule 5 says.
func saveAlias(ctx context.Context, s *Server, a aliasArgs) (toolResult, error) {
	content, _, ok, err := a.firstText("content", "observation")
	if err != nil {
		return toolResult{}, err
	}
	if !ok {
		return toolResult{}, refusalInvalid("content is required")
	}
	typ, _, err := a.text("type")
	if err != nil {
		return toolResult{}, err
	}
	typ = strings.ToLower(strings.TrimSpace(typ))
	title, _, err := a.text("title")
	if err != nil {
		return toolResult{}, err
	}
	if strings.TrimSpace(title) == "" {
		title = firstLine(content)
	}
	project, err := projectFromArgs(a)
	if err != nil {
		return toolResult{}, err
	}
	switch typ {
	case "decision", "policy":
		native := newNativeArgs()
		native.set("title", title)
		native.set("context", content)
		native.setText("project", project)
		out, err := s.runNative(ctx, "brain_decision", native)
		if err != nil {
			return toolResult{}, err
		}
		return addStructuredFields(out, map[string]any{"type": typ}), nil
	case "attempt", "failed_attempt":
		native := newNativeArgs()
		native.set("title", title)
		native.set("result", content)
		native.setText("project", project)
		out, err := s.runNative(ctx, "brain_attempt", native)
		if err != nil {
			return toolResult{}, err
		}
		return addStructuredFields(out, map[string]any{"type": typ}), nil
	}
	native := newNativeArgs()
	native.set("action", "record")
	native.set("text", content)
	native.setText("title", title)
	native.setText("subtype", typ)
	native.setText("project", project)
	if scope, ok, err := a.text("scope"); err != nil {
		return toolResult{}, err
	} else if ok {
		native.set("scope", scope)
	}
	if topic, ok, err := a.text("topic_key"); err != nil {
		return toolResult{}, err
	} else if ok {
		native.set("topic_key", topic)
	}
	if source, ok, err := a.text("source"); err != nil {
		return toolResult{}, err
	} else if ok {
		native.set("source", source)
	}
	out, err := s.runNative(ctx, "brain_note", native)
	if err != nil {
		return toolResult{}, err
	}
	return addStructuredFields(out, map[string]any{"type": typ}), nil
}

func translateSearch(a aliasArgs) (nativeArgs, error) {
	query, err := requireAliasText(a, "query")
	if err != nil {
		return nil, err
	}
	native := newNativeArgs()
	native.set("query", query)
	setProjectFromAlias(native, a)
	if mode, ok, err := a.text("match_mode"); err != nil {
		return nil, err
	} else if ok && strings.TrimSpace(mode) != "" {
		mode = strings.ToLower(strings.TrimSpace(mode))
		if mode != "all" && mode != "any" {
			return nil, refusalInvalid("match_mode must be all or any")
		}
		native.set("match_mode", mode)
	}
	if format, ok, err := a.text("response_format"); err != nil {
		return nil, err
	} else if ok {
		switch strings.ToLower(strings.TrimSpace(format)) {
		case "compact":
			native.set("compact", true)
		case "full", "detailed":
			native.set("compact", false)
		default:
			return nil, refusalInvalid("response_format must be compact or full")
		}
	}
	if all, ok, err := a.boolean("all_projects"); err != nil {
		return nil, err
	} else if ok {
		native.set("all_projects", all)
	}
	if scope, ok, err := a.text("scope"); err != nil {
		return nil, err
	} else if ok {
		native.set("scope", scope)
	}
	if kinds, _, err := a.firstList("types", "type"); err != nil {
		return nil, err
	} else if len(kinds) > 0 {
		native.set("kinds", kinds)
	}
	if since, ok, err := a.text("since"); err != nil {
		return nil, err
	} else if ok {
		native.set("since", since)
	}
	if limit, ok, err := a.integer("limit"); err != nil {
		return nil, err
	} else if ok {
		native.set("limit", limit)
	}
	return native, nil
}

func translateGetObservation(a aliasArgs) (nativeArgs, error) {
	raw, ok := a.raw("id")
	if !ok {
		return nil, refusalInvalid("id is required")
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, refusalNotFound(
			"engram integer observation ids are not addressable here",
			"mem_search to find the record id, then mem_get_observation with it")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, refusalInvalid("id is required")
	}
	if strings.HasPrefix(id, "engram:") {
		return nil, refusalNotFound(
			"the engram sync id "+id+" was not imported into this brain",
			"mem_search to find the record by text, or brain import --from engram to import it")
	}
	native := newNativeArgs()
	native.set("action", "get")
	native.set("id", id)
	if index := strings.Index(id, "/"); index > 0 {
		native.set("id", id[index+1:])
		native.set("project", id[:index])
		return native, nil
	}
	setProjectFromAlias(native, a)
	return native, nil
}

func withObservationLegacyFields(out toolResult) toolResult {
	data, err := json.Marshal(out.Structured)
	if err != nil {
		return out
	}
	var detail RecordDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		return out
	}
	content := detail.Body
	if strings.TrimSpace(content) == "" {
		content = detail.Context
	}
	if strings.TrimSpace(content) == "" {
		content = detail.Result
	}
	if strings.TrimSpace(content) == "" {
		content = detail.Title
	}
	return addStructuredFields(out, map[string]any{"type": detail.Kind, "content": content})
}

func withSearchLegacyFields(out toolResult) toolResult {
	data, err := json.Marshal(out.Structured)
	if err != nil {
		return out
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return out
	}
	hits, _ := object["hits"].([]any)
	for _, raw := range hits {
		hit, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		hit["type"] = hit["kind"]
		if _, ok := hit["content"]; !ok {
			hit["content"] = hit["snippet"]
		}
	}
	out.Structured = object
	return out
}

func updateAlias(ctx context.Context, s *Server, a aliasArgs) (toolResult, error) {
	content, _, ok, err := a.firstText("content", "observation")
	if err != nil {
		return toolResult{}, err
	}
	_, hasFind := a.raw("find")
	_, hasReplace := a.raw("replace")
	if !ok {
		if hasFind || hasReplace {
			return toolResult{}, refusalUnsupported(
				"in-place find/replace editing is not available; the Markdown truth is append-only",
				"mem_update with content and topic_key, or brain_note(topic_key=...)")
		}
		return toolResult{}, refusalInvalid("content is required")
	}
	project, err := projectFromArgs(a)
	if err != nil {
		return toolResult{}, err
	}
	title, _, err := a.text("title")
	if err != nil {
		return toolResult{}, err
	}
	topic, _, err := a.text("topic_key")
	if err != nil {
		return toolResult{}, err
	}
	if strings.TrimSpace(topic) == "" {
		id, _, err := a.text("id")
		if err != nil {
			return toolResult{}, err
		}
		if strings.TrimSpace(id) != "" {
			readArgs := newNativeArgs()
			readArgs.set("action", "get")
			readArgs.set("id", strings.TrimSpace(id))
			readArgs.setText("project", project)
			out, err := s.runNative(ctx, "brain_record", readArgs)
			if err != nil {
				return toolResult{}, err
			}
			data, err := json.Marshal(out.Structured)
			if err != nil {
				return toolResult{}, err
			}
			var detail RecordDetail
			if err := json.Unmarshal(data, &detail); err != nil {
				return toolResult{}, err
			}
			if strings.TrimSpace(title) == "" {
				title = detail.Title
			}
			seed := strings.TrimSpace(detail.Title)
			if seed == "" {
				seed = content
			}
			topic = domain.Slugify(seed)
		}
	}
	if strings.TrimSpace(topic) == "" {
		return toolResult{}, refusalInvalid("topic_key (or an id whose title supplies one) is required")
	}
	native := newNativeArgs()
	native.set("action", "record")
	native.set("text", content)
	native.setText("title", title)
	native.set("topic_key", topic)
	native.setText("project", project)
	out, err := s.runNative(ctx, "brain_note", native)
	if err != nil {
		return toolResult{}, err
	}
	return addStructuredFields(out, map[string]any{"topic_key": topic}), nil
}

func sessionStartAlias(ctx context.Context, s *Server, a aliasArgs) (toolResult, error) {
	project, err := projectFromArgs(a)
	if err != nil {
		return toolResult{}, err
	}
	if strings.TrimSpace(project) != "" {
		target, err := s.resolveTarget(ctx, project)
		if err != nil {
			return toolResult{}, err
		}
		project = target.ID
	} else if allowed, err := s.policy.readProject(); err == nil && allowed != "" {
		project = allowed
	} else {
		project = s.policy.DefaultProject
	}
	session := time.Now().UTC().Format("2006-01-02")
	if project != "" {
		session = project + "/" + session
	}
	structured := map[string]any{
		"session_id": session,
		"degraded":   true,
		"wrote":      false,
		"note":       "sessions are not modelled; this handle is synthetic and stores nothing",
	}
	if project != "" {
		structured["project_id"] = project
	}
	return toolResult{Text: "session: " + session + " (synthetic, nothing stored)", Structured: structured}, nil
}

func sessionEndAlias(ctx context.Context, s *Server, a aliasArgs) (toolResult, error) {
	session, _, err := a.text("session_id")
	if err != nil {
		return toolResult{}, err
	}
	summary, _, err := a.text("summary")
	if err != nil {
		return toolResult{}, err
	}
	if strings.TrimSpace(summary) == "" {
		return toolResult{
			Text: "session " + orNone(session) + " closed without a write",
			Structured: map[string]any{
				"session_id": session,
				"closed":     true,
				"wrote":      false,
				"degraded":   true,
			},
		}, nil
	}
	native := newNativeArgs()
	native.set("summary", summary)
	if project, err := projectFromArgs(a); err != nil {
		return toolResult{}, err
	} else {
		native.setText("project", project)
	}
	out, err := s.runNative(ctx, "brain_handoff", native)
	if err != nil {
		return toolResult{}, err
	}
	return addStructuredFields(out, map[string]any{"session_id": session, "degraded": true}), nil
}

func summaryAlias(ctx context.Context, s *Server, a aliasArgs) (toolResult, error) {
	native, err := handoffAliasArgs(a)
	if err != nil {
		return toolResult{}, err
	}
	return s.runNative(ctx, "brain_handoff", native)
}

func handoffAliasArgs(a aliasArgs) (nativeArgs, error) {
	native := newNativeArgs()
	summary, _, err := a.text("summary")
	if err != nil {
		return nil, err
	}
	native.setText("summary", summary)
	if project, err := projectFromArgs(a); err != nil {
		return nil, err
	} else {
		native.setText("project", project)
	}
	sections := map[string]any{}
	for key, target := range map[string]string{
		"goal": "goal", "instructions": "instructions", "discoveries": "discoveries",
		"accomplished": "accomplished", "next_steps": "next",
	} {
		value, ok, err := a.text(key)
		if err != nil {
			return nil, err
		}
		if ok {
			sections[target] = value
		}
	}
	if files, ok, err := a.list("relevant_files"); err != nil {
		return nil, err
	} else if ok {
		sections["files"] = files
	}
	if len(sections) > 0 {
		native.set("sections", sections)
	}
	if strings.TrimSpace(summary) == "" && len(sections) == 0 {
		return nil, refusalInvalid("pass summary or at least one section")
	}
	return native, nil
}

func finalizeAlias(ctx context.Context, s *Server, a aliasArgs) (toolResult, error) {
	native, err := handoffAliasArgs(a)
	if err != nil {
		return toolResult{}, err
	}
	summary, _, err := a.text("summary")
	if err != nil {
		return toolResult{}, err
	}
	if strings.TrimSpace(summary) == "" {
		sections, _ := nativeArgsToHandoffSections(native)
		summary = renderHandoffSections(sections)
	}
	project, err := projectFromArgs(a)
	if err != nil {
		return toolResult{}, err
	}
	if strings.TrimSpace(project) == "" {
		if target, err := s.resolveTarget(ctx, ""); err == nil {
			project = target.ID
		}
	}
	if strings.TrimSpace(summary) != "" && strings.TrimSpace(project) != "" {
		if replay, err := s.identicalHandoff(ctx, project, summary); err == nil && replay != "" {
			return toolResult{
				Text: "handoff " + replay + " already recorded (replay)",
				Structured: map[string]any{
					"id": replay, "project_id": project, "replayed": true,
				},
			}, nil
		}
	}
	return s.runNative(ctx, "brain_handoff", native)
}

// nativeArgsToHandoffSections decodes the sections object the alias built.
func nativeArgsToHandoffSections(native nativeArgs) (HandoffSection, error) {
	raw, ok := native["sections"]
	if !ok {
		return HandoffSection{}, nil
	}
	var sections struct {
		Goal         string   `json:"goal"`
		Instructions string   `json:"instructions"`
		Discoveries  string   `json:"discoveries"`
		Accomplished string   `json:"accomplished"`
		Next         string   `json:"next"`
		Files        []string `json:"files"`
	}
	if err := json.Unmarshal(raw, &sections); err != nil {
		return HandoffSection{}, err
	}
	return HandoffSection{
		Goal: sections.Goal, Instructions: sections.Instructions, Discoveries: sections.Discoveries,
		Accomplished: sections.Accomplished, Next: sections.Next, Files: sections.Files,
	}, nil
}

// identicalHandoff reports the id of the latest handoff of the project when its
// stored body is exactly the summary about to be written. It is best effort:
// a backend without record reads simply writes.
func (s *Server) identicalHandoff(ctx context.Context, projectSelector, summary string) (string, error) {
	backend, ok := s.backend.(RecordBackend)
	if !ok {
		return "", nil
	}
	target, err := s.resolveTarget(ctx, projectSelector)
	if err != nil {
		return "", err
	}
	records, err := backend.ListRecords(ctx, RecordQuery{ProjectID: target.ID, Kinds: []string{"handoff"}, Limit: 5})
	if err != nil {
		return "", err
	}
	for _, record := range records {
		detail, err := backend.GetRecord(ctx, target.ID, record.ID)
		if err != nil {
			continue
		}
		if strings.TrimSpace(detail.Body) == strings.TrimSpace(summary) {
			return record.ID, nil
		}
	}
	return "", nil
}

func reviewAlias(ctx context.Context, s *Server, a aliasArgs) (toolResult, error) {
	action, _, err := a.text("action")
	if err != nil {
		return toolResult{}, err
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		action = "list"
	}
	if action == "list" {
		native := newNativeArgs()
		native.set("action", "due")
		setProjectFromAlias(native, a)
		if due, ok, err := a.text("due_before"); err != nil {
			return toolResult{}, err
		} else if ok {
			native.set("due_before", due)
		}
		return s.runNative(ctx, "brain_review", native)
	}
	if action != "mark_reviewed" && action != "mark" && action != "review" {
		return toolResult{}, refusalInvalidf("unknown action %q (list, mark_reviewed)", action)
	}
	native := newNativeArgs()
	native.set("action", "review")
	setProjectFromAlias(native, a)
	if id, _, _, err := a.firstText("observation_id", "id"); err != nil {
		return toolResult{}, err
	} else if id != "" {
		native.set("id", id)
	}
	if all, ok, err := a.boolean("all"); err != nil {
		return toolResult{}, err
	} else if ok && all {
		native.set("ids", []string{"*"})
	}
	if trust, ok, err := a.text("trust"); err != nil {
		return toolResult{}, err
	} else if ok {
		native.set("trust", trust)
	}
	return s.runNative(ctx, "brain_judge", native)
}

func translateJudge(a aliasArgs) (nativeArgs, error) {
	native := newNativeArgs()
	native.set("action", "judge")
	setProjectFromAlias(native, a)
	from, _, err := a.text("from")
	if err != nil {
		return nil, err
	}
	to, _, err := a.text("to")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
		id, _, err := a.text("judgment_id")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(id) != "" {
			pairFrom, pairTo, ok := judgmentPair(id)
			if !ok {
				return nil, refusalInvalidWith(
					"judgment_id "+id+" is not a <source>-><target> pair",
					"mem_judge with from and to; brain_record(action=relations) lists source_id and target_id")
			}
			from, to = pairFrom, pairTo
		}
	}
	if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
		return nil, refusalInvalid("from and to (or a <source>-><target> judgment_id) are required")
	}
	native.set("from", strings.TrimSpace(from))
	native.set("to", strings.TrimSpace(to))
	if verdict, ok, err := a.text("verdict"); err != nil {
		return nil, err
	} else if ok {
		native.set("verdict", legacyVerdict(verdict))
	}
	if reason, ok, err := a.text("reason"); err != nil {
		return nil, err
	} else if ok {
		native.set("reason", reason)
	}
	if confidence, ok, err := a.number("confidence"); err != nil {
		return nil, err
	} else if ok {
		native.set("confidence", confidence)
	}
	return native, nil
}

func translateJudgeMany(a aliasArgs) (nativeArgs, error) {
	raw, ok := a.raw("judgments")
	if !ok {
		return nil, refusalInvalid("judgments is required")
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, refusalInvalid("judgments must be an array of objects")
	}
	translated := make([]map[string]any, 0, len(items))
	for index, item := range items {
		args := aliasArgs(item)
		from, _, err := args.text("from")
		if err != nil {
			return nil, err
		}
		to, _, err := args.text("to")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
			id, _, err := args.text("judgment_id")
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(id) != "" {
				pairFrom, pairTo, ok := judgmentPair(id)
				if !ok {
					return nil, refusalInvalidf("judgments[%d].judgment_id is not a <source>-><target> pair", index)
				}
				from, to = pairFrom, pairTo
			}
		}
		if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
			return nil, refusalInvalidf("judgments[%d] needs from and to, or a judgment_id pair", index)
		}
		verdict, _, err := args.text("verdict")
		if err != nil {
			return nil, err
		}
		reason, _, err := args.text("reason")
		if err != nil {
			return nil, err
		}
		entry := map[string]any{"from": strings.TrimSpace(from), "to": strings.TrimSpace(to),
			"verdict": legacyVerdict(verdict), "reason": reason}
		if confidence, ok, err := args.number("confidence"); err != nil {
			return nil, err
		} else if ok {
			entry["confidence"] = confidence
		}
		translated = append(translated, entry)
	}
	native := newNativeArgs()
	native.set("action", "judge_many")
	native.set("judgments", translated)
	setProjectFromAlias(native, a)
	return native, nil
}

func (a aliasArgs) number(key string) (float64, bool, error) {
	raw, ok := a.raw(key)
	if !ok {
		return 0, false, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, true, refusalInvalidf("%s must be a number", key)
	}
	return value, true, nil
}

func translateCompare(a aliasArgs) (nativeArgs, error) {
	from, _, err := a.text("memory_id_a")
	if err != nil {
		return nil, err
	}
	to, _, err := a.text("memory_id_b")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
		return nil, refusalInvalid("memory_id_a and memory_id_b are required")
	}
	native := newNativeArgs()
	native.set("action", "compare")
	native.set("from", strings.TrimSpace(from))
	native.set("to", strings.TrimSpace(to))
	if project, err := projectFromArgs(a); err != nil {
		return nil, err
	} else {
		native.setText("project", project)
	}
	if reasoning, ok, err := a.text("reasoning"); err != nil {
		return nil, err
	} else if ok {
		native.set("reason", reasoning)
	}
	if verdict, ok, err := a.text("verdict"); err != nil {
		return nil, err
	} else if ok && strings.TrimSpace(verdict) != "" {
		native.set("verdict", legacyVerdict(verdict))
	}
	return native, nil
}

func statsAlias(ctx context.Context, s *Server, _ aliasArgs) (toolResult, error) {
	projects, err := s.backend.Projects(ctx)
	if err != nil {
		return toolResult{}, err
	}
	projects = s.policy.filterProjects(projects)
	projects = append([]Project(nil), projects...)
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	structured := map[string]any{"projects": len(projects)}
	backend, ok := s.backend.(RecordBackend)
	if !ok {
		return toolResult{Text: fmt.Sprintf("%d projects", len(projects)), Structured: structured}, nil
	}
	byKind := map[string]int{}
	byProject := map[string]int{}
	total := 0
	for _, project := range projects {
		records, err := backend.ListRecords(ctx, RecordQuery{ProjectID: project.ID, Limit: countsScanLimit})
		if err != nil {
			return toolResult{}, err
		}
		total += len(records)
		byProject[project.ID] = len(records)
		for _, record := range records {
			byKind[record.Kind]++
		}
	}
	structured["records"] = total
	structured["by_kind"] = byKind
	structured["by_project"] = byProject
	return toolResult{
		Text:       fmt.Sprintf("%d projects, %d records", len(projects), total),
		Structured: structured,
	}, nil
}

// ---------------------------------------------------------------------------
// codegraph and graphify placeholders

// codegraphStubTools is the 8-name codegraph vocabulary. Queries refuse with a
// reason; status and files return an honest indexed=false object. No query
// returns an empty collection as if it were an answer.
func codegraphStubTools() []tool {
	querySchema := stubSchema(map[string]any{
		"query": stringProp("What to look up."), "path": stringProp("Checkout path."),
		"file": stringProp("File path."), "symbol": stringProp("Symbol name."),
		"node": stringProp("Node id."), "id": stringProp("Node id."),
		"depth": intProp("Traversal depth.", 0), "limit": intProp("Row limit.", 0),
	})
	refuse := func(name string) toolFunc {
		return func(context.Context, json.RawMessage) (toolResult, error) {
			return toolResult{}, refusalUnsupported(
				name+" is not available: no code index exists in 0.17",
				"brain_search (memory) or brain graphs (checkout)")
		}
	}
	read := func(name, title, description string, run toolFunc) tool {
		return tool{
			Name: name, Description: description, Annotations: readOnlyAnnotations(title),
			InputSchema: querySchema, OutputSchema: outputObject(map[string]any{}),
			Stub: true, Note: "code index lands in 0.18", run: run,
		}
	}
	tools := []tool{
		read("codegraph_explore", "Explore code", "Explore a symbol and its neighbours. Refused in 0.17: no code index.", refuse("codegraph_explore")),
		read("codegraph_search", "Search code", "Search code symbols. Refused in 0.17: no code index.", refuse("codegraph_search")),
		read("codegraph_node", "Read a code node", "Read one code node. Refused in 0.17: no code index.", refuse("codegraph_node")),
		read("codegraph_callers", "List callers", "List callers of a symbol. Refused in 0.17: no code index.", refuse("codegraph_callers")),
		read("codegraph_callees", "List callees", "List callees of a symbol. Refused in 0.17: no code index.", refuse("codegraph_callees")),
		read("codegraph_impact", "Report impact", "Report the blast radius of a change. Refused in 0.17: no code index.", refuse("codegraph_impact")),
	}
	status := tool{
		Name: "codegraph_status", Description: "Report whether a code graph is indexed. 0.17 has no code index, so indexed is always false; no timestamp or count is fabricated.",
		Annotations: readOnlyAnnotations("Code index status"), InputSchema: querySchema,
		OutputSchema: outputObject(map[string]any{
			"indexed": boolProp("Whether a code index exists."), "engine": stringProp("Graph engine."),
			"hint": stringProp("How to build the index."), "reason": stringProp("Why it is not indexed."),
		}, "indexed", "engine"),
		Stub: true, Note: "code index lands in 0.18",
		run: func(context.Context, json.RawMessage) (toolResult, error) {
			structured := map[string]any{
				"indexed": false, "engine": "codegraph", "hint": "run brain code index",
				"reason": "no code index in 0.17",
			}
			return toolResult{Text: "codegraph: not indexed (no code index in 0.17)", Structured: structured}, nil
		},
	}
	files := tool{
		Name: "codegraph_files", Description: "List indexed code files. 0.17 has no code index, so the list is empty and indexed is false.",
		Annotations: readOnlyAnnotations("Code index files"), InputSchema: querySchema,
		OutputSchema: outputObject(map[string]any{
			"files":   arrayProp("Indexed files; empty because nothing is indexed.", stringProp("File path.")),
			"indexed": boolProp("Whether a code index exists."), "engine": stringProp("Graph engine."),
			"hint": stringProp("How to build the index."), "reason": stringProp("Why it is not indexed."),
		}, "files", "indexed"),
		Stub: true, Note: "code index lands in 0.18",
		run: func(context.Context, json.RawMessage) (toolResult, error) {
			structured := map[string]any{
				"files": []string{}, "indexed": false, "engine": "codegraph",
				"hint": "run brain code index", "reason": "no code index in 0.17",
			}
			return toolResult{Text: "codegraph: not indexed (no code index in 0.17)", Structured: structured}, nil
		},
	}
	tools = append(tools, files, status)
	return tools
}

// graphifyStubTools is the 10-name graphify vocabulary. graph_stats returns an
// honest indexed=false object with zero counts; every query refuses.
func graphifyStubTools() []tool {
	querySchema := stubSchema(map[string]any{
		"query": stringProp("Graph query."), "node": stringProp("Node id."),
		"id": stringProp("Node id."), "source": stringProp("Source node."),
		"target": stringProp("Target node."), "community": stringProp("Community id."),
		"depth": intProp("Traversal depth.", 0), "limit": intProp("Row limit.", 0),
	})
	refuse := func(name, reason string) toolFunc {
		return func(context.Context, json.RawMessage) (toolResult, error) {
			return toolResult{}, refusalUnsupported(name+" is not available: "+reason, "brain graphs (checkout) or brain_search (memory)")
		}
	}
	read := func(name, title, description string, run toolFunc) tool {
		return tool{
			Name: name, Description: description, Annotations: readOnlyAnnotations(title),
			InputSchema: querySchema, OutputSchema: outputObject(map[string]any{}),
			Stub: true, Note: "knowledge graph lands in 0.18", run: run,
		}
	}
	return []tool{
		read("query_graph", "Query the knowledge graph", "Query the knowledge graph. Refused in 0.17: no graph index.", refuse("query_graph", "no knowledge graph exists in 0.17")),
		read("get_node", "Read a graph node", "Read one graph node. Refused in 0.17: no graph index.", refuse("get_node", "no knowledge graph exists in 0.17")),
		read("get_neighbors", "List graph neighbours", "List the neighbours of a node. Refused in 0.17: no graph index.", refuse("get_neighbors", "no knowledge graph exists in 0.17")),
		read("get_community", "Read a graph community", "Read one community. Refused in 0.17: no graph index.", refuse("get_community", "no knowledge graph exists in 0.17")),
		read("shortest_path", "Shortest path", "Find the shortest path between two nodes. Refused in 0.17: no graph index.", refuse("shortest_path", "no knowledge graph exists in 0.17")),
		read("god_nodes", "Central graph nodes", "Report the most connected nodes. Refused in 0.17: no graph index.", refuse("god_nodes", "no knowledge graph exists in 0.17")),
		{
			Name:        "graph_stats",
			Description: "Report graph size. 0.17 has no knowledge graph, so indexed is false and every count is zero; the reason says so.",
			Annotations: readOnlyAnnotations("Graph statistics"), InputSchema: querySchema,
			OutputSchema: outputObject(map[string]any{
				"indexed": boolProp("Whether a graph index exists."), "engine": stringProp("Graph engine."),
				"nodes": intProp("Node count.", 0), "edges": intProp("Edge count.", 0),
				"communities": intProp("Community count.", 0),
				"hint":        stringProp("How to build the graph."), "reason": stringProp("Why it is not indexed."),
			}, "indexed", "engine", "nodes", "edges", "communities"),
			Stub: true, Note: "knowledge graph lands in 0.18",
			run: func(context.Context, json.RawMessage) (toolResult, error) {
				structured := map[string]any{
					"indexed": false, "engine": "graphify", "nodes": 0, "edges": 0, "communities": 0,
					"hint": "run brain graphs index", "reason": "no knowledge graph in 0.17",
				}
				return toolResult{Text: "graphify: not indexed (no knowledge graph in 0.17)", Structured: structured}, nil
			},
		},
		read("list_prs", "List pull requests", "List pull requests. Refused permanently: connector PR dumps are not a graph.", refuse("list_prs", "pull requests are not part of this graph")),
		read("get_pr_impact", "Pull request impact", "Report the impact of a pull request. Refused permanently: connector PR dumps are not a graph.", refuse("get_pr_impact", "pull requests are not part of this graph")),
		read("triage_prs", "Triage pull requests", "Triage pull requests. Refused permanently: connector PR dumps are not a graph.", refuse("triage_prs", "pull requests are not part of this graph")),
	}
}

// stubSchema accepts the usual code-graph arguments and ignores them. A stub
// must reach its refusal, not fail on an unknown field.
func stubSchema(properties map[string]any) map[string]any {
	schema := objectSchema(nil, properties)
	schema["additionalProperties"] = true
	return schema
}

// Aliases returns the public alias table of a profile. The native profile and
// the two code-graph placeholders have no A-D aliases.
func Aliases(profile Profile) []AliasSpec {
	var specs []aliasSpec
	switch profile.Name {
	case ProfileLV24Legacy:
		specs = legacyAliasSpecs()
	case ProfileEngram:
		specs = engramAliasSpecs()
	default:
		return nil
	}
	aliases := make([]AliasSpec, 0, len(specs))
	for _, spec := range specs {
		alias := spec.AliasSpec
		if profile.Name == ProfileLV24Legacy {
			alias.Deprecated = true
			alias.Reason = profileLegacyReason
		}
		aliases = append(aliases, alias)
	}
	return aliases
}

// profileGaps reports the aliases and stubs the active profile cannot serve
// faithfully. The native gaps stay visible in the native and bridge profiles.
func (s *Server) profileGaps() []CapabilityGap {
	switch s.profile.Name {
	case ProfileEngram:
		return engramGaps()
	case ProfileCodegraph:
		return codegraphGaps()
	case ProfileGraphify:
		return graphifyGaps()
	default:
		return capabilityGaps()
	}
}

func engramGaps() []CapabilityGap {
	gaps := make([]CapabilityGap, 0)
	for _, spec := range engramAliasSpecs() {
		switch spec.Grade {
		case GradeApproximate:
			remediation := spec.Target
			if remediation == "" {
				remediation = "run without --strict; this alias returns a synthetic handle and writes nothing"
			}
			gaps = append(gaps, CapabilityGap{
				Tool: spec.Legacy, Grade: spec.Grade, Reason: spec.Note, Remediation: remediation,
			})
		case GradeUnsupported:
			gaps = append(gaps, CapabilityGap{
				Tool: spec.Legacy, Grade: spec.Grade, Reason: spec.Reason, Remediation: spec.Remediation,
			})
		}
	}
	gaps = append(gaps, CapabilityGap{
		Tool: "mem_consolidate", Grade: GradeEnvelope, Reason: "two-phase consolidation is refused by the engine in 0.17",
		Remediation: "brain record supersede <old-id> --by <new-id>",
	})
	return gaps
}

func codegraphGaps() []CapabilityGap {
	gaps := make([]CapabilityGap, 0, 8)
	for _, tool := range codegraphStubTools() {
		reason := "UNSUPPORTED: no code index in 0.17"
		remediation := "brain_search (memory) or brain graphs (checkout)"
		switch tool.Name {
		case "codegraph_status", "codegraph_files":
			reason = "honest stub: no code index in 0.17"
			remediation = "run brain code index (0.18)"
		}
		gaps = append(gaps, CapabilityGap{Tool: tool.Name, Grade: GradeStub, Reason: reason, Remediation: remediation})
	}
	return gaps
}

func graphifyGaps() []CapabilityGap {
	gaps := make([]CapabilityGap, 0, 10)
	for _, tool := range graphifyStubTools() {
		reason := "UNSUPPORTED: no knowledge graph in 0.17"
		remediation := "brain graphs (checkout) or brain_search (memory)"
		switch tool.Name {
		case "graph_stats":
			reason = "honest stub: no knowledge graph in 0.17"
			remediation = "run brain graphs index (0.18)"
		case "list_prs", "get_pr_impact", "triage_prs":
			reason = "UNSUPPORTED permanently: connector PR dumps are not a graph"
		}
		gaps = append(gaps, CapabilityGap{Tool: tool.Name, Grade: GradeStub, Reason: reason, Remediation: remediation})
	}
	return gaps
}
