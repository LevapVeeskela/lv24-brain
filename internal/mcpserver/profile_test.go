package mcpserver

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// newProfileServer builds a server for one profile and policy.
func newProfileServer(t *testing.T, backend Backend, name string, strict bool, policy Policy) *Server {
	t.Helper()
	profile, err := ResolveProfile(name, "", strict)
	if err != nil {
		t.Fatalf("ResolveProfile(%q): %v", name, err)
	}
	return NewWithProfile(backend, "test-version", profile, policy)
}

// profileCall runs one tools/call and returns its result object.
func profileCall(t *testing.T, server *Server, name, args string) map[string]any {
	t.Helper()
	responses := serveSession(t, server,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args))
	return resultOf(t, responses[0])
}

func profileToolNames(t *testing.T, server *Server) []string {
	t.Helper()
	return toolNames(t, resultOf(t, serveSession(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)[0]))
}

func TestProfilesExposeTheExactVocabulary(t *testing.T) {
	legacy := []string{
		"brain_state_get", "brain_state_set", "brain_journal", "brain_records",
		"brain_relations", "brain_review_record", "brain_decision_status",
	}
	engram := []string{
		"mem_save", "mem_search", "mem_search_prompts", "mem_context", "mem_get_observation",
		"mem_timeline", "mem_update", "mem_delete", "mem_save_prompt", "mem_pin", "mem_unpin",
		"mem_session_start", "mem_session_end", "mem_session_summary", "mem_session_finalize",
		"mem_capture_passive", "mem_review", "mem_suggest_topic_key", "mem_judge", "mem_judge_many",
		"mem_compare", "mem_consolidate", "mem_stats", "mem_doctor", "mem_current_project",
		"mem_list_projects", "mem_merge_projects", "brain_capabilities",
	}
	codegraph := []string{
		"codegraph_explore", "codegraph_search", "codegraph_node", "codegraph_callers",
		"codegraph_callees", "codegraph_impact", "codegraph_status", "codegraph_files",
		"brain_capabilities",
	}
	graphify := []string{
		"query_graph", "get_node", "get_neighbors", "graph_stats", "shortest_path",
		"get_community", "god_nodes", "list_prs", "get_pr_impact", "triage_prs",
		"brain_capabilities",
	}
	cases := []struct {
		profile string
		want    []string
	}{
		{ProfileLV24, nativeToolNames},
		{ProfileLV24Legacy, append(append([]string{}, nativeToolNames...), legacy...)},
		{ProfileEngram, engram},
		{ProfileCodegraph, codegraph},
		{ProfileGraphify, graphify},
	}
	for _, test := range cases {
		server := newProfileServer(t, newSurfaceFake(), test.profile, false, Policy{})
		got := profileToolNames(t, server)
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s tools = %v, want %v", test.profile, got, test.want)
		}
		seen := map[string]bool{}
		for _, name := range got {
			if seen[name] {
				t.Fatalf("%s lists %s twice", test.profile, name)
			}
			seen[name] = true
		}
	}
}

func TestMigrationProfilesNeverListNativeNames(t *testing.T) {
	for _, profile := range []string{ProfileEngram, ProfileCodegraph, ProfileGraphify} {
		server := newProfileServer(t, newSurfaceFake(), profile, false, Policy{})
		listed := map[string]bool{}
		for _, name := range profileToolNames(t, server) {
			listed[name] = true
		}
		for _, native := range nativeToolNames {
			if native == "brain_capabilities" {
				continue
			}
			if listed[native] {
				t.Fatalf("%s lists native %s next to the legacy vocabulary", profile, native)
			}
		}
		if !listed["brain_capabilities"] {
			t.Fatalf("%s does not serve brain_capabilities", profile)
		}
	}
}

