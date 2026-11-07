// Package mcpserver implements the Model Context Protocol (revision
// 2025-06-18) over newline-delimited JSON-RPC 2.0 messages on a byte stream,
// and exposes brain operations as MCP tools.
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

// ProtocolVersion is the revision this server speaks when the client asks for
// a version it does not know.
const ProtocolVersion = "2025-06-18"

// serverName is reported in serverInfo.
const serverName = "lv24-brain"

// serverInstructions is sent to clients during initialize.
const serverInstructions = "Company Brain is the curated memory of this project. " +
	"Call brain_open at the start of a session to load the project's state, decisions, attempts and last handoff, " +
	"and use brain_search or brain_journal to find earlier work. " +
	"Record durable findings as they happen with brain_note, brain_decision and brain_attempt (a failed approach, with its reason). " +
	"Call brain_handoff at the end of the session with a summary and the next action; git context is captured automatically. " +
	"Never store secrets, tokens or personal data in the brain."

// supportedVersions lists the protocol revisions that may be echoed back.
var supportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Server answers MCP requests against a Backend.
type Server struct {
	backend Backend
	version string
	tools   []tool
}

// New builds a Server. version is reported as serverInfo.version.
func New(backend Backend, version string) *Server {
	s := &Server{backend: backend, version: version}
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
	result, rpcErr := s.dispatch(ctx, req)
	if rpcErr != nil {
		return encodeResponse(response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr})
	}
	return encodeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) dispatch(ctx context.Context, req request) (json.RawMessage, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params)
	case "ping":
		return json.RawMessage(`{}`), nil
	case "tools/list":
		return s.listTools()
	case "tools/call":
		return s.callTool(ctx, req.Params)
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
	if slices.Contains(supportedVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	return marshalResult(initializeResult{
		ProtocolVersion: version,
		Capabilities:    capabilities{Tools: toolsCapability{ListChanged: false}},
		ServerInfo:      serverInfo{Name: serverName, Version: s.version},
		Instructions:    serverInstructions,
	})
}

func (s *Server) listTools() (json.RawMessage, *rpcError) {
	type toolInfo struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	infos := make([]toolInfo, 0, len(s.tools))
	for _, t := range s.tools {
		infos = append(infos, toolInfo{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
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

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
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
	out, err := t.run(ctx, p.Arguments)
	if err != nil {
		// Tool failures are reported in the result so the model can read and
		// correct them; they are not JSON-RPC protocol errors.
		return marshalResult(callResult{
			Content: []contentItem{{Type: "text", Text: err.Error()}},
			IsError: true,
		})
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
