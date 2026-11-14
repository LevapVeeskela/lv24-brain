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
	"and use brain_search or brain_journal to find earlier work. " +
	"Record durable findings as they happen with brain_note, brain_decision and brain_attempt (a failed approach, with its reason). " +
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
	tools   []tool
	// catalog makes toolList report every tool group, including the ones a
	// given backend would hide. It is set by Catalog only.
	catalog bool
	// mrtKey signs MRTR requestState.
	mrtKey []byte
}

// New builds a Server. version is reported as serverInfo.version.
func New(backend Backend, version string) *Server {
	s := &Server{backend: backend, version: version, mrtKey: newMRTRKey()}
	s.tools = s.toolList()
	return s
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
}

type capabilities struct {
	Tools toolsCapability `json:"tools"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
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
		Capabilities:    capabilities{Tools: toolsCapability{ListChanged: false}},
		ServerInfo:      serverInfo{Name: ServerName, Version: s.version},
		Instructions:    serverInstructions,
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
		return nil, invalidParams("unknown tool: " + p.Name)
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