func TestResolveProfileFailsClosed(t *testing.T) {
	for _, name := range []string{"engram,admin", "bogus", "lv25"} {
		if _, err := ResolveProfile(name, "", false); err == nil || !strings.Contains(err.Error(), "unknown profile") {
			t.Fatalf("%q: err = %v", name, err)
		}
	}
	for _, version := range []string{"2", "1.1", "latest"} {
		if _, err := ResolveProfile(ProfileEngram, version, false); err == nil || !strings.Contains(err.Error(), "unsupported profile version") {
			t.Fatalf("version %q: err = %v", version, err)
		}
	}
	for _, version := range []string{"", "1", "1.0"} {
		profile, err := ResolveProfile("engram", version, true)
		if err != nil || profile.Name != ProfileEngram || profile.Version != ProfileVersion || !profile.Strict || profile.Native {
			t.Fatalf("version %q: profile = %+v, err = %v", version, profile, err)
		}
	}
	bridge, err := ResolveProfile(ProfileLV24Legacy, "", false)
	if err != nil || !bridge.Deprecated || bridge.Native {
		t.Fatalf("bridge profile = %+v, err = %v", bridge, err)
	}
	native, err := ResolveProfile("", "", false)
	if err != nil || native.Name != ProfileLV24 || !native.Native {
		t.Fatalf("default profile = %+v, err = %v", native, err)
	}
}

func TestEngramGradesAndTargets(t *testing.T) {
	profile, _ := ResolveProfile(ProfileEngram, "", false)
	aliases := Aliases(profile)
	if len(aliases) != 27 {
		t.Fatalf("aliases = %d, want 27", len(aliases))
	}
	counts := map[string]int{}
	native := map[string]bool{}
	for _, spec := range Catalog("test-version", DefaultProfile()) {
		native[spec.Name] = true
	}
	for _, alias := range aliases {
		counts[alias.Grade]++
		if alias.Grade == GradeUnsupported {
			if alias.Target != "" || alias.Remediation == "" {
				t.Fatalf("%s: target = %q, remediation = %q", alias.Legacy, alias.Target, alias.Remediation)
			}
			continue
		}
		if alias.Target == "" {
			// A synthetic alias (mem_session_start) has no native target.
			if alias.Grade != GradeApproximate || alias.Note == "" {
				t.Fatalf("%s has no target and no explanation", alias.Legacy)
			}
			continue
		}
		if !native[alias.Target] {
			t.Fatalf("%s targets unknown tool %q", alias.Legacy, alias.Target)
		}
	}
	want := map[string]int{"A": 3, "B": 15, "C": 7, "D": 2}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("grades = %v, want %v", counts, want)
	}
}

func TestStrictRefusesEveryApproximateAndUnsupportedAlias(t *testing.T) {
	server := newProfileServer(t, newSurfaceFake(), ProfileEngram, true, Policy{})
	approx := map[string]string{
		"mem_timeline":        `{"record_id":"decision-alpha-1"}`,
		"mem_update":          `{"content":"new body","topic_key":"topic"}`,
		"mem_pin":             `{"id":"decision-alpha-1"}`,
		"mem_unpin":           `{"id":"decision-alpha-1"}`,
		"mem_session_start":   `{}`,
		"mem_session_end":     `{}`,
		"mem_capture_passive": `{"content":"raw text"}`,
	}
	for name, args := range approx {
		result := profileCall(t, server, name, args)
		if result["isError"] != true {
			t.Fatalf("%s succeeded under --strict: %v", name, result)
		}
		if code := structured(t, result)["code"]; code != RefusalUnsupported {
			t.Fatalf("%s code = %v, want %s", name, code, RefusalUnsupported)
		}
	}
	for _, name := range []string{"mem_delete", "mem_merge_projects"} {
		result := profileCall(t, server, name, `{}`)
		refusal := structured(t, result)
		if result["isError"] != true || refusal["code"] != RefusalUnsupported {
			t.Fatalf("%s = %v", name, result)
		}
		if tryInstead, _ := refusal["try_instead"].(string); tryInstead == "" {
			t.Fatalf("%s has no remediation: %v", name, refusal)
		}
	}
	deleteResult := structured(t, profileCall(t, server, "mem_delete", `{"id":"note-1"}`))
	if !strings.Contains(deleteResult["try_instead"].(string), "brain record trash") {
		t.Fatalf("mem_delete remediation = %v", deleteResult["try_instead"])
	}
	mergeResult := structured(t, profileCall(t, server, "mem_merge_projects", `{"from":"a","to":"b"}`))
	if !strings.Contains(mergeResult["try_instead"].(string), "--map-project") {
		t.Fatalf("mem_merge_projects remediation = %v", mergeResult["try_instead"])
	}
}

