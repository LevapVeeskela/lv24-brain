package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// fullFake implements every optional backend interface, so tools/list reports
// all 16 tools.
type fullFake struct {
	stateFake
	records       []RecordSummary
	hits          []Hit
	recordsQuery  RecordQuery
	relations     []RelationRow
	judged        []JudgeInput
	conflicts     bool
	notFound      bool
	supersedes    []string
	statusChanges []string
	catchup       Catchup
	review        Review
}

// ListRelations implements RelationBackend.
func (f *fullFake) ListRelations(_ context.Context, query RelationQuery) ([]RelationRow, error) {
	out := f.relations
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

// JudgeRelation implements RelationBackend.
func (f *fullFake) JudgeRelation(_ context.Context, in JudgeInput) (JudgedRelation, error) {
	f.judged = append(f.judged, in)
	return JudgedRelation{
		Changed: true,
		Relation: RelationRow{
			SourceKind: "decision", SourceID: in.From, SourceTitle: "Use fsstore",
			TargetID: in.To, TargetState: "found", Relation: in.Verdict, Status: "accepted",
			Reason: in.Reason, Confidence: in.Confidence, VerdictBy: in.By,
		},
	}, nil
}

// Search honours the page window the tool asks for, unlike the fixed fake.
func (f *fullFake) Search(_ context.Context, in SearchInput) ([]Hit, error) {
	f.search = in
	out := f.hits
	if in.Limit > 0 && len(out) > in.Limit {
		out = out[:in.Limit]
	}
	return out, nil
}

func (f *fullFake) ListRecords(_ context.Context, query RecordQuery) ([]RecordSummary, error) {
	f.recordsQuery = query
	out := f.records
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (f *fullFake) GetRecord(_ context.Context, projectID, id string) (RecordDetail, error) {
	if f.notFound {
		return RecordDetail{}, fmt.Errorf("record %q: %w", id, domain.ErrNotFound)
	}
	return RecordDetail{Kind: "decision", ID: id, ProjectID: projectID, Title: "Use fsstore", Status: "proposed", Links: []RecordLink{}, Backlinks: []RecordBacklink{}}, nil
}

func (f *fullFake) SetDecisionStatus(_ context.Context, projectID, id, status string) (RecordSummary, error) {
	if f.conflicts {
		return RecordSummary{}, fmt.Errorf("decision %q: %w", id, domain.ErrConflict)
	}
	f.statusChanges = append(f.statusChanges, id+"="+status)
	return RecordSummary{Kind: "decision", ID: id, ProjectID: projectID, Title: "Use fsstore", Status: status}, nil
}

func (f *fullFake) Supersede(_ context.Context, projectID, oldID, newID string) (RecordSummary, error) {
	f.supersedes = append(f.supersedes, oldID+"->"+newID)
	return RecordSummary{Kind: "decision", ID: oldID, ProjectID: projectID, Title: "Use fsstore", Status: "superseded"}, nil
}

func (f *fullFake) Catchup(_ context.Context, projectID string, _ time.Time, _ int) (Catchup, error) {
	result := f.catchup
	result.ProjectID = projectID
	if result.Notes == nil {
		result.Notes = []CatchupItem{}
	}
	if result.Decisions == nil {
		result.Decisions = []CatchupItem{}
	}
	if result.Attempts == nil {
		result.Attempts = []CatchupItem{}
	}
	if result.Handoffs == nil {
		result.Handoffs = []CatchupItem{}
	}
	if result.Journal == nil {
		result.Journal = []string{}
	}
	return result, nil
}

func (f *fullFake) Review(_ context.Context, projectID string) (Review, error) {
	result := f.review
	result.ProjectID = projectID
	if result.Summary == "" {
		result.Summary = "# " + projectID + " — review"
	}
	if result.Findings == nil {
		result.Findings = []ReviewFinding{}
	}
	return result, nil
}

// modernMeta is the _meta envelope a 2026 client sends on every request.
const modernMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// elicitationMeta declares the elicitation capability, which MRTR prompts need.
const elicitationMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"elicitation":{"form":{}}}}`

// requestMessage builds one JSON-RPC request; params must be a JSON object.
func requestMessage(id int, method, params string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
}

// modernRequest adds the 2026 envelope to a params object.
func modernRequest(id int, method, params, meta string) string {
	if params == "" {
		return requestMessage(id, method, "{"+meta+"}")
	}
	return requestMessage(id, method, "{"+params+","+meta+"}")
}

// modernCall is one tools/call under the 2026 revision.
func modernCall(id int, name, args, meta string) string {
	return modernRequest(id, "tools/call", fmt.Sprintf(`"name":%q,"arguments":%s`, name, args), meta)
}

// resultOf returns the result object of a response, failing on a JSON-RPC error.
func resultOf(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	if errObj, ok := response["error"]; ok {
		t.Fatalf("unexpected JSON-RPC error: %v", errObj)
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing: %v", response)
	}
	return result
}

func toolNames(t *testing.T, list map[string]any) []string {
	t.Helper()
	raw, ok := list["tools"].([]any)
	if !ok {
		t.Fatalf("tools missing: %v", list)
	}
	names := make([]string, 0, len(raw))
	for _, entry := range raw {
		names = append(names, entry.(map[string]any)["name"].(string))
	}
	return names
}

func TestProtocolDiscoverWithoutInitialize(t *testing.T) {
	responses := session(t, &fullFake{},
		modernRequest(1, "server/discover", "", modernMeta),
	)
	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}
	result := resultOf(t, responses[0])
	if result["resultType"] != resultComplete {
		t.Fatalf("resultType = %v, want %s", result["resultType"], resultComplete)
	}
	versions := result["supportedVersions"].([]any)
	if len(versions) == 0 || versions[0] != ProtocolVersion2026 {
		t.Fatalf("supportedVersions = %v, want %s first", versions, ProtocolVersion2026)
	}
	found := false
	for _, version := range versions {
		if version == ProtocolVersion {
			found = true
		}
	}
	if !found {
		t.Fatalf("supportedVersions = %v, want the legacy revision too", versions)
	}
	caps := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"].(map[string]any); !ok {
		t.Fatalf("capabilities.tools = %v", caps["tools"])
	}
	meta := result["_meta"].(map[string]any)
	info, ok := meta[metaServerInfo].(map[string]any)
	if !ok || info["name"] != ServerName || info["version"] != "test-version" {
		t.Fatalf("_meta.%s = %v", metaServerInfo, meta[metaServerInfo])
	}
	if result["ttlMs"] != float64(discoverTTLMs) || result["cacheScope"] != cacheScopePublic {
		t.Fatalf("cache hints = %v/%v", result["ttlMs"], result["cacheScope"])
	}
	instructions, _ := result["instructions"].(string)
	if !strings.Contains(instructions, "brain_open") {
		t.Fatalf("instructions = %q", instructions)
	}
}

func TestProtocolDiscoverNeedsNoMeta(t *testing.T) {
	// A dual-era client probes with server/discover before it knows the era; the
	// answer is always the 2026 shape.
	result := resultOf(t, session(t, &fullFake{}, requestMessage(1, "server/discover", `{}`))[0])
	if result["resultType"] != resultComplete {
		t.Fatalf("resultType = %v", result["resultType"])
	}
	if _, ok := result["supportedVersions"].([]any); !ok {
		t.Fatalf("supportedVersions missing: %v", result)
	}
}

func TestProtocolModernToolsListNeedsNoHandshake(t *testing.T) {
	responses := session(t, &fullFake{}, modernRequest(1, "tools/list", "", modernMeta))
	result := resultOf(t, responses[0])
	if result["resultType"] != resultComplete {
		t.Fatalf("resultType = %v", result["resultType"])
	}
	if result["ttlMs"] != float64(toolsListTTLMs) || result["cacheScope"] != cacheScopePublic {
		t.Fatalf("cache hints = %v/%v", result["ttlMs"], result["cacheScope"])
	}
	if meta, ok := result["_meta"].(map[string]any); !ok || meta[metaServerInfo] == nil {
		t.Fatalf("_meta = %v", result["_meta"])
	}
	if got := len(toolNames(t, result)); got != 15 {
		t.Fatalf("tools = %d, want 15", got)
	}
}

func TestProtocolModernToolsCall(t *testing.T) {
	result := resultOf(t, session(t, &fullFake{}, modernCall(1, "brain_open", `{"project":"beta"}`, modernMeta))[0])
	if result["resultType"] != resultComplete {
		t.Fatalf("resultType = %v", result["resultType"])
	}
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	if obj := structured(t, result); obj["project_id"] != "beta" {
		t.Fatalf("structuredContent = %v", obj)
	}
	if meta, ok := result["_meta"].(map[string]any); !ok || meta[metaServerInfo] == nil {
		t.Fatalf("_meta = %v", result["_meta"])
	}
}

func TestProtocolLegacyHandshakeKeepsLegacyShape(t *testing.T) {
	responses := session(t, &fullFake{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"legacy","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"brain_open","arguments":{}}}`,
	)
	if len(responses) != 3 {
		t.Fatalf("responses = %d, want 3", len(responses))
	}
	initialize := resultOf(t, responses[0])
	if initialize["protocolVersion"] != ProtocolVersion {
		t.Fatalf("protocolVersion = %v", initialize["protocolVersion"])
	}
	list := resultOf(t, responses[1])
	if _, ok := list["resultType"]; ok {
		t.Fatalf("legacy tools/list carries resultType: %v", list["resultType"])
	}
	if _, ok := list["ttlMs"]; ok {
		t.Fatalf("legacy tools/list carries cache hints: %v", list["ttlMs"])
	}
	call := resultOf(t, responses[2])
	if _, ok := call["resultType"]; ok {
		t.Fatalf("legacy tools/call carries resultType: %v", call["resultType"])
	}
	if call["isError"] != false {
		t.Fatalf("isError = %v", call["isError"])
	}
}

