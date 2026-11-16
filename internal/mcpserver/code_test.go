package mcpserver

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// graphIndexFake is one canned code graph. It records the arguments of every
// call so a test can prove the action reached the port with the translated
// values, and it can fail every read with one error.
type graphIndexFake struct {
	status     ports.GraphStatus
	syncResult ports.GraphSyncResult
	hits       []ports.SymbolHit
	node       ports.NodeView
	explore    ports.ExploreView
	impact     ports.ImpactView
	err        error

	searchQuery  ports.GraphQuery
	nodeSymbol   string
	nodeFile     string
	nodeOpts     ports.NodeOpts
	exploreQuery string
	exploreFiles int
	impactSymbol string
	impactDepth  int
	closeCount   int
	indexCalls   int
	syncCalls    int
}

func (f *graphIndexFake) Status(context.Context, string) (ports.GraphStatus, error) {
	return f.status, f.err
}

func (f *graphIndexFake) Sync(context.Context, string, []string) (ports.GraphSyncResult, error) {
	return f.syncResult, f.err
}

func (f *graphIndexFake) Search(_ context.Context, query ports.GraphQuery) ([]ports.SymbolHit, error) {
	f.searchQuery = query
	return f.hits, f.err
}

func (f *graphIndexFake) Node(_ context.Context, _, symbol, file string, opts ports.NodeOpts) (ports.NodeView, error) {
	f.nodeSymbol, f.nodeFile, f.nodeOpts = symbol, file, opts
	return f.node, f.err
}

func (f *graphIndexFake) Explore(_ context.Context, _, query string, maxFiles int) (ports.ExploreView, error) {
	f.exploreQuery, f.exploreFiles = query, maxFiles
	return f.explore, f.err
}

func (f *graphIndexFake) Impact(_ context.Context, _, symbol, _ string, depth int) (ports.ImpactView, error) {
	f.impactSymbol, f.impactDepth = symbol, depth
	return f.impact, f.err
}

func (f *graphIndexFake) Close() error {
	f.closeCount++
	return nil
}

// graphWriterFake adds the writable half of the native store.
type graphWriterFake struct{ *graphIndexFake }

func (w graphWriterFake) Index(context.Context, string) (ports.GraphSyncResult, error) {
	w.indexCalls++
	return w.syncResult, w.err
}

func (w graphWriterFake) Sync(context.Context, string, []string) (ports.GraphSyncResult, error) {
	w.syncCalls++
	return w.syncResult, w.err
}

// graphFake is a brain backend that also serves the code graph.
type graphFake struct {
	*surfaceFake
	index     *graphIndexFake
	openErr   error
	writerErr error
	opens     []string
}