func TestLegacyBridgeCallsTheNativeAction(t *testing.T) {
	cases := []struct {
		legacy toolCase
		native toolCase
	}{
		{toolCase{"brain_state_get", `{"project":"alpha"}`}, toolCase{"brain_state", `{"action":"get","project":"alpha"}`}},
		{toolCase{"brain_state_set", `{"goal":"new goal"}`}, toolCase{"brain_state", `{"action":"set","goal":"new goal"}`}},
		{toolCase{"brain_journal", `{"limit":1}`}, toolCase{"brain_catchup", `{"mode":"journal","limit":1}`}},
		{toolCase{"brain_records", `{}`}, toolCase{"brain_record", `{"action":"list"}`}},
		{toolCase{"brain_relations", `{}`}, toolCase{"brain_record", `{"action":"relations"}`}},
		{toolCase{"brain_review_record", `{"all":true}`}, toolCase{"brain_judge", `{"action":"review","ids":["*"]}`}},
		{toolCase{"brain_decision_status", `{"id":"decision-alpha-1","status":"accepted"}`}, toolCase{"brain_judge", `{"action":"accept","id":"decision-alpha-1"}`}},
	}
	for _, test := range cases {
		bridge := newProfileServer(t, newSurfaceFake(), ProfileLV24Legacy, false, Policy{})
		legacyResult := profileCall(t, bridge, test.legacy.name, test.legacy.args)
		if legacyResult["isError"] != false {
			t.Fatalf("%s failed: %v", test.legacy.name, legacyResult)
		}
		nativeResult := profileCall(t, newProfileServer(t, newSurfaceFake(), ProfileLV24, false, Policy{}), test.native.name, test.native.args)
		if nativeResult["isError"] != false {
			t.Fatalf("%s failed: %v", test.native.name, nativeResult)
		}
		if !reflect.DeepEqual(legacyResult["structuredContent"], nativeResult["structuredContent"]) {
			t.Fatalf("%s = %v, want %v", test.legacy.name, legacyResult["structuredContent"], nativeResult["structuredContent"])
		}
		compat, ok := legacyResult["_meta"].(map[string]any)["lv24.brain/compat"].(map[string]any)
		if !ok || compat["legacy"] != test.legacy.name || compat["deprecated"] != true {
			t.Fatalf("%s compat = %v", test.legacy.name, legacyResult["_meta"])
		}
	}
}

type toolCase struct {
	name string
	args string
}

