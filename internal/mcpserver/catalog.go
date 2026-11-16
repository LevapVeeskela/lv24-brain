package mcpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Tool profiles. A profile is a vocabulary, not an add-on: tools/list in a
// profile never lists two names for the same intent. The native profile serves
// the 17 tools of 0.18; the migration profiles serve a peer vocabulary whose
// calls are translated to the native tool that does the work.
const (
	// ProfileLV24 is the native 17-tool profile and the default.
	ProfileLV24 = "lv24"
	// ProfileLV24Legacy is the 24-name bridge for clients written against
	// 0.16: the 17 native names plus the seven retired ones.
	ProfileLV24Legacy = "lv24-legacy"
	// ProfileEngram is the 27-name engram-compatible vocabulary.
	ProfileEngram = "engram"
	// ProfileCodegraph is the 8-name codegraph vocabulary, mapped to
	// brain_code wherever the semantics match.
	ProfileCodegraph = "codegraph"
	// ProfileGraphify is the 10-name graphify vocabulary, mapped to brain_code
	// and brain_graph; the analytics names refuse honestly.
	ProfileGraphify = "graphify"
	// ProfileVersion is the additive-compatible revision of every profile. A
	// minor bump adds names or fields; a major bump changes what an existing
	// name means.
	ProfileVersion = "1.0"
)

// Compatibility grades. A and B preserve the facts of the legacy call; C
// approximates and says so; D cannot be served at all. GradeStub marks a
// placeholder tool; this release serves no code-graph stub.
const (
	GradeExact          = "A"
	GradeEnvelope       = "B"
	GradeApproximate    = "C"
	GradeUnsupported    = "D"
	GradeStub           = "stub"
	profileDeprecation  = "lv24-legacy is a bridge for 0.16 clients and is removed in 0.19"
	profileLegacyReason = "the 0.16 name was retired in 0.17; this profile keeps it callable"
)

// SupportedProfiles is the closed profile vocabulary of this build, in the
// order --list-tools and error messages report it.
var SupportedProfiles = []string{
	ProfileLV24,
	ProfileLV24Legacy,
	ProfileEngram,
	ProfileCodegraph,
	ProfileGraphify,
}

// Profile is the tool vocabulary one server process exposes.
type Profile struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Strict  bool   `json:"strict"`
	// Native reports whether the profile is the native vocabulary.
	Native bool `json:"native"`
	// Deprecated marks a bridge profile that a later release removes.
	Deprecated bool `json:"deprecated,omitempty"`
}

// DefaultProfile returns the native profile.
func DefaultProfile() Profile {
	return Profile{Name: ProfileLV24, Version: ProfileVersion, Native: true}
}

// ResolveProfile validates the profile flags. An unknown profile or an
// unsupported version pin is a plain error: the server refuses to start rather
// than silently serve another vocabulary.
func ResolveProfile(name, version string, strict bool) (Profile, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = ProfileLV24
	}
	if !slices.Contains(SupportedProfiles, name) {
		return Profile{}, fmt.Errorf("unknown profile %q (this build serves: %s)", name, strings.Join(SupportedProfiles, ", "))
	}
	switch strings.TrimSpace(version) {
	case "", "1", ProfileVersion:
	default:
		return Profile{}, fmt.Errorf("unsupported profile version %q (this build serves: %s; pass 1 to pin the major)", version, ProfileVersion)
	}
	return Profile{
		Name:       name,
		Version:    ProfileVersion,
		Strict:     strict,
		Native:     name == ProfileLV24,
		Deprecated: name == ProfileLV24Legacy,
	}, nil
}

// CapabilityGap is one action the surface cannot serve and the path that can.
// It is reported by brain_capabilities and by --list-tools.
type CapabilityGap struct {
	Tool        string `json:"tool"`
	Action      string `json:"action,omitempty"`
	Grade       string `json:"grade,omitempty"`
	Reason      string `json:"reason"`
	Remediation string `json:"remediation"`
}