func newGraphFake() *graphFake {
	symbol := ports.GraphSymbol{
		ID: "function:abc", Kind: "function", Name: "Run", QualName: "main.Run",
		File: "main.go", Language: "go", StartLine: 10, EndLine: 20, Signature: "func Run() error",
	}
	hits := []ports.SymbolHit{{
		GraphSymbol: symbol, Score: 1, Match: ports.GraphMatchExact, Confidence: 1, Provenance: "indexed",
	}}
	return &graphFake{
		surfaceFake: newSurfaceFake(),
		index: &graphIndexFake{
			status: ports.GraphStatus{
				Engine: ports.GraphEngineNative, Project: "alpha", Root: "C:/work/alpha",
				DBPath: "C:/brain/index/graph/alpha-1234.db", SchemaVersion: 1, ExtractionVersion: 3,
				IndexState: "complete", IndexedAt: time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC),
				AgeSeconds: 120, SizeBytes: 1 << 20, Files: 42, Nodes: 900, Edges: 2000,
			},
			syncResult: ports.GraphSyncResult{Engine: ports.GraphEngineNative, Project: "alpha", Changed: 7},
			hits:       hits,
			node: ports.NodeView{
				Symbol: symbol,
				Source: []ports.SourceLine{{Number: 10, Text: "func Run() error {"}, {Number: 11, Text: "\treturn nil"}},
				Callers: []ports.GraphNeighbor{{
					GraphSymbol: ports.GraphSymbol{ID: "function:def", Kind: "function", Name: "main", File: "main.go", StartLine: 1, EndLine: 5},
					Edge:        "calls", Line: 4, Count: 2, Confidence: 1,
				}},
				Callees: []ports.GraphNeighbor{{
					GraphSymbol: ports.GraphSymbol{ID: "function:ghi", Kind: "function", Name: "Load", File: "load.go", StartLine: 7, EndLine: 9},
					Edge:        "calls", Line: 11, Confidence: 1,
				}},
			},
			explore: ports.ExploreView{
				Query: "run", Symbols: hits,
				Files:       []ports.ExploreFile{{Path: "main.go", Lines: []ports.SourceLine{{Number: 10, Text: "func Run() error {"}}}},
				BlastRadius: []ports.ImpactHit{{GraphSymbol: symbol, Distance: 1, Via: "calls", Test: true}},
				Bytes:       400,
			},
			impact: ports.ImpactView{
				Symbol: symbol, Depth: 2,
				Hits:  []ports.ImpactHit{{GraphSymbol: symbol, Distance: 1, Via: "calls", ViaFile: "main.go", ViaLine: 4, Test: true, Confidence: 1}},
				Files: []string{"main_test.go"}, Tests: []string{"main_test.go"},
			},
		},
	}
}

func (g *graphFake) OpenGraph(_ context.Context, projectID, engine string) (ports.GraphIndex, error) {
	g.opens = append(g.opens, projectID+"/"+engine)
	if g.openErr != nil {
		return nil, g.openErr
	}
	return g.index, nil
}

func (g *graphFake) OpenGraphWriter(_ context.Context, projectID, engine string) (GraphWriter, error) {
	g.opens = append(g.opens, projectID+"/"+engine)
	if g.writerErr != nil {
		return nil, g.writerErr
	}
	return graphWriterFake{g.index}, nil
}

// graphServer builds a native-profile server over one graph fake.
func graphServer(t *testing.T, policy Policy) (*graphFake, *Server) {
	t.Helper()
	backend := newGraphFake()
	return backend, newProfileServer(t, backend, ProfileLV24, false, policy)
}

func TestBrainCodeAnnotationsAndLimits(t *testing.T) {
	spec := findSpec(t, Catalog("test-version", DefaultProfile()), "brain_code")
	annotations := spec.Annotations
	if annotations.ReadOnlyHint || annotations.DestructiveHint || !annotations.IdempotentHint || annotations.OpenWorldHint {
		t.Fatalf("brain_code annotations = %+v", annotations)
	}
	if strings.Join(spec.ReadActions, ",") != "status,search,node,explore,impact" {
		t.Fatalf("read actions = %v", spec.ReadActions)
	}
	if strings.Join(spec.WriteActions, ",") != "index,sync" {
		t.Fatalf("write actions = %v", spec.WriteActions)
	}
	for _, key := range []string{"search_results", "impact_depth", "explore_files", "explore_bytes", "neighbors"} {
		if spec.Limits[key] <= 0 {
			t.Fatalf("limits.%s = %v", key, spec.Limits)
		}
	}
	props, _ := spec.InputSchema["properties"].(map[string]any)
	for _, key := range []string{"action", "project", "query", "symbol", "file", "engine", "depth", "limit", "max_bytes"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("inputSchema misses %s: %v", key, props)
		}
	}

	graph := findSpec(t, Catalog("test-version", DefaultProfile()), "brain_graph")
	if !graph.Annotations.ReadOnlyHint || graph.Annotations.DestructiveHint {
		t.Fatalf("brain_graph annotations = %+v", graph.Annotations)
	}
}