func TestEngramAliasesTranslateArguments(t *testing.T) {
	backend := newSurfaceFake()
	backend.hits = []Hit{{ID: "note-alpha-1", ProjectID: "alpha", Kind: "note", Title: "Cache fix", Snippet: "cache fixed"}}
	server := newProfileServer(t, backend, ProfileEngram, false, Policy{})

	if result := profileCall(t, server, "mem_save", `{"content":"Use SQLite","type":"decision","title":"Use SQLite"}`); result["isError"] != false {
		t.Fatalf("mem_save decision = %v", result)
	}
	if len(backend.decisions) != 1 || backend.decisions[0].Title != "Use SQLite" || backend.decisions[0].Context != "Use SQLite" {
		t.Fatalf("decisions = %+v", backend.decisions)
	}
	if result := profileCall(t, server, "mem_save", `{"observation":"cache fixed","type":"discovery","topic_key":"cache"}`); result["isError"] != false {
		t.Fatalf("mem_save note = %v", result)
	}
	if len(backend.notes) != 1 || backend.notes[0].Body != "cache fixed" || backend.notes[0].Subtype != "discovery" || backend.notes[0].TopicKey != "cache" {
		t.Fatalf("notes = %+v", backend.notes)
	}

	search := profileCall(t, server, "mem_search", `{"query":"cache","match_mode":"all","response_format":"compact"}`)
	if search["isError"] != false {
		t.Fatalf("mem_search = %v", search)
	}
	if backend.search.MatchMode != "all" {
		t.Fatalf("match mode = %q", backend.search.MatchMode)
	}
	hit := structured(t, search)["hits"].([]any)[0].(map[string]any)
	if hit["type"] != "note" || hit["content"] == "" {
		t.Fatalf("legacy search fields = %v", hit)
	}

	if result := profileCall(t, server, "mem_get_observation", `{"id":"alpha/decision-alpha-1"}`); result["isError"] != false {
		t.Fatalf("mem_get_observation = %v", result)
	} else if got := structured(t, result); got["type"] != "decision" || got["content"] == "" {
		t.Fatalf("observation fields = %v", got)
	}
	numeric := profileCall(t, server, "mem_get_observation", `{"id":42}`)
	refusal := structured(t, numeric)
	if numeric["isError"] != true || refusal["code"] != RefusalNotFound || !strings.Contains(refusal["try_instead"].(string), "mem_search") {
		t.Fatalf("numeric id = %v", numeric)
	}

	if result := profileCall(t, server, "mem_judge", `{"from":"a","to":"b","verdict":"related","reason":"same topic"}`); result["isError"] != false {
		t.Fatalf("mem_judge = %v", result)
	}
	if len(backend.judged) != 1 || backend.judged[0].Verdict != "relates_to" {
		t.Fatalf("judged = %+v", backend.judged)
	}
	if result := profileCall(t, server, "mem_judge", `{"judgment_id":"c->d","verdict":"conflict","reason":"different"}`); result["isError"] != false {
		t.Fatalf("mem_judge judgment_id = %v", result)
	}
	if backend.judged[1].From != "c" || backend.judged[1].To != "d" || backend.judged[1].Verdict != "conflicts_with" {
		t.Fatalf("judged pair = %+v", backend.judged[1])
	}

	if result := profileCall(t, server, "mem_review", `{"action":"mark_reviewed","observation_id":"decision-alpha-1","trust":"stated"}`); result["isError"] != false {
		t.Fatalf("mem_review = %v", result)
	}
	if len(backend.reviews) != 1 || backend.reviews[0].ID != "decision-alpha-1" || backend.reviews[0].Trust != "stated" {
		t.Fatalf("reviews = %+v", backend.reviews)
	}

	if result := profileCall(t, server, "mem_current_project", `{"cwd":"Alpha-alias"}`); result["isError"] != false {
		t.Fatalf("mem_current_project = %v", result)
	} else if got := structured(t, result)["current"].(map[string]any); got["id"] != "alpha" {
		t.Fatalf("current = %v", got)
	}
	if result := profileCall(t, server, "mem_list_projects", `{}`); result["isError"] != false {
		t.Fatalf("mem_list_projects = %v", result)
	}
}

func TestEngramDroppedArgumentsAreReportedAndStrictRefuses(t *testing.T) {
	backend := newSurfaceFake()
	server := newProfileServer(t, backend, ProfileEngram, false, Policy{})
	result := profileCall(t, server, "mem_save", `{"content":"x","session_id":"s1","org":"acme"}`)
	if result["isError"] != false {
		t.Fatalf("mem_save = %v", result)
	}
	compat, ok := result["_meta"].(map[string]any)["lv24.brain/compat"].(map[string]any)
	if !ok {
		t.Fatalf("compat meta missing: %v", result["_meta"])
	}
	ignored, _ := compat["ignored_fields"].([]any)
	if len(ignored) != 2 || ignored[0] != "org" || ignored[1] != "session_id" {
		t.Fatalf("ignored fields = %v", ignored)
	}

	strictBackend := newSurfaceFake()
	strict := newProfileServer(t, strictBackend, ProfileEngram, true, Policy{})
	refused := profileCall(t, strict, "mem_save", `{"content":"x","session_id":"s1"}`)
	refusal := structured(t, refused)
	if refused["isError"] != true || refusal["code"] != RefusalInvalidArguments || !strings.Contains(refusal["message"].(string), "session_id") {
		t.Fatalf("strict mem_save = %v", refused)
	}
	if len(strictBackend.notes) != 0 {
		t.Fatal("strict refusal wrote a note")
	}
}

