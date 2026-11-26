// Package mcpserver implements the Model Context Protocol over
// newline-delimited JSON-RPC 2.0 messages on a byte stream, and exposes brain
// operations as MCP tools.
//
// Two revisions are served from the same process:
//
//   - 2026-07-28, the stateless revision. A request carries its revision and
//     capabilities in `_meta`, no initialize handshake is needed, every result
//     carries resultType and _meta.serverInfo, and server/discover advertises
//     the supported versions.
//   - 2025-06-18 and older, the handshake revision. A client that opens with
//     initialize gets the 2025 result shapes without resultType.
//
// Only protocol messages may be written to the output stream: diagnostics
// belong on stderr.
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
)

// serverInstructions is sent to clients during initialize and in the discovery
// result.
const serverInstructions = "Company Brain is the curated memory of this project. " +
	"Call brain_open at the start of a session to load the project's state, decisions, attempts and last handoff, " +
	"then brain_catchup and brain_search to find earlier work. " +
	"Record durable findings as they happen with brain_note, brain_decision and brain_attempt (a failed approach, with its reason). " +
	"Capture ideas only when the owner explicitly asks, with brain_idea, and never mark one done without confirmation. " +
	"Call brain_handoff at the end of the session with a summary and the next action; git context is captured automatically. " +
	"Never store secrets, tokens or personal data in the brain."

// JSON-RPC 2.0 error codes, plus the MCP error codes the server emits.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	// codeUnsupportedProtocolVersion is the MCP UnsupportedProtocolVersion
	// error: the request declared a revision this server does not serve.
	codeUnsupportedProtocolVersion = -32022
)

// Server answers MCP requests against a Backend.
type Server struct {
	backend Backend
	version string
	// profile is the tool vocabulary this server exposes.
	profile Profile
	// tools is the effective tool list: what tools/list reports and what
	// tools/call may run. allTools also holds the tools the policy omits, so a
	// call to a hidden write tool gets a typed refusal.
	tools    []tool
	allTools []tool
	// nativeTools and nativeByName are always the native 15-tool surface, even
	// in a migration profile: an alias translates its legacy arguments and
	// runs the native tool by name.
	nativeTools  []tool
	nativeByName map[string]tool
	policy       Policy
	// catalog makes toolList report every tool group, including the ones a
	// given backend would hide. It is set by Catalog only.
	catalog bool
	// mrtKey signs MRTR requestState.
	mrtKey []byte
}

// New builds an unrestricted Server. version is reported as serverInfo.version.
// It serves the native profile over every registered project, as releases
// before 0.15 did; use NewWithPolicy for a least-privilege profile.
func New(backend Backend, version string) *Server {
	return NewWithPolicy(backend, version, Policy{})
}

// NewWithPolicy builds a Server with a least-privilege policy and the default
// profile. The policy is applied when the server is built: a read-only server
// never lists a write tool, so a host cannot call one it was not offered.
func NewWithPolicy(backend Backend, version string, policy Policy) *Server {
	return NewWithProfile(backend, version, DefaultProfile(), policy)
}

// NewWithProfile builds a Server with an explicit profile and policy.
func NewWithProfile(backend Backend, version string, profile Profile, policy Policy) *Server {
	if profile.Name == "" {
		profile = DefaultProfile()
	}
	s := &Server{backend: backend, version: version, profile: profile, policy: policy, mrtKey: newMRTRKey()}
	s.buildTools()
	return s
}

// applyPolicy filters the tool list to the tools the policy exposes.
func (s *Server) applyPolicy(all []tool) []tool {
	if !s.policy.ReadOnly {
		return all
	}
	visible := make([]tool, 0, len(all))
	for _, t := range all {
		if s.policy.listsTool(t) {
			visible = append(visible, t)
		}
	}
	return visible
}

// request is one incoming JSON-RPC message. A missing id marks a notification.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Data carries the structured payload of a defined MCP error, such as the
	// supported versions of UnsupportedProtocolVersion.
	Data any `json:"data,omitempty"`
}

// response is one outgoing JSON-RPC message. Exactly one of Result and Error
// is set.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	ServerInfo      serverInfo   `json:"serverInfo"`
	Instructions    string       `json:"instructions"`
	// Meta carries the lv24.brain/capabilities payload, so a handshake client
	// can read the profile, grades and gaps without a second call.
	Meta map[string]any `json:"_meta,omitempty"`
}