func TestBrainCodeRoutesEveryAction(t *testing.T) {
	backend, server := graphServer(t, Policy{})

	status := structured(t, profileCall(t, server, "brain_code", `{"project":"alpha"}`))
	if status["action"] != "status" || status["project"] != "alpha" {
		t.Fatalf("default action = %v", status)
	}
	envelope, _ := status["status"].(map[string]any)
	if envelope["files"] != float64(42) || envelope["nodes"] != float64(900) {
		t.Fatalf("status envelope = %v", envelope)
	}
	if !strings.Contains(textOf(t, profileCall(t, server, "brain_code", `{"project":"alpha"}`)), "files: 42") {
		t.Fatal("status text misses the counts")
	}
	profileCall(t, server, "brain_code", `{"action":"status","project":"alpha","engine":"codegraph"}`)
	if backend.opens[len(backend.opens)-1] != "alpha/codegraph" {
		t.Fatalf("engine override reached the backend as %q", backend.opens[len(backend.opens)-1])
	}

	index := structured(t, profileCall(t, server, "brain_code", `{"action":"index","project":"alpha"}`))
	if index["action"] != "index" || backend.index.indexCalls != 1 {
		t.Fatalf("index = %v (calls %d)", index, backend.index.indexCalls)
	}
	if sync := structured(t, profileCall(t, server, "brain_code", `{"action":"sync","project":"alpha"}`)); sync["action"] != "sync" || backend.index.syncCalls != 1 {
		t.Fatalf("sync = %v (calls %d)", sync, backend.index.syncCalls)
	}

	search := structured(t, profileCall(t, server, "brain_code", `{"action":"search","project":"alpha","query":"Run","file":"main.go","limit":5}`))
	searchBody, _ := search["search"].(map[string]any)
	if searchBody["count"] != float64(1) {
		t.Fatalf("search = %v", search)
	}
	if backend.index.searchQuery.Text != "Run" || backend.index.searchQuery.File != "main.go" || backend.index.searchQuery.Limit != 5 {
		t.Fatalf("search query = %+v", backend.index.searchQuery)
	}

	node := structured(t, profileCall(t, server, "brain_code", `{"action":"node","project":"alpha","symbol":"main.Run","file":"main.go","limit":7}`))
	if _, ok := node["node"].(map[string]any); !ok {
		t.Fatalf("node = %v", node)
	}
	if backend.index.nodeSymbol != "main.Run" || backend.index.nodeFile != "main.go" || backend.index.nodeOpts.Limit != 7 {
		t.Fatalf("node args = %q/%q/%+v", backend.index.nodeSymbol, backend.index.nodeFile, backend.index.nodeOpts)
	}
	if !backend.index.nodeOpts.IncludeSource || !backend.index.nodeOpts.IncludeCallers || !backend.index.nodeOpts.IncludeCallees {
		t.Fatalf("node does not include source and neighbours: %+v", backend.index.nodeOpts)
	}

	explore := structured(t, profileCall(t, server, "brain_code", `{"action":"explore","project":"alpha","query":"run","limit":3,"max_bytes":8192}`))
	if _, ok := explore["explore"].(map[string]any); !ok {
		t.Fatalf("explore = %v", explore)
	}
	if backend.index.exploreQuery != "run" || backend.index.exploreFiles != 3 {
		t.Fatalf("explore args = %q/%d", backend.index.exploreQuery, backend.index.exploreFiles)
	}

	impact := structured(t, profileCall(t, server, "brain_code", `{"action":"impact","project":"alpha","symbol":"Run","depth":4}`))
	if _, ok := impact["impact"].(map[string]any); !ok {
		t.Fatalf("impact = %v", impact)
	}
	if backend.index.impactSymbol != "Run" || backend.index.impactDepth != 4 {
		t.Fatalf("impact args = %q/%d", backend.index.impactSymbol, backend.index.impactDepth)
	}

	if backend.index.closeCount == 0 {
		t.Fatal("an opened index was never closed")
	}
}