func TestCodegraphProfileIsHonestWithoutAGraph(t *testing.T) {
	server := newProfileServer(t, newSurfaceFake(), ProfileCodegraph, false, Policy{})
	for _, name := range []string{
		"codegraph_explore", "codegraph_search", "codegraph_node", "codegraph_callers",
		"codegraph_callees", "codegraph_impact", "codegraph_files", "codegraph_status",
	} {
		result := profileCall(t, server, name, `{"project":"alpha","symbol":"Run","query":"run"}`)
		refusal := structured(t, result)
		if result["isError"] != true || refusal["code"] != RefusalUnsupported {
			t.Fatalf("%s = %v", name, result)
		}
		if tryInstead, _ := refusal["try_instead"].(string); tryInstead == "" {
			t.Fatalf("%s has no hint: %v", name, refusal)
		}
	}
	// Nothing is fabricated: no indexed:true, no zero count, no empty list.
	for _, name := range []string{"codegraph_status", "codegraph_files"} {
		if text := strings.ToLower(textOf(t, profileCall(t, server, name, `{}`))); strings.Contains(text, "indexed: true") {
			t.Fatalf("%s claims an index: %q", name, text)
		}
	}
}

func TestCodegraphProfilePublishesItsMapping(t *testing.T) {
	catalogue := BuildCatalogue("test-version", mustProfile(t, ProfileCodegraph), Policy{})
	if len(catalogue.Aliases) != 8 {
		t.Fatalf("aliases = %d, want 8", len(catalogue.Aliases))
	}
	want := map[string]struct {
		target string
		action string
		grade  string
	}{
		"codegraph_explore": {target: "brain_code", action: "explore", grade: GradeEnvelope},
		"codegraph_search":  {target: "brain_code", action: "search", grade: GradeEnvelope},
		"codegraph_node":    {target: "brain_code", action: "node", grade: GradeEnvelope},
		"codegraph_callers": {target: "brain_code", action: "node", grade: GradeApproximate},
		"codegraph_callees": {target: "brain_code", action: "node", grade: GradeApproximate},
		"codegraph_impact":  {target: "brain_code", action: "impact", grade: GradeEnvelope},
		"codegraph_status":  {target: "brain_code", action: "status", grade: GradeEnvelope},
		"codegraph_files":   {target: "brain_code", action: "status", grade: GradeApproximate},
	}
	for _, alias := range catalogue.Aliases {
		expect, ok := want[alias.Legacy]
		if !ok {
			t.Fatalf("unexpected alias %s", alias.Legacy)
		}
		if alias.Target != expect.target || alias.Action != expect.action || alias.Grade != expect.grade {
			t.Fatalf("%s = %+v, want %+v", alias.Legacy, alias, expect)
		}
	}
	if catalogue.Capabilities.Tools.Total != 9 || catalogue.Capabilities.Tools.Stubs != 0 {
		t.Fatalf("codegraph tools = %+v", catalogue.Capabilities.Tools)
	}
}

func TestGraphifyProfileIsHonestWithoutAGraph(t *testing.T) {
	server := newProfileServer(t, newSurfaceFake(), ProfileGraphify, false, Policy{})
	for _, name := range []string{
		"query_graph", "get_node", "get_neighbors", "get_community", "shortest_path",
		"god_nodes", "graph_stats", "list_prs", "get_pr_impact", "triage_prs",
	} {
		result := profileCall(t, server, name, `{"project":"alpha","question":"run","label":"Run","symbol":"Run"}`)
		refusal := structured(t, result)
		if result["isError"] != true || refusal["code"] != RefusalUnsupported {
			t.Fatalf("%s = %v", name, result)
		}
		if tryInstead, _ := refusal["try_instead"].(string); tryInstead == "" {
			t.Fatalf("%s has no hint: %v", name, refusal)
		}
	}
}