func TestProtocolLegacyVersionInMetaKeepsLegacyShape(t *testing.T) {
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-06-18"}`
	result := resultOf(t, session(t, &fullFake{}, modernRequest(1, "tools/list", "", meta))[0])
	if _, ok := result["resultType"]; ok {
		t.Fatalf("resultType = %v, want the legacy shape", result["resultType"])
	}
}

func TestProtocolUnsupportedVersionIsRefused(t *testing.T) {
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2027-01-01"}`
	response := session(t, &fullFake{}, modernRequest(1, "tools/list", "", meta))[0]
	code, message := errorOf(t, response)
	if code != codeUnsupportedProtocolVersion {
		t.Fatalf("code = %d, want %d", code, codeUnsupportedProtocolVersion)
	}
	if message != "Unsupported protocol version" {
		t.Fatalf("message = %q", message)
	}
	data, ok := response["error"].(map[string]any)["data"].(map[string]any)
	if !ok {
		t.Fatalf("error data missing: %v", response["error"])
	}
	if data["requested"] != "2027-01-01" {
		t.Fatalf("requested = %v", data["requested"])
	}
	supported, _ := data["supported"].([]any)
	if len(supported) == 0 || supported[0] != ProtocolVersion2026 {
		t.Fatalf("supported = %v", data["supported"])
	}
}