func TestBrainCodeRefusesEngineConditions(t *testing.T) {
	cases := []struct {
		name    string
		openErr error
		want    string
		call    string
	}{
		{"off", ErrGraphOff, "off", `{"project":"alpha"}`},
		{"absent", ErrGraphAbsent, "no code graph index", `{"project":"alpha"}`},
		{"index-missing", ErrGraphIndexMissing, "missing, empty", `{"project":"alpha"}`},
		{"read-only-write", ErrGraphReadOnly, "read-only", `{"action":"index","project":"alpha"}`},
	}
	for _, test := range cases {
		backend := newGraphFake()
		backend.openErr = test.openErr
		backend.writerErr = test.openErr
		server := newProfileServer(t, backend, ProfileLV24, false, Policy{})
		result := profileCall(t, server, "brain_code", test.call)
		refusal := structured(t, result)
		if result["isError"] != true || refusal["code"] != RefusalUnsupported {
			t.Fatalf("%s = %v", test.name, result)
		}
		if !strings.Contains(refusal["message"].(string), test.want) {
			t.Fatalf("%s message = %v", test.name, refusal["message"])
		}
		if tryInstead, _ := refusal["try_instead"].(string); tryInstead == "" {
			t.Fatalf("%s has no remediation: %v", test.name, refusal)
		}
	}
}

func TestBrainCodeValidatesArguments(t *testing.T) {
	_, server := graphServer(t, Policy{})
	cases := []struct{ name, args, code string }{
		{"unknown-action", `{"action":"delete","project":"alpha"}`, RefusalInvalidArguments},
		{"unknown-engine", `{"project":"alpha","engine":"git"}`, RefusalInvalidArguments},
		{"missing-project", `{}`, RefusalInvalidArguments},
		{"missing-query", `{"action":"search","project":"alpha"}`, RefusalInvalidArguments},
		{"missing-symbol", `{"action":"node","project":"alpha"}`, RefusalInvalidArguments},
		{"missing-impact-symbol", `{"action":"impact","project":"alpha"}`, RefusalInvalidArguments},
		{"depth", `{"action":"impact","project":"alpha","symbol":"Run","depth":11}`, RefusalInvalidArguments},
		{"search-limit", `{"action":"search","project":"alpha","query":"Run","limit":101}`, RefusalInvalidArguments},
		{"node-limit", `{"action":"node","project":"alpha","symbol":"Run","limit":101}`, RefusalInvalidArguments},
		{"explore-limit", `{"action":"explore","project":"alpha","query":"run","limit":25}`, RefusalInvalidArguments},
		{"max-bytes", `{"action":"explore","project":"alpha","query":"run","max_bytes":100000}`, RefusalInvalidArguments},
		{"unknown-field", `{"project":"alpha","bogus":1}`, RefusalInvalidArguments},
	}
	for _, test := range cases {
		result := profileCall(t, server, "brain_code", test.args)
		refusal := structured(t, result)
		if result["isError"] != true || refusal["code"] != test.code {
			t.Fatalf("%s = %v", test.name, result)
		}
	}

	small := profileCall(t, server, "brain_code", `{"action":"explore","project":"alpha","query":"run","max_bytes":10}`)
	refusal := structured(t, small)
	if small["isError"] != true || refusal["code"] != RefusalUnsupported || !strings.Contains(refusal["message"].(string), "400 bytes") {
		t.Fatalf("small max_bytes = %v", small)
	}
}