func TestGraphifyProfilePublishesItsMapping(t *testing.T) {
	catalogue := BuildCatalogue("test-version", mustProfile(t, ProfileGraphify), Policy{})
	if len(catalogue.Aliases) != 10 {
		t.Fatalf("aliases = %d, want 10", len(catalogue.Aliases))
	}
	want := map[string]struct {
		target string
		action string
		grade  string
	}{
		"query_graph":   {target: "brain_code", action: "search", grade: GradeApproximate},
		"get_node":      {target: "brain_code", action: "node", grade: GradeEnvelope},
		"get_neighbors": {target: "brain_graph", action: "neighbors", grade: GradeApproximate},
		"graph_stats":   {target: "brain_graph", action: "stats", grade: GradeEnvelope},
		"shortest_path": {target: "brain_graph", action: "path", grade: GradeUnsupported},
		"get_community": {target: "brain_graph", action: "communities", grade: GradeUnsupported},
		"god_nodes":     {target: "brain_graph", action: "god_nodes", grade: GradeUnsupported},
		"list_prs":      {grade: GradeUnsupported},
		"get_pr_impact": {target: "brain_graph", action: "pr_impact", grade: GradeUnsupported},
		"triage_prs":    {grade: GradeUnsupported},
	}
	for _, alias := range catalogue.Aliases {
		expect, ok := want[alias.Legacy]
		if !ok {
			t.Fatalf("unexpected alias %s", alias.Legacy)
		}
		if alias.Target != expect.target || alias.Action != expect.action || alias.Grade != expect.grade {
			t.Fatalf("%s = %+v, want %+v", alias.Legacy, alias, expect)
		}
		if alias.Grade == GradeUnsupported && alias.Remediation == "" {
			t.Fatalf("%s has no remediation", alias.Legacy)
		}
	}
}

func TestCapabilitiesReflectTheActiveProfile(t *testing.T) {
	cases := []struct {
		profile  string
		total    int
		aliased  int
		stubs    int
		wantGaps int
	}{
		{ProfileLV24, 17, 0, 0, 11},
		{ProfileLV24Legacy, 24, 7, 0, 11},
		{ProfileEngram, 28, 27, 0, 10},
		{ProfileCodegraph, 9, 8, 0, 3},
		{ProfileGraphify, 11, 10, 0, 8},
	}
	for _, test := range cases {
		payload := CapabilityReport("test-version", mustProfile(t, test.profile), Policy{})
		if payload.Tools.Total != test.total || payload.Tools.Aliased != test.aliased || payload.Tools.Stubs != test.stubs {
			t.Fatalf("%s tools = %+v", test.profile, payload.Tools)
		}
		if len(payload.Gaps) != test.wantGaps {
			t.Fatalf("%s gaps = %d, want %d: %+v", test.profile, len(payload.Gaps), test.wantGaps, payload.Gaps)
		}
		if payload.Profile != test.profile || payload.ProfileVersion != ProfileVersion {
			t.Fatalf("%s identity = %+v", test.profile, payload)
		}
		wantCodeGraph := test.profile != ProfileEngram
		if payload.Features["code_graph"] != wantCodeGraph {
			t.Fatalf("%s code_graph = %v, want %v", test.profile, payload.Features["code_graph"], wantCodeGraph)
		}
		var wantKnowledge any = "partial"
		if test.profile != ProfileLV24 && test.profile != ProfileLV24Legacy && test.profile != ProfileGraphify {
			wantKnowledge = false
		}
		if payload.Features["knowledge_graph"] != wantKnowledge {
			t.Fatalf("%s knowledge_graph = %v, want %v", test.profile, payload.Features["knowledge_graph"], wantKnowledge)
		}
		if payload.Features["resources"] != true {
			t.Fatalf("%s does not advertise resources", test.profile)
		}
	}
	engram := CapabilityReport("test-version", mustProfile(t, ProfileEngram), Policy{})
	if engram.Tools.Grades["A"] != 3 || engram.Tools.Grades["B"] != 15 ||
		engram.Tools.Grades["C"] != 7 || engram.Tools.Grades["D"] != 2 {
		t.Fatalf("engram grades = %v", engram.Tools.Grades)
	}
	found := false
	for _, gap := range engram.Gaps {
		if gap.Tool == "mem_delete" && gap.Grade == GradeUnsupported && strings.Contains(gap.Remediation, "brain record trash") {
			found = true
		}
	}
	if !found {
		t.Fatalf("engram gaps = %+v", engram.Gaps)
	}
}