func TestProtocolUnknownToolsListCursor(t *testing.T) {
	response := session(t, &fullFake{}, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":"not-ours"}}`)[0]
	code, _ := errorOf(t, response)
	if code != codeInvalidParams {
		t.Fatalf("code = %d, want %d", code, codeInvalidParams)
	}
}

// serveSession feeds messages to one existing server, so messages that must
// share process state (MRTR requestState) stay in the same process.
func serveSession(t *testing.T, server *Server, messages ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(messages, "\n") + "\n")
	if err := server.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var responses []map[string]any
	for _, line := range strings.Split(out.String(), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("stdout line is not JSON: %q: %v", line, err)
		}
		responses = append(responses, msg)
	}
	return responses
}

func TestProtocolMRTRRetryCompletesTheCall(t *testing.T) {
	backend := &fullFake{}
	server := New(backend, "test-version")
	prompt := resultOf(t, serveSession(t, server,
		modernCall(1, "brain_judge", `{"action":"supersede","id":"decision-alpha-1","to":"decision-alpha-2"}`, elicitationMeta),
	)[0])
	state := prompt["requestState"].(string)

	args := `{"action":"supersede","id":"decision-alpha-1","to":"decision-alpha-2"}`
	retry := modernRequest(2, "tools/call",
		fmt.Sprintf(`"name":"brain_judge","arguments":%s,"inputResponses":{"%s":{"action":"accept","content":{"confirm":true}}},"requestState":%q`, args, confirmationKey, state),
		elicitationMeta)
	result := resultOf(t, serveSession(t, server, retry)[0])
	if result["resultType"] != resultComplete {
		t.Fatalf("resultType = %v", result["resultType"])
	}
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	if len(backend.supersedes) != 1 || backend.supersedes[0] != "decision-alpha-1->decision-alpha-2" {
		t.Fatalf("supersedes = %v", backend.supersedes)
	}
}