func TestBrainCodeReadOnlyKeepsReadsAndRefusesWrites(t *testing.T) {
	backend, server := graphServer(t, Policy{ReadOnly: true})
	names := profileToolNames(t, server)
	if !slices.Contains(names, "brain_code") {
		t.Fatal("a read-only server dropped brain_code")
	}
	for _, action := range []string{"index", "sync"} {
		result := profileCall(t, server, "brain_code", `{"action":"`+action+`","project":"alpha"}`)
		if result["isError"] != true || structured(t, result)["code"] != RefusalUnsupported {
			t.Fatalf("%s = %v", action, result)
		}
	}
	if len(backend.opens) != 0 {
		t.Fatalf("a refused write opened the engine: %v", backend.opens)
	}
	if result := profileCall(t, server, "brain_code", `{"action":"status","project":"alpha"}`); result["isError"] != false {
		t.Fatalf("status = %v", result)
	}
}

func TestBrainGraphStatsAndNeighbors(t *testing.T) {
	backend, server := graphServer(t, Policy{})

	stats := structured(t, profileCall(t, server, "brain_graph", `{"project":"alpha"}`))
	if stats["action"] != "stats" {
		t.Fatalf("default action = %v", stats)
	}
	envelope, _ := stats["stats"].(map[string]any)
	if envelope["nodes"] != float64(900) || envelope["edges"] != float64(2000) {
		t.Fatalf("stats = %v", envelope)
	}
	if _, ok := envelope["communities"]; ok {
		t.Fatalf("stats invents communities: %v", envelope)
	}

	neighbors := structured(t, profileCall(t, server, "brain_graph", `{"action":"neighbors","project":"alpha","symbol":"Run","limit":4}`))
	view, _ := neighbors["neighbors"].(map[string]any)
	if view == nil {
		t.Fatalf("neighbors = %v", neighbors)
	}
	if backend.index.nodeOpts.IncludeSource {
		t.Fatal("neighbors read the source")
	}
	if !backend.index.nodeOpts.IncludeCallers || !backend.index.nodeOpts.IncludeCallees || backend.index.nodeOpts.Limit != 4 {
		t.Fatalf("neighbor opts = %+v", backend.index.nodeOpts)
	}
	text := textOf(t, profileCall(t, server, "brain_graph", `{"action":"neighbors","project":"alpha","symbol":"Run"}`))
	if !strings.Contains(text, "callers: 1") {
		t.Fatalf("neighbors text = %q", text)
	}
}

func TestBrainGraphAnalyticsAreHonestlyUnsupported(t *testing.T) {
	backend, server := graphServer(t, Policy{})
	for _, action := range []string{"path", "communities", "god_nodes", "pr_impact"} {
		result := profileCall(t, server, "brain_graph", `{"action":"`+action+`","project":"alpha","symbol":"Run","source":"a","target":"b"}`)
		refusal := structured(t, result)
		if result["isError"] != true || refusal["code"] != RefusalUnsupported {
			t.Fatalf("%s = %v", action, result)
		}
		if tryInstead, _ := refusal["try_instead"].(string); tryInstead == "" {
			t.Fatalf("%s has no alternative: %v", action, refusal)
		}
	}
	if len(backend.opens) != 0 {
		t.Fatalf("an analytics action opened the graph: %v", backend.opens)
	}
	payload := CapabilityReport("test-version", DefaultProfile(), Policy{})
	found := map[string]bool{}
	for _, gap := range payload.Gaps {
		if gap.Tool == "brain_graph" {
			found[gap.Action] = true
		}
	}
	for _, action := range []string{"path", "communities", "god_nodes", "pr_impact"} {
		if !found[action] {
			t.Fatalf("capabilities miss the %s gap: %+v", action, payload.Gaps)
		}
	}
}