type capabilities struct {
	Tools     toolsCapability     `json:"tools"`
	Resources resourcesCapability `json:"resources"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// resourcesCapability advertises the read-only memory:// resources. The server
// does not support subscriptions: a resource is read on demand.
type resourcesCapability struct {
	ListChanged bool `json:"listChanged"`
	Subscribe   bool `json:"subscribe"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// Serve reads newline-delimited JSON-RPC messages from r and writes responses
// to w, one per line, until r is exhausted or ctx is cancelled. Requests are
// handled in arrival order. A read or write error stops the loop.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	reader := bufio.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, readErr := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			if out := s.handleMessage(ctx, trimmed); out != nil {
				if _, err := w.Write(append(out, '\n')); err != nil {
					return err
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// handleMessage returns the encoded response for one message, or nil when the
// message is a notification.
func (s *Server) handleMessage(ctx context.Context, line []byte) []byte {
	if !json.Valid(line) {
		return encodeResponse(response{JSONRPC: "2.0", ID: nil, Error: &rpcError{Code: codeParseError, Message: "parse error: malformed JSON"}})
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return encodeResponse(response{JSONRPC: "2.0", ID: nil, Error: &rpcError{Code: codeInvalidRequest, Message: "invalid request: " + err.Error()}})
	}
	isNotification := len(req.ID) == 0
	if req.JSONRPC != "2.0" || req.Method == "" {
		if isNotification {
			return encodeResponse(response{JSONRPC: "2.0", ID: nil, Error: &rpcError{Code: codeInvalidRequest, Message: "invalid request: jsonrpc must be \"2.0\" and method is required"}})
		}
		return encodeResponse(response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: codeInvalidRequest, Message: "invalid request: jsonrpc must be \"2.0\" and method is required"}})
	}
	if isNotification {
		// notifications/initialized and other notifications need no reply, and
		// a notification must never trigger a tool call.
		return nil
	}
	env, rpcErr := requestEnvelope(req.Method, req.Params)
	if rpcErr != nil {
		return encodeResponse(response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr})
	}
	if req.Method == "server/discover" {
		// Discovery is the 2026 entry point: its result always has the 2026
		// shape, even when the probe itself declared no revision.
		env.revision = revisionModern
	}
	result, rpcErr := s.dispatch(ctx, env, req)
	if rpcErr != nil {
		return encodeResponse(response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr})
	}
	return encodeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: s.finalizeResult(env, req.Method, result)})
}