func TestProtocolMRTRDeclineChangesNothing(t *testing.T) {
	backend := &fullFake{}
	server := New(backend, "test-version")
	prompt := resultOf(t, serveSession(t, server,
		modernCall(1, "brain_judge", `{"action":"supersede","id":"decision-alpha-1","to":"decision-alpha-2"}`, elicitationMeta),
	)[0])
	state := prompt["requestState"].(string)

	args := `{"action":"supersede","id":"decision-alpha-1","to":"decision-alpha-2"}`
	retry := modernRequest(2, "tools/call",
		fmt.Sprintf(`"name":"brain_judge","arguments":%s,"inputResponses":{"%s":{"action":"decline"}},"requestState":%q`, args, confirmationKey, state),
		elicitationMeta)
	result := resultOf(t, serveSession(t, server, retry)[0])
	if result["isError"] != true {
		t.Fatalf("declined confirmation is not an error: %v", result)
	}
	refusal := structured(t, result)
	if refusal["code"] != RefusalDeclined {
		t.Fatalf("code = %v, want %s", refusal["code"], RefusalDeclined)
	}
	if len(backend.supersedes) != 0 {
		t.Fatalf("supersedes = %v, want none", backend.supersedes)
	}
}

func TestProtocolMRTRTamperedStateIsRefused(t *testing.T) {
	backend := &fullFake{}
	args := `{"action":"supersede","id":"decision-alpha-1","to":"decision-alpha-2"}`
	retry := modernRequest(2, "tools/call",
		fmt.Sprintf(`"name":"brain_judge","arguments":%s,"inputResponses":{"%s":{"action":"accept","content":{"confirm":true}}},"requestState":"forged.state"`, args, confirmationKey),
		elicitationMeta)
	result := resultOf(t, session(t, backend, retry)[0])
	if result["isError"] != true {
		t.Fatalf("tampered state accepted: %v", result)
	}
	if code := structured(t, result)["code"]; code != RefusalInvalidArguments {
		t.Fatalf("code = %v, want %s", code, RefusalInvalidArguments)
	}
	if len(backend.supersedes) != 0 {
		t.Fatalf("supersedes = %v, want none", backend.supersedes)
	}
}

func TestProtocolMRTRWithoutElicitationRunsTheCall(t *testing.T) {
	// The client declared no elicitation capability, so the server must not ask:
	// the call keeps the pre-MRTR behaviour.
	backend := &fullFake{}
	result := resultOf(t, session(t, backend,
		modernCall(1, "brain_judge", `{"action":"supersede","id":"decision-alpha-1","to":"decision-alpha-2"}`, modernMeta),
	)[0])
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	if result["resultType"] != resultComplete {
		t.Fatalf("resultType = %v", result["resultType"])
	}
	if len(backend.supersedes) != 1 {
		t.Fatalf("supersedes = %v", backend.supersedes)
	}
}

func TestProtocolMRTRLegacyClientsAreNeverPrompted(t *testing.T) {
	backend := &fullFake{}
	result := toolCall(t, backend, "brain_judge", `{"action":"supersede","id":"decision-alpha-1","to":"decision-alpha-2"}`)
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	if len(backend.supersedes) != 1 {
		t.Fatalf("supersedes = %v", backend.supersedes)
	}
}
