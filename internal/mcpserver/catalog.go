package mcpserver

import (
	"fmt"
	"strings"
)

// Tool profiles. A profile is a vocabulary, not an add-on: tools/list in a
// profile never mixes two names for the same intent. 0.17 serves one profile,
// the native 15-tool surface; later releases add migration vocabularies.
const (
	// ProfileLV24 is the native 15-tool profile and the default.
	ProfileLV24 = "lv24"
	// ProfileVersion is the additive-compatible revision of the profile. A
	// minor bump adds names or fields; a major bump changes what an existing
	// name means.
	ProfileVersion = "1.0"
)

// Profile is the tool vocabulary one server process exposes.
type Profile struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Strict  bool   `json:"strict"`
}

// DefaultProfile returns the native profile.
func DefaultProfile() Profile {
	return Profile{Name: ProfileLV24, Version: ProfileVersion}
}

// ResolveProfile validates the profile flags. An unknown profile or an
// unsupported version pin is a plain error: the server refuses to start rather
// than silently serve another vocabulary.
func ResolveProfile(name, version string, strict bool) (Profile, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = ProfileLV24
	}
	if name != ProfileLV24 {
		return Profile{}, fmt.Errorf("unknown profile %q (this build serves: %s)", name, ProfileLV24)
	}
	switch strings.TrimSpace(version) {
	case "", "1", ProfileVersion:
	default:
		return Profile{}, fmt.Errorf("unsupported profile version %q (this build serves: %s)", version, ProfileVersion)
	}
	return Profile{Name: name, Version: ProfileVersion, Strict: strict}, nil
}

// CapabilityGap is one action the surface cannot serve and the CLI path that
// does. It is reported by brain_capabilities and by --list-tools.
type CapabilityGap struct {
	Tool        string `json:"tool"`
	Action      string `json:"action,omitempty"`
	Reason      string `json:"reason"`
	Remediation string `json:"remediation"`
}

// ToolSummary counts the tools of one profile.
type ToolSummary struct {
	Total       int      `json:"total"`
	ReadOnly    int      `json:"read_only"`
	AppendOnly  int      `json:"append_only"`
	Destructive int      `json:"destructive"`
	Names       []string `json:"names"`
}

// Capabilities is the contract introspection payload: what this profile
// serves, what it cannot serve, the limits and the refusal vocabulary. It is
// served by brain_capabilities and by `brain mcp --list-tools --json`.
type Capabilities struct {
	Profile        string            `json:"profile"`
	ProfileVersion string            `json:"profile_version"`
	Engine         string            `json:"engine"`
	Policy         PolicyInfo        `json:"policy"`
	Tools          ToolSummary       `json:"tools"`
	Features       map[string]any    `json:"features"`
	Limits         map[string]int    `json:"limits"`
	Gaps           []CapabilityGap   `json:"gaps"`
	RefusalCodes   []string          `json:"refusal_codes"`
	Endpoints      map[string]bool   `json:"endpoints"`
	Schemas        map[string]string `json:"schemas"`
}

// ToolSpec is the protocol metadata of one tool: everything tools/list reports
// plus the limits the catalogue documents. It never runs a tool.
type ToolSpec struct {
	Name         string         `json:"name"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	Annotations  Annotations    `json:"annotations"`
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`
	Limits       map[string]int `json:"limits,omitempty"`
	// Policy is read, append or destructive: the write posture a host can
	// show without interpreting four hints.
	Policy string `json:"policy,omitempty"`
	// ReadActions names the actions that never mutate, for a mixed tool whose
	// destructiveHint covers the other actions. Clients ignore it.
	ReadActions []string `json:"x-lv24-read-actions,omitempty"`
}

// toolPolicy names the write posture of a tool.
func toolPolicy(t tool) string {
	switch {
	case t.Annotations.ReadOnlyHint:
		return "read"
	case t.Annotations.DestructiveHint:
		return "destructive"
	default:
		return "append"
	}
}

// Catalog returns the metadata of every tool the profile exposes. It is what
// `brain mcp --list-tools --json` prints, so the contract can be inspected
// without a backend, a data directory or a running server. version is reported
// as serverInfo.version.
func Catalog(version string, profile Profile) []ToolSpec {
	if profile.Name == "" {
		profile = DefaultProfile()
	}
	s := &Server{version: version, profile: profile, catalog: true}
	s.allTools = s.toolList()
	s.tools = s.allTools
	specs := make([]ToolSpec, 0, len(s.tools))
	for _, t := range s.tools {
		specs = append(specs, ToolSpec{
			Name:         t.Name,
			Title:        t.Annotations.Title,
			Description:  t.Description,
			Annotations:  t.Annotations,
			InputSchema:  t.InputSchema,
			OutputSchema: t.OutputSchema,
			Limits:       t.Limits,
			Policy:       toolPolicy(t),
			ReadActions:  t.ReadActions,
		})
	}
	return specs
}