func TestCodegraphAliasesCallBrainCode(t *testing.T) {
	backend := newGraphFake()
	server := newProfileServer(t, backend, ProfileCodegraph, false, Policy{})

	if result := profileCall(t, server, "codegraph_explore", `{"project":"alpha","query":"run","maxFiles":2}`); result["isError"] != false {
		t.Fatalf("explore = %v", result)
	}
	if backend.index.exploreQuery != "run" || backend.index.exploreFiles != 2 {
		t.Fatalf("explore args = %q/%d", backend.index.exploreQuery, backend.index.exploreFiles)
	}
	if result := profileCall(t, server, "codegraph_search", `{"project":"alpha","query":"Run","file":"main.go","limit":5}`); result["isError"] != false {
		t.Fatalf("search = %v", result)
	}
	if backend.index.searchQuery.Text != "Run" || backend.index.searchQuery.File != "main.go" || backend.index.searchQuery.Limit != 5 {
		t.Fatalf("search args = %+v", backend.index.searchQuery)
	}
	nodeResult := profileCall(t, server, "codegraph_node", `{"project":"alpha","symbol":"Run","includeCode":true}`)
	if nodeResult["isError"] != false {
		t.Fatalf("node = %v", nodeResult)
	}
	ignored, _ := nodeResult["_meta"].(map[string]any)["lv24.brain/compat"].(map[string]any)["ignored_fields"].([]any)
	if len(ignored) != 1 || ignored[0] != "includeCode" {
		t.Fatalf("dropped includeCode is not reported: %v", ignored)
	}
	callers := structured(t, profileCall(t, server, "codegraph_callers", `{"project":"alpha","symbol":"Run"}`))
	if list, _ := callers["callers"].([]any); len(list) != 1 {
		t.Fatalf("callers = %v", callers)
	}
	callees := structured(t, profileCall(t, server, "codegraph_callees", `{"project":"alpha","symbol":"Run"}`))
	if list, _ := callees["callees"].([]any); len(list) != 1 {
		t.Fatalf("callees = %v", callees)
	}
	if result := profileCall(t, server, "codegraph_impact", `{"project":"alpha","symbol":"Run","depth":3}`); result["isError"] != false {
		t.Fatalf("impact = %v", result)
	}
	if backend.index.impactDepth != 3 {
		t.Fatalf("impact depth = %d", backend.index.impactDepth)
	}
	status := structured(t, profileCall(t, server, "codegraph_status", `{"project":"alpha"}`))
	if status["indexed"] != true || status["engine"] != "codegraph" || status["files"] != float64(42) {
		t.Fatalf("status = %v", status)
	}
	files := structured(t, profileCall(t, server, "codegraph_files", `{"project":"alpha"}`))
	if files["file_count"] != float64(42) {
		t.Fatalf("files = %v", files)
	}
	if _, fabricated := files["files"]; fabricated {
		t.Fatalf("files returns a list it cannot serve: %v", files)
	}
	compat, _ := profileCall(t, server, "codegraph_search", `{"project":"alpha","query":"Run"}`)["_meta"].(map[string]any)["lv24.brain/compat"].(map[string]any)
	if compat["legacy"] != "codegraph_search" || compat["target"] != "brain_code" {
		t.Fatalf("compat = %v", compat)
	}

	strict := newProfileServer(t, newGraphFake(), ProfileCodegraph, true, Policy{})
	if result := profileCall(t, strict, "codegraph_files", `{"project":"alpha"}`); result["isError"] != true {
		t.Fatalf("strict grade C accepted: %v", result)
	}
	if result := profileCall(t, strict, "codegraph_search", `{"project":"alpha","query":"Run","kind":"function"}`); result["isError"] != true {
		t.Fatalf("strict dropped field accepted: %v", result)
	}
	if result := profileCall(t, strict, "codegraph_search", `{"project":"alpha","query":"Run"}`); result["isError"] != false {
		t.Fatalf("strict grade B refused: %v", result)
	}
}