func (s *Server) dispatch(ctx context.Context, env envelope, req request) (json.RawMessage, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params)
	case "server/discover":
		return s.discover()
	case "ping":
		return json.RawMessage(`{}`), nil
	case "tools/list":
		return s.listTools(req.Params)
	case "tools/call":
		return s.callTool(ctx, env, req.Params)
	case "resources/list":
		return s.listResources(ctx, req.Params)
	case "resources/templates/list":
		return s.listResourceTemplates(req.Params)
	case "resources/read":
		return s.readResource(ctx, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

func (s *Server) initialize(params json.RawMessage) (json.RawMessage, *rpcError) {
	var p initializeParams
	if err := decodeParams(params, &p); err != nil {
		return nil, invalidParams("initialize params: " + err.Error())
	}
	version := ProtocolVersion
	if slices.Contains(legacyProtocolVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	return marshalResult(initializeResult{
		ProtocolVersion: version,
		Capabilities: capabilities{
			Tools:     toolsCapability{ListChanged: false},
			Resources: resourcesCapability{ListChanged: false, Subscribe: false},
		},
		ServerInfo:   serverInfo{Name: ServerName, Version: s.version},
		Instructions: serverInstructions,
		Meta:         capabilityMeta(s.capabilities()),
	})
}

// toolInfo is one entry of tools/list.
type toolInfo struct {
	Name         string         `json:"name"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Annotations  Annotations    `json:"annotations"`
	// ReadActions marks the non-mutating actions of a mixed tool; clients
	// ignore unknown fields, hosts and docs use it.
	ReadActions []string `json:"x-lv24-read-actions,omitempty"`
	// WriteActions marks the mutating actions of a mixed tool, so a host can
	// be precise without interpreting the four hints.
	WriteActions []string `json:"x-lv24-write-actions,omitempty"`
}

func (s *Server) listTools(params json.RawMessage) (json.RawMessage, *rpcError) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	if err := decodeParams(params, &p); err != nil {
		return nil, invalidParams("tools/list params: " + err.Error())
	}
	if p.Cursor != "" {
		// The catalogue is smaller than one page, so this server never issues a
		// cursor; an unknown cursor is a client error.
		return nil, invalidParams("tools/list: unknown cursor")
	}
	infos := make([]toolInfo, 0, len(s.tools))
	for _, t := range s.tools {
		infos = append(infos, toolInfo{
			Name:         t.Name,
			Title:        t.Annotations.Title,
			Description:  t.Description,
			InputSchema:  t.InputSchema,
			OutputSchema: t.OutputSchema,
			Annotations:  t.Annotations,
			ReadActions:  t.ReadActions,
			WriteActions: t.WriteActions,
		})
	}
	return marshalResult(struct {
		Tools []toolInfo `json:"tools"`
	}{Tools: infos})
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callResult struct {
	Content           []contentItem `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError"`
	// Meta carries the per-call compatibility envelope of a migration alias,
	// for example lv24.brain/compat with its grade and ignored fields.
	Meta map[string]any `json:"_meta,omitempty"`
}

// callToolParams is one tools/call, including the MRTR retry fields.
type callToolParams struct {
	Name           string                     `json:"name"`
	Arguments      json.RawMessage            `json:"arguments"`
	InputResponses map[string]json.RawMessage `json:"inputResponses"`
	RequestState   string                     `json:"requestState"`
}

func (s *Server) callTool(ctx context.Context, env envelope, params json.RawMessage) (json.RawMessage, *rpcError) {
	var p callToolParams
	if err := decodeParams(params, &p); err != nil {
		return nil, invalidParams("tools/call params: " + err.Error())
	}
	if p.Name == "" {
		return nil, invalidParams("tools/call: tool name is required")
	}
	t, ok := s.findTool(p.Name)
	if !ok {
		if retired, known := retiredTools[p.Name]; known {
			// 0.17 retired seven names. A typed NOT_FOUND with the native
			// replacement keeps a stale client from guessing.
			return refusalResult(retiredRefusal(p.Name, retired))
		}
		if hidden, known := s.findAnyTool(p.Name); known {
			// The policy omitted the tool, so the call must not reach the
			// backend. A typed refusal tells the caller what to do instead.
			return refusalResult(refusalUnsupported(
				hidden.Name+" is not available: this server runs read-only",
				"brain_search or brain_context").Refusal)
		}
		if alias, known := nativeAliasFor(s.profile.Name, p.Name); known {
			// A native name called against a migration vocabulary is not a
			// protocol error: the client is in the wrong vocabulary and the
			// alias names the replacement.
			return refusalResult(Refusal{
				Code:       RefusalNotFound,
				Message:    p.Name + " is not listed in the " + s.profile.Name + " profile",
				TryInstead: alias,
			})
		}
		return nil, invalidParams("unknown tool: " + p.Name)
	}
	if refusal := s.refusedWriteAction(t, p.Arguments); refusal != nil {
		return refusalResult(*refusal)
	}
	outcome := s.checkInput(env, t, p)
	if outcome.refusal != nil {
		return refusalResult(*outcome.refusal)
	}
	if outcome.result != nil {
		return marshalResult(outcome.result)
	}
	out, err := t.run(ctx, p.Arguments)
	if err != nil {
		// Tool failures are reported in the result so the model can read and
		// correct them; they are not JSON-RPC protocol errors.
		return refusalResult(refusalFrom(err))
	}
	return marshalResult(callResult{
		Content:           []contentItem{{Type: "text", Text: out.Text}},
		StructuredContent: out.Structured,
		IsError:           false,
		Meta:              out.Meta,
	})
}

func (s *Server) findTool(name string) (tool, bool) {
	for _, t := range s.tools {
		if t.Name == name {
			return t, true
		}
	}
	return tool{}, false
}

// findAnyTool looks a tool up in the full list, including the tools the policy
// omits, so a call to a hidden write tool gets a typed refusal instead of
// "unknown tool".
func (s *Server) findAnyTool(name string) (tool, bool) {
	for _, t := range s.allTools {
		if t.Name == name {
			return t, true
		}
	}
	return tool{}, false
}

// decodeParams unmarshals optional params; absent params leave dst untouched.
func decodeParams(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

func invalidParams(message string) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: message}
}

func marshalResult(v any) (json.RawMessage, *rpcError) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "encode result: " + err.Error()}
	}
	return data, nil
}

// encodeResponse serializes a response. Marshal failures cannot occur for the
// fixed response shape, so a failure degrades to an internal error reply.
func encodeResponse(resp response) []byte {
	data, err := json.Marshal(resp)
	if err != nil {
		fallback, _ := json.Marshal(response{
			JSONRPC: "2.0",
			ID:      resp.ID,
			Error:   &rpcError{Code: codeInternalError, Message: "encode response: " + err.Error()},
		})
		return fallback
	}
	return data
}