// CatalogForPolicy splits the catalogue into the tools a server with this
// policy exposes and the names it omits. It is what `brain mcp --list-tools`
// prints, so a host can see exactly which tools a profile offers.
func CatalogForPolicy(version string, profile Profile, policy Policy) (visible []ToolSpec, omitted []string) {
	specs := Catalog(version, profile)
	visible = make([]ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if policy.ToolListed(spec) {
			visible = append(visible, spec)
			continue
		}
		omitted = append(omitted, spec.Name)
	}
	return visible, omitted
}

// CapabilityReport builds the capabilities payload of a profile under a
// policy without starting a server. brain_capabilities and --list-tools both
// use it, so the two cannot drift.
func CapabilityReport(version string, profile Profile, policy Policy) Capabilities {
	if profile.Name == "" {
		profile = DefaultProfile()
	}
	s := &Server{version: version, profile: profile, policy: policy, catalog: true}
	s.allTools = s.toolList()
	s.tools = s.applyPolicy(s.allTools)
	return s.capabilities()
}

// capabilities assembles the payload from the effective tool list.
func (s *Server) capabilities() Capabilities {
	summary := ToolSummary{Total: len(s.tools), Names: make([]string, 0, len(s.tools))}
	for _, t := range s.tools {
		summary.Names = append(summary.Names, t.Name)
		switch toolPolicy(t) {
		case "read":
			summary.ReadOnly++
		case "destructive":
			summary.Destructive++
		default:
			summary.AppendOnly++
		}
	}
	return Capabilities{
		Profile:        s.profile.Name,
		ProfileVersion: s.profile.Version,
		Engine:         s.version,
		Policy:         s.policy.Info(),
		Tools:          summary,
		Features: map[string]any{
			"full_text":       true,
			"semantic_search": false,
			"topics":          true,
			"pinning":         false,
			"record_history":  "supersede-chain",
			"relations":       true,
			"sessions":        false,
			"consolidation":   false,
			"prompt_zone":     "local-only",
			"code_graph":      false,
			"knowledge_graph": false,
			"resources":       false,
			"prompts":         false,
		},
		Limits: map[string]int{
			"body_bytes":         1 << 20,
			"search_page":        searchMaxPageSize,
			"search_window":      searchWindow,
			"record_page":        recordsMaxPageSize,
			"relation_page":      relationsMaxPageSize,
			"journal_page":       journalMaxPageSize,
			"context_tokens":     32000,
			"expand_tokens":      ExpandMaxTokens,
			"judgments_per_call": maxJudgeMany,
			"prompt_page":        promptMaxPageSize,
		},
		Gaps:         capabilityGaps(),
		RefusalCodes: RefusalCodes(),
		Endpoints: map[string]bool{
			"stdio": true, "http": false, "sse": false, "resources": false, "prompts": false,
		},
		Schemas: map[string]string{"record": "knowledge.v1", "refusal": "refusal.v1"},
	}
}

// capabilityGaps lists the actions this profile accepts but cannot serve, with
// the CLI path that can. A gap is never a silent no-op.
func capabilityGaps() []CapabilityGap {
	return []CapabilityGap{
		{Tool: "brain_record", Action: "list due_review", Reason: "the due queue is a review action", Remediation: "brain_review(action=due)"},
		{Tool: "brain_search", Action: "include_prompts", Reason: "the prompt zone is local-only and never enters the search index", Remediation: "brain_prompt(action=search)"},
		{Tool: "brain_prompt", Action: "discard", Reason: "no app method deletes a prompt in 0.17", Remediation: "edit projects/<id>/prompts/<date>.md by hand"},
		{Tool: "brain_projects", Action: "merge", Reason: "no app method merges projects in 0.17", Remediation: "brain project set <id> --alias <old-id> (renames) or brain import --from engram --map-project <from>=<to>"},
		{Tool: "brain_judge", Action: "pin", Reason: "pinning needs additive frontmatter and ranking weight (0.18)", Remediation: "brain note \"<why this record matters>\""},
		{Tool: "brain_judge", Action: "unpin", Reason: "pinning needs additive frontmatter and ranking weight (0.18)", Remediation: "brain note \"<why this record matters>\""},
		{Tool: "brain_judge", Action: "consolidate", Reason: "two-phase consolidation is not exposed over MCP in 0.17", Remediation: "brain record supersede <old-id> --by <new-id>"},
	}
}

// gapFor reports whether an action is a documented gap.
func gapFor(tool, action string) (CapabilityGap, bool) {
	for _, gap := range capabilityGaps() {
		if gap.Tool == tool && gap.Action == action {
			return gap, true
		}
	}
	return CapabilityGap{}, false
}

// refusedGap turns a documented gap into an UNSUPPORTED refusal whose hint is
// the CLI path that can do the work.
func refusedGap(tool, action string) error {
	gap, ok := gapFor(tool, action)
	if !ok {
		return refusalUnsupported(tool+" action "+action+" is not available", "run brain --help")
	}
	return refusalUnsupported(tool+" action "+action+": "+gap.Reason, gap.Remediation)
}