func TestGraphifyAliasesCallTheGraphTools(t *testing.T) {
	backend := newGraphFake()
	server := newProfileServer(t, backend, ProfileGraphify, false, Policy{})

	if result := profileCall(t, server, "query_graph", `{"project":"alpha","question":"run the loader","limit":5}`); result["isError"] != false {
		t.Fatalf("query_graph = %v", result)
	}
	if backend.index.searchQuery.Text != "run the loader" || backend.index.searchQuery.Limit != 5 {
		t.Fatalf("query args = %+v", backend.index.searchQuery)
	}
	if result := profileCall(t, server, "get_node", `{"project":"alpha","label":"Run"}`); result["isError"] != false {
		t.Fatalf("get_node = %v", result)
	}
	dropped := profileCall(t, server, "get_node", `{"project":"alpha","label":"Run","depth":3}`)
	ignored, _ := dropped["_meta"].(map[string]any)["lv24.brain/compat"].(map[string]any)["ignored_fields"].([]any)
	if len(ignored) != 1 || ignored[0] != "depth" {
		t.Fatalf("dropped depth is not reported: %v", ignored)
	}
	if result := profileCall(t, server, "get_neighbors", `{"project":"alpha","node_id":"function:abc"}`); result["isError"] != false {
		t.Fatalf("get_neighbors = %v", result)
	}
	stats := structured(t, profileCall(t, server, "graph_stats", `{"project":"alpha"}`))
	if stats["indexed"] != true || stats["nodes"] != float64(900) || stats["communities_available"] != false {
		t.Fatalf("graph_stats = %v", stats)
	}
	for _, name := range []string{"shortest_path", "get_community", "god_nodes", "list_prs", "get_pr_impact", "triage_prs"} {
		result := profileCall(t, server, name, `{"project":"alpha","source":"a","target":"b"}`)
		if result["isError"] != true || structured(t, result)["code"] != RefusalUnsupported {
			t.Fatalf("%s = %v", name, result)
		}
	}
}

func TestCodeSymbolResourceServesTheGraph(t *testing.T) {
	backend, server := graphServer(t, Policy{Projects: []string{"alpha"}})
	templates := resultOf(t, serveSession(t, server, `{"jsonrpc":"2.0","id":1,"method":"resources/templates/list"}`)[0])["resourceTemplates"].([]any)
	found := false
	for _, raw := range templates {
		if raw.(map[string]any)["uriTemplate"] == ResourceSymbolTemplate {
			found = true
		}
	}
	if !found {
		t.Fatalf("code:// template is not advertised: %v", templates)
	}

	resource, refusal := resourceRead(t, server, "code://alpha/symbol/function:abc")
	if refusal != nil {
		t.Fatalf("code:// refusal = %v", refusal)
	}
	text := resource["contents"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "function Run") || !strings.Contains(text, "callers:") {
		t.Fatalf("code resource = %q", text)
	}
	if backend.index.nodeSymbol != "function:abc" {
		t.Fatalf("resource resolved %q", backend.index.nodeSymbol)
	}
	if resource["cacheScope"] != cacheScopePrivate {
		t.Fatalf("cacheScope = %v", resource["cacheScope"])
	}

	if _, refusal := resourceRead(t, server, "code://beta/symbol/function:abc"); refusal == nil || refusal["code"] != RefusalNotFound {
		t.Fatalf("project outside the allowlist = %v", refusal)
	}
	if _, refusal := resourceRead(t, server, "code://alpha/symbol"); refusal == nil || refusal["code"] != RefusalNotFound {
		t.Fatalf("malformed code uri = %v", refusal)
	}
}

func TestCodeResourceIsNotAdvertisedWithoutAGraphBackend(t *testing.T) {
	server := newProfileServer(t, newSurfaceFake(), ProfileLV24, false, Policy{})
	templates := resultOf(t, serveSession(t, server, `{"jsonrpc":"2.0","id":1,"method":"resources/templates/list"}`)[0])["resourceTemplates"].([]any)
	for _, raw := range templates {
		if raw.(map[string]any)["uriTemplate"] == ResourceSymbolTemplate {
			t.Fatal("code:// is advertised without a graph backend")
		}
	}
}
