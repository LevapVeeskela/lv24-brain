package mcpserver

import (
	"bytes"
	"encoding/json"
	"slices"
)

// Protocol revisions. The server is dual-era: every request is served under the
// revision it declares, and clients that open with initialize keep the
// handshake flow.
const (
	// ServerName is reported as serverInfo.
	ServerName = "lv24-brain"
	// ProtocolVersion is the legacy revision served to handshake clients when
	// they ask for a revision this server does not know.
	ProtocolVersion = "2025-06-18"
	// ProtocolVersion2026 is the current, stateless revision: no initialize, a
	// per-request _meta envelope, server/discover and resultType on every
	// result.
	ProtocolVersion2026 = "2026-07-28"
)

// resultType values of a 2026 result.
const (
	resultComplete      = "complete"
	resultInputRequired = "input_required"
)

// Cache hints of the list results, in milliseconds.
const (
	toolsListTTLMs   = 300_000
	discoverTTLMs    = 3_600_000
	cacheScopePublic = "public"
	// cacheScopePrivate marks a result that varies per caller or per server
	// profile and must not be shared through a cache.
	cacheScopePrivate = "private"
)

// Reserved 2026 _meta keys.
const (
	metaProtocolVersion = "io.modelcontextprotocol/protocolVersion"
	metaServerInfo      = "io.modelcontextprotocol/serverInfo"
)

// legacyProtocolVersions are the handshake-era revisions the server may echo
// back in initialize.
var legacyProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// SupportedVersions returns the protocol revisions this server advertises,
// newest first. A 2026 client picks one and declares it in _meta on every
// request.
func SupportedVersions() []string {
	return slices.Concat([]string{ProtocolVersion2026}, legacyProtocolVersions)
}

// Annotations are the MCP tool behaviour hints. Hosts use them to auto-approve
// read-only tools and to warn before destructive ones; clients must treat them
// as untrusted unless the server is trusted.
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

// readOnlyAnnotations describes a tool that only reads local state.
func readOnlyAnnotations(title string) Annotations {
	return Annotations{Title: title, ReadOnlyHint: true, DestructiveHint: false, IdempotentHint: true, OpenWorldHint: false}
}

// appendOnlyAnnotations describes a tool that appends a new record and never
// replaces one; a repeated call creates a second record, so it is not
// idempotent.
func appendOnlyAnnotations(title string) Annotations {
	return Annotations{Title: title, ReadOnlyHint: false, DestructiveHint: false, IdempotentHint: false, OpenWorldHint: false}
}

// mutatingAnnotations describes a tool that replaces or reclassifies stored
// state. Repeating the same call is a no-op, so it is idempotent, but it can
// drop the previous value.
func mutatingAnnotations(title string) Annotations {
	return Annotations{Title: title, ReadOnlyHint: false, DestructiveHint: true, IdempotentHint: true, OpenWorldHint: false}
}

// unrepeatedDestructiveAnnotations describes a destructive tool whose repeated
// call is not a no-op, for example one that appends or discards local data.
func unrepeatedDestructiveAnnotations(title string) Annotations {
	return Annotations{Title: title, ReadOnlyHint: false, DestructiveHint: true, IdempotentHint: false, OpenWorldHint: false}
}

// revision is the protocol era one request is served under.
type revision int

const (
	// revisionLegacy answers with the pre-2026 result shapes.
	revisionLegacy revision = iota
	// revisionModern adds resultType, _meta.serverInfo and cache hints.
	revisionModern
)

// envelope is the protocol metadata of one request.
type envelope struct {
	revision     revision
	capabilities *clientCapabilities
}

// modern reports whether the request is served under the 2026 revision.
func (e envelope) modern() bool { return e.revision == revisionModern }

// clientCapabilities is the subset of the client capabilities the server acts
// on. Only elicitation is used: it gates the MRTR confirmation prompts.
type clientCapabilities struct {
	Elicitation json.RawMessage `json:"elicitation"`
	Sampling    json.RawMessage `json:"sampling"`
	Roots       json.RawMessage `json:"roots"`
}