// ToolSummary counts the tools of one profile.
type ToolSummary struct {
	Total       int `json:"total"`
	ReadOnly    int `json:"read_only"`
	AppendOnly  int `json:"append_only"`
	Destructive int `json:"destructive"`
	// Native counts tools of the native surface, Aliased counts legacy names
	// mapped to a native call, and Stubs counts honest code-graph stubs.
	Native  int            `json:"native"`
	Aliased int            `json:"aliased"`
	Stubs   int            `json:"stubs,omitempty"`
	Grades  map[string]int `json:"grades,omitempty"`
	Names   []string       `json:"names"`
}

// Capabilities is the contract introspection payload: what this profile
// serves, what it cannot serve, the limits and the refusal vocabulary. It is
// served by brain_capabilities and by `brain mcp --list-tools --json`, so the
// two cannot drift.
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

// AliasSpec is one legacy name of a migration profile. Target is empty when
// the alias has no native equivalent (grade D). Unsupported lists the legacy
// arguments the translation drops.
type AliasSpec struct {
	Legacy      string   `json:"legacy"`
	Target      string   `json:"target"`
	Action      string   `json:"action,omitempty"`
	Grade       string   `json:"grade"`
	Deprecated  bool     `json:"deprecated,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	Remediation string   `json:"remediation,omitempty"`
	Unsupported []string `json:"unsupported,omitempty"`
	Note        string   `json:"note,omitempty"`
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
	// WriteActions names the mutating actions of a mixed tool, so a read-only
	// catalogue can list the read half and refuse the rest.
	WriteActions []string `json:"x-lv24-write-actions,omitempty"`
	// Legacy marks a migration alias. Target, Action and Grade describe what
	// it calls and how faithfully; Unsupported lists the dropped legacy
	// arguments.
	Legacy      bool     `json:"legacy,omitempty"`
	Deprecated  bool     `json:"deprecated,omitempty"`
	Target      string   `json:"target,omitempty"`
	Action      string   `json:"action,omitempty"`
	Grade       string   `json:"grade,omitempty"`
	Unsupported []string `json:"unsupported,omitempty"`
	Note        string   `json:"note,omitempty"`
	// Reason and Remediation explain a D alias: why it is refused and which
	// CLI path can do the work.
	Reason      string `json:"reason,omitempty"`
	Remediation string `json:"remediation,omitempty"`
	// Stub marks a codegraph/graphify placeholder that never fabricates data.
	Stub bool `json:"stub,omitempty"`
}

// Catalogue is the complete `brain mcp --list-tools --json` contract: the
// effective tool list, the alias table, the capabilities payload and a digest
// over the vocabulary. Digest is stable for the same profile and policy.
type Catalogue struct {
	Profile      Profile      `json:"profile"`
	Tools        []ToolSpec   `json:"tools"`
	Aliases      []AliasSpec  `json:"aliases"`
	Capabilities Capabilities `json:"capabilities"`
	OmittedTools []string     `json:"omitted_tools"`
	Digest       string       `json:"digest"`
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

// newCatalogueServer builds a backend-less server whose tool list is the
// catalogue of the profile under the policy.
func newCatalogueServer(version string, profile Profile, policy Policy) *Server {
	if profile.Name == "" {
		profile = DefaultProfile()
	}
	s := &Server{version: version, profile: profile, policy: policy, catalog: true}
	s.buildTools()
	return s
}

// specsOf renders the tool metadata of a list.
func specsOf(tools []tool) []ToolSpec {
	specs := make([]ToolSpec, 0, len(tools))
	for _, t := range tools {
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
			WriteActions: t.WriteActions,
			Legacy:       t.Legacy,
			Deprecated:   t.Deprecated,
			Target:       t.Target,
			Action:       t.Action,
			Grade:        t.Grade,
			Unsupported:  t.Unsupported,
			Note:         t.Note,
			Reason:       t.Reason,
			Remediation:  t.Remediation,
			Stub:         t.Stub,
		})
	}
	return specs
}

// Catalog returns the metadata of every tool the profile exposes. It is what
// `brain mcp --list-tools --json` prints, so the contract can be inspected
// without a backend, a data directory or a running server. version is reported
// as serverInfo.version.
func Catalog(version string, profile Profile) []ToolSpec {
	visible, _ := CatalogForPolicy(version, profile, Policy{})
	return visible
}

// CatalogForPolicy splits the catalogue into the tools a server with this
// policy exposes and the names it omits. It is what `brain mcp --list-tools`
// prints, so a host can see exactly which tools a profile offers.
func CatalogForPolicy(version string, profile Profile, policy Policy) (visible []ToolSpec, omitted []string) {
	s := newCatalogueServer(version, profile, policy)
	return specsOf(s.tools), omittedNames(s.allTools, s.tools)
}

// omittedNames returns the names of all that are not visible, in list order.
func omittedNames(all, visible []tool) []string {
	shown := make(map[string]bool, len(visible))
	for _, t := range visible {
		shown[t.Name] = true
	}
	omitted := make([]string, 0)
	for _, t := range all {
		if !shown[t.Name] {
			omitted = append(omitted, t.Name)
		}
	}
	return omitted
}

// CapabilityReport builds the capabilities payload of a profile under a
// policy without starting a server. brain_capabilities and --list-tools both
// use it, so the two cannot drift.
func CapabilityReport(version string, profile Profile, policy Policy) Capabilities {
	return newCatalogueServer(version, profile, policy).capabilities()
}

// capabilityMeta wraps the payload in the reserved _meta key of initialize,
// server/discover and each result.
func capabilityMeta(payload Capabilities) map[string]any {
	return map[string]any{"lv24.brain/capabilities": payload}
}

// BuildCatalogue assembles the --list-tools document and its digest.
func BuildCatalogue(version string, profile Profile, policy Policy) Catalogue {
	s := newCatalogueServer(version, profile, policy)
	catalogue := Catalogue{
		Profile:      s.profile,
		Tools:        specsOf(s.tools),
		Aliases:      Aliases(s.profile),
		Capabilities: s.capabilities(),
		OmittedTools: omittedNames(s.allTools, s.tools),
	}
	if catalogue.Aliases == nil {
		catalogue.Aliases = []AliasSpec{}
	}
	if catalogue.OmittedTools == nil {
		catalogue.OmittedTools = []string{}
	}
	catalogue.Digest = catalogueDigest(catalogue)
	return catalogue
}

// catalogueDigest hashes the vocabulary a host pins: the profile identity, the
// effective tools, the alias table and the omitted names. The policy is
// reflected through Tools and OmittedTools.
func catalogueDigest(catalogue Catalogue) string {
	payload := struct {
		Profile      Profile     `json:"profile"`
		Tools        []ToolSpec  `json:"tools"`
		Aliases      []AliasSpec `json:"aliases"`
		OmittedTools []string    `json:"omitted_tools"`
	}{catalogue.Profile, catalogue.Tools, catalogue.Aliases, catalogue.OmittedTools}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// capabilities assembles the payload from the effective tool list.
func (s *Server) capabilities() Capabilities {
	summary := ToolSummary{Total: len(s.tools), Names: make([]string, 0, len(s.tools))}
	grades := map[string]int{GradeExact: 0, GradeEnvelope: 0, GradeApproximate: 0, GradeUnsupported: 0}
	for _, t := range s.tools {
		summary.Names = append(summary.Names, t.Name)
		switch {
		case t.Legacy:
			summary.Aliased++
		case t.Stub:
			summary.Stubs++
		default:
			summary.Native++
		}
		if t.Grade != "" && t.Grade != GradeStub {
			grades[t.Grade]++
		}
		switch toolPolicy(t) {
		case "read":
			summary.ReadOnly++
		case "destructive":
			summary.Destructive++
		default:
			summary.AppendOnly++
		}
	}
	summary.Grades = grades
	return Capabilities{
		Profile:        s.profile.Name,
		ProfileVersion: s.profile.Version,
		Engine:         s.version,
		Policy:         s.policy.Info(),
		Tools:          summary,
		Features:       s.featureFlags(),
		Limits: map[string]int{
			"body_bytes":              1 << 20,
			"search_page":             searchMaxPageSize,
			"search_window":           searchWindow,
			"record_page":             recordsMaxPageSize,
			"relation_page":           relationsMaxPageSize,
			"journal_page":            journalMaxPageSize,
			"context_tokens":          32000,
			"expand_tokens":           ExpandMaxTokens,
			"judgments_per_call":      maxJudgeMany,
			"prompt_page":             promptMaxPageSize,
			"resource_bytes":          resourceRecordMaxBytes,
			"resource_state_bytes":    resourceStateMaxBytes,
			"resource_projects_bytes": resourceProjectsMaxBytes,
			"resource_review_bytes":   resourceReviewMaxBytes,
			"resource_symbol_bytes":   resourceSymbolMaxBytes,
			"graph_search":            ports.MaxGraphSearchLimit,
			"graph_impact_depth":      ports.MaxGraphImpactDepth,
			"graph_explore_files":     ports.MaxGraphExploreFiles,
			"graph_explore_bytes":     ports.MaxGraphExploreBytes,
			"graph_neighbors":         ports.MaxGraphSearchLimit,
		},
		Gaps:         s.profileGaps(),
		RefusalCodes: RefusalCodes(),
		Endpoints: map[string]bool{
			"stdio": true, "http": false, "sse": false, "resources": true, "prompts": false,
		},
		Schemas: map[string]string{"record": "knowledge.v1", "refusal": "refusal.v1"},
	}
}

// featureFlags reports what the profile can do, including the approximations
// of a migration vocabulary. A flag is never a promise the backend cannot
// keep: code_graph is true only where the profile serves brain_code, and the
// analytics half of the knowledge graph is reported separately because five of
// its six actions answer UNSUPPORTED in this release.
func (s *Server) featureFlags() map[string]any {
	features := map[string]any{
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
		"resources":       true,
		"prompts":         false,
	}
	switch s.profile.Name {
	case ProfileLV24Legacy:
		features["deprecated"] = profileDeprecation
	case ProfileEngram:
		features["pinning"] = "approximate"
		features["sessions"] = "synthetic"
		features["consolidation"] = "refused"
	}
	switch s.profile.Name {
	case ProfileLV24, ProfileLV24Legacy, ProfileCodegraph:
		features["code_graph"] = true
		features["code_graph_analytics"] = false
	case ProfileGraphify:
		features["code_graph"] = true
		features["knowledge_graph"] = "partial"
		features["code_graph_analytics"] = false
	}
	if s.servesGraphTool("brain_graph") {
		features["knowledge_graph"] = "partial"
		features["knowledge_graph_analytics"] = false
	}
	return features
}

// servesGraphTool reports whether the effective profile lists the named tool.
func (s *Server) servesGraphTool(name string) bool {
	for _, t := range s.tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// capabilityGaps lists the native actions this release accepts but cannot
// serve, with the CLI path that can. A gap is never a silent no-op.
func capabilityGaps() []CapabilityGap {
	return []CapabilityGap{
		{Tool: "brain_record", Action: "list due_review", Reason: "the due queue is a review action", Remediation: "brain_review(action=due)"},
		{Tool: "brain_search", Action: "include_prompts", Reason: "the prompt zone is local-only and never enters the search index", Remediation: "brain_prompt(action=search)"},
		{Tool: "brain_prompt", Action: "discard", Reason: "no app method deletes a prompt in 0.17", Remediation: "edit projects/<id>/prompts/<date>.md by hand"},
		{Tool: "brain_projects", Action: "merge", Reason: "no app method merges projects in 0.17", Remediation: "brain project set <id> --alias <old-id> (renames) or brain import --from engram --map-project <from>=<to>"},
		{Tool: "brain_judge", Action: "pin", Reason: "pinning needs additive frontmatter and ranking weight (0.18)", Remediation: "brain note \"<why this record matters>\""},
		{Tool: "brain_judge", Action: "unpin", Reason: "pinning needs additive frontmatter and ranking weight (0.18)", Remediation: "brain note \"<why this record matters>\""},
		{Tool: "brain_judge", Action: "consolidate", Reason: "two-phase consolidation is not exposed over MCP in 0.17", Remediation: "brain record supersede <old-id> --by <new-id>"},
		graphUnsupported["path"],
		graphUnsupported["communities"],
		graphUnsupported["god_nodes"],
		graphUnsupported["pr_impact"],
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