func mustProfile(t *testing.T, name string) Profile {
	t.Helper()
	profile, err := ResolveProfile(name, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestCatalogueDigestIsStable(t *testing.T) {
	engram := mustProfile(t, ProfileEngram)
	first := BuildCatalogue("test-version", engram, Policy{})
	second := BuildCatalogue("test-version", engram, Policy{})
	if first.Digest == "" || first.Digest != second.Digest {
		t.Fatalf("digest = %q / %q", first.Digest, second.Digest)
	}
	if other := BuildCatalogue("test-version", mustProfile(t, ProfileLV24), Policy{}); other.Digest == first.Digest {
		t.Fatal("different profiles share a digest")
	}
	readOnly := BuildCatalogue("test-version", engram, Policy{ReadOnly: true})
	if readOnly.Digest == first.Digest {
		t.Fatal("a policy change does not change the digest")
	}
	if len(readOnly.OmittedTools) == 0 {
		t.Fatal("a read-only engram catalogue omits no tool")
	}
}

func TestCapabilitiesAppearInInitializeAndDiscover(t *testing.T) {
	server := newProfileServer(t, newSurfaceFake(), ProfileEngram, false, Policy{})
	for _, response := range serveSession(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"server/discover","params":{}}`,
	) {
		result := resultOf(t, response)
		caps, ok := result["capabilities"].(map[string]any)
		if !ok {
			t.Fatalf("capabilities missing: %v", result)
		}
		resources, ok := caps["resources"].(map[string]any)
		if !ok || resources["subscribe"] != false {
			t.Fatalf("resources capability = %v", caps["resources"])
		}
		meta, _ := result["_meta"].(map[string]any)
		payload, ok := meta["lv24.brain/capabilities"].(map[string]any)
		if !ok || payload["profile"] != ProfileEngram || payload["profile_version"] != ProfileVersion {
			t.Fatalf("capabilities _meta = %v", result["_meta"])
		}
	}
}

func TestProfileSwitchIsReflectedPerInvocation(t *testing.T) {
	for _, profile := range SupportedProfiles {
		server := newProfileServer(t, newSurfaceFake(), profile, false, Policy{})
		payload := structured(t, profileCall(t, server, "brain_capabilities", `{}`))
		if payload["profile"] != profile {
			t.Fatalf("%s capabilities reports %v", profile, payload["profile"])
		}
		names, _ := payload["tools"].(map[string]any)["names"].([]any)
		listed := profileToolNames(t, server)
		if len(names) != len(listed) {
			t.Fatalf("%s names = %d, listed = %d", profile, len(names), len(listed))
		}
	}
}

func TestProfileCatalogueMatchesTheServedList(t *testing.T) {
	for _, profile := range SupportedProfiles {
		server := newProfileServer(t, newSurfaceFake(), profile, false, Policy{ReadOnly: true})
		visible, omitted := CatalogForPolicy("test-version", server.profile, Policy{ReadOnly: true})
		listed := profileToolNames(t, server)
		if len(visible) != len(listed) {
			t.Fatalf("%s catalogue = %d, served = %d", profile, len(visible), len(listed))
		}
		if len(omitted) == 0 && len(listed) != len(Catalog("test-version", server.profile)) {
			t.Fatalf("%s read-only policy changed nothing", profile)
		}
	}
}