// supportsElicitation reports whether the client declared elicitation.
func (c *clientCapabilities) supportsElicitation() bool {
	if c == nil {
		return false
	}
	trimmed := bytes.TrimSpace(c.Elicitation)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// requestEnvelope resolves the protocol era of one request from its _meta. A
// request without _meta, and every initialize request, is served under the
// legacy revision; initialize is what selects legacy semantics.
func requestEnvelope(method string, params json.RawMessage) (envelope, *rpcError) {
	if method == "initialize" {
		return envelope{revision: revisionLegacy}, nil
	}
	var p struct {
		Meta json.RawMessage `json:"_meta"`
	}
	if err := decodeParams(params, &p); err != nil || len(bytes.TrimSpace(p.Meta)) == 0 {
		return envelope{revision: revisionLegacy}, nil
	}
	var meta struct {
		ProtocolVersion    json.RawMessage     `json:"io.modelcontextprotocol/protocolVersion"`
		ClientCapabilities *clientCapabilities `json:"io.modelcontextprotocol/clientCapabilities"`
	}
	if err := json.Unmarshal(p.Meta, &meta); err != nil {
		return envelope{}, invalidParams("params._meta: " + err.Error())
	}
	version, ok, err := metaString(meta.ProtocolVersion)
	if err != nil {
		return envelope{}, invalidParams("params._meta." + metaProtocolVersion + ": " + err.Error())
	}
	switch {
	case !ok:
		return envelope{revision: revisionLegacy}, nil
	case version == ProtocolVersion2026:
		return envelope{revision: revisionModern, capabilities: meta.ClientCapabilities}, nil
	case slices.Contains(legacyProtocolVersions, version):
		// A supported handshake-era version selected per request keeps the
		// legacy result shapes; the client may skip initialize entirely.
		return envelope{revision: revisionLegacy}, nil
	default:
		return envelope{}, unsupportedVersion(version)
	}
}

// metaString decodes an optional string-valued _meta key. ok is false when the
// key is absent or null.
func metaString(raw json.RawMessage) (value string, ok bool, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", false, nil
	}
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", false, err
	}
	return value, true, nil
}

// unsupportedVersion builds the 2026 UnsupportedProtocolVersionError. Its data
// lists the revisions the client may retry with.
func unsupportedVersion(requested string) *rpcError {
	return &rpcError{
		Code:    codeUnsupportedProtocolVersion,
		Message: "Unsupported protocol version",
		Data: map[string]any{
			"supported": SupportedVersions(),
			"requested": requested,
		},
	}
}

// discover answers server/discover. It reports the versions, capabilities and
// identity of the server; finalizeResult adds resultType, _meta.serverInfo and
// the cache hints every 2026 result carries.
func (s *Server) discover() (json.RawMessage, *rpcError) {
	return marshalResult(struct {
		SupportedVersions []string       `json:"supportedVersions"`
		Capabilities      capabilities   `json:"capabilities"`
		Instructions      string         `json:"instructions"`
		Meta              map[string]any `json:"_meta,omitempty"`
	}{
		SupportedVersions: SupportedVersions(),
		Capabilities: capabilities{
			Tools:     toolsCapability{ListChanged: false},
			Resources: resourcesCapability{ListChanged: false, Subscribe: false},
		},
		Instructions: serverInstructions,
		Meta:         capabilityMeta(s.capabilities()),
	})
}

// finalizeResult adds the 2026 result envelope to a modern result: the required
// resultType, the serverInfo _meta entry and, for the list and discovery
// results, the ttlMs/cacheScope cache hints. Nothing is added for legacy
// requests, so their byte shape stays what 2025 clients expect.
func (s *Server) finalizeResult(env envelope, method string, result json.RawMessage) json.RawMessage {
	if !env.modern() {
		return result
	}
	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		// A result that is not an object cannot carry the envelope; it is
		// still a valid JSON-RPC result, so it is returned unchanged.
		return result
	}
	if _, ok := payload["resultType"]; !ok {
		payload["resultType"] = resultComplete
	}
	payload["_meta"] = withServerInfo(payload["_meta"], serverInfo{Name: ServerName, Version: s.version})
	if ttl, scope, ok := s.cacheHints(method); ok {
		payload["ttlMs"] = ttl
		payload["cacheScope"] = scope
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return result
	}
	return out
}

// withServerInfo adds the serverInfo entry to a result _meta object.
func withServerInfo(existing any, info serverInfo) map[string]any {
	meta, ok := existing.(map[string]any)
	if !ok {
		meta = map[string]any{}
	}
	meta[metaServerInfo] = info
	return meta
}

// cacheHints returns the ttlMs and cacheScope a method's results must carry
// under the 2026 revision. The discovery result is server-wide and identical
// for every caller, so it is public. The tool list is identical for every
// caller only when no profile hides tools: a read-only server's list differs
// from a writable one, so it is private.
func (s *Server) cacheHints(method string) (ttlMs int, scope string, ok bool) {
	switch method {
	case "tools/list":
		if s.policy.ReadOnly {
			return toolsListTTLMs, cacheScopePrivate, true
		}
		return toolsListTTLMs, cacheScopePublic, true
	case "server/discover":
		return discoverTTLMs, cacheScopePublic, true
	default:
		return 0, "", false
	}
}
