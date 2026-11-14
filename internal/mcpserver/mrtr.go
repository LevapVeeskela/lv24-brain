package mcpserver

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Multi Round-Trip Requests (MRTR). A 2026 client has no server-initiated
// requests: when a tool needs the user to confirm an operation, the server
// answers with resultType "input_required" and an elicitation request, and the
// client retries the same call with inputResponses and the requestState it was
// given. The state is a signed, short-lived digest of the call, so a retry from
// another tool, another argument set or another process cannot be replayed.
const (
	// confirmationKey is the inputRequests key of the confirmation prompt.
	confirmationKey = "confirm"
	// requestStateTTL bounds how long a confirmation stays valid.
	requestStateTTL = 10 * time.Minute
)

// confirmation is what a tool asks the user before it runs.
type confirmation struct {
	Message string
}

// inputRequiredResult is the MRTR interim result. It replaces the whole tool
// result, so it carries no content.
type inputRequiredResult struct {
	ResultType    string                  `json:"resultType"`
	InputRequests map[string]inputRequest `json:"inputRequests,omitempty"`
	RequestState  string                  `json:"requestState,omitempty"`
}

// inputRequest is one server-to-client request inside an MRTR result.
type inputRequest struct {
	Method string `json:"method"`
	Params any    `json:"params"`
}

// elicitParams asks the client to show a form. The confirmation is a single
// boolean, so the form is the smallest one the spec allows.
type elicitParams struct {
	Mode            string         `json:"mode"`
	Message         string         `json:"message"`
	RequestedSchema map[string]any `json:"requestedSchema"`
}

// elicitResult is one client answer to an elicitation request.
type elicitResult struct {
	Action  string `json:"action"`
	Content struct {
		Confirm bool `json:"confirm"`
	} `json:"content"`
}

// requestState is the integrity-protected payload a retry echoes back.
type requestState struct {
	Version int    `json:"v"`
	Tool    string `json:"tool"`
	Digest  string `json:"digest"`
	Expires int64  `json:"exp"`
}

// mrtrOutcome is what a tool call does after the MRTR check: run the tool, ask
// the client for input, or refuse.
type mrtrOutcome struct {
	result  *inputRequiredResult
	refusal *Refusal
}

// newMRTRKey returns the per-process key that signs requestState. A random key
// keeps state from being forged; the fallback only matters if the OS entropy
// source fails, in which case state is still bound to this process.
func newMRTRKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		binary.BigEndian.PutUint64(key, uint64(time.Now().UnixNano()))
	}
	return key
}

// checkInput applies the MRTR rules to one tools/call.
func (s *Server) checkInput(env envelope, t tool, call callToolParams) mrtrOutcome {
	if t.confirm == nil {
		return mrtrOutcome{}
	}
	needed := t.confirm(call.Arguments)
	if len(call.InputResponses) == 0 {
		if needed == nil || !env.capabilities.supportsElicitation() {
			// The call needs no confirmation, or the client cannot answer one:
			// run the tool as before rather than block it.
			return mrtrOutcome{}
		}
		state, err := s.signRequestState(t.Name, call.Arguments)
		if err != nil {
			return mrtrOutcome{refusal: &Refusal{Code: RefusalFailed, Message: "cannot sign the confirmation: " + err.Error(), Retryable: true}}
		}
		return mrtrOutcome{result: &inputRequiredResult{
			ResultType: resultInputRequired,
			InputRequests: map[string]inputRequest{confirmationKey: {
				Method: "elicitation/create",
				Params: elicitParams{
					Mode:    "form",
					Message: needed.Message,
					RequestedSchema: map[string]any{
						"type":                 "object",
						"properties":           map[string]any{"confirm": map[string]any{"type": "boolean", "description": "True confirms the change; false leaves the record unchanged."}},
						"required":             []string{"confirm"},
						"additionalProperties": false,
					},
				},
			}},
			RequestState: state,
		}}
	}
	if err := s.verifyRequestState(call.RequestState, t.Name, call.Arguments); err != nil {
		return mrtrOutcome{refusal: &Refusal{
			Code:       RefusalInvalidArguments,
			Message:    err.Error(),
			TryInstead: "retry the call without inputResponses to get a fresh confirmation",
			Retryable:  true,
		}}
	}
	var answer elicitResult
	if raw, ok := call.InputResponses[confirmationKey]; ok {
		if err := json.Unmarshal(raw, &answer); err != nil {
			refusal := refusalInvalid("inputResponses.confirm: " + err.Error()).Refusal
			return mrtrOutcome{refusal: &refusal}
		}
	}
	if answer.Action != "accept" || !answer.Content.Confirm {
		message := "the operation was not confirmed; nothing changed"
		if needed != nil {
			message = needed.Message + " (not confirmed; nothing changed)"
		}
		return mrtrOutcome{refusal: &Refusal{
			Code:       RefusalDeclined,
			Message:    message,
			TryInstead: "do not retry; report the record as unchanged or ask the user again",
		}}
	}
	return mrtrOutcome{}
}

// signRequestState binds a confirmation to this tool, these arguments and this
// process for requestStateTTL.
func (s *Server) signRequestState(toolName string, args json.RawMessage) (string, error) {
	digest, err := argumentDigest(toolName, args)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(requestState{
		Version: 1, Tool: toolName, Digest: digest,
		Expires: time.Now().Add(requestStateTTL).Unix(),
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." + s.sign(payload), nil
}

// verifyRequestState rejects a state that is unsigned, expired, or bound to
// another tool or another argument set.
func (s *Server) verifyRequestState(state, toolName string, args json.RawMessage) error {
	parts := strings.Split(state, ".")
	if len(parts) != 2 {
		return fmt.Errorf("requestState is not the state this server issued")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("requestState is not the state this server issued")
	}
	expected := s.sign(payload)
	if !hmac.Equal([]byte(expected), []byte(parts[1])) {
		return fmt.Errorf("requestState was modified; the confirmation is not trusted")
	}
	var parsed requestState
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return fmt.Errorf("requestState is not the state this server issued")
	}
	if parsed.Version != 1 || parsed.Tool != toolName {
		return fmt.Errorf("requestState was issued for another tool")
	}
	if time.Now().Unix() > parsed.Expires {
		return fmt.Errorf("the confirmation expired; ask again")
	}
	digest, err := argumentDigest(toolName, args)
	if err != nil {
		return err
	}
	if parsed.Digest != digest {
		return fmt.Errorf("the arguments changed since the confirmation was issued")
	}
	return nil
}

// sign is the HMAC of a payload under the process key.
func (s *Server) sign(payload []byte) string {
	mac := hmac.New(sha256.New, s.mrtKey)
	mac.Write([]byte(ProtocolVersion2026))
	mac.Write([]byte{0})
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// argumentDigest canonicalises arguments (object key order is normalised by
// encoding/json) and hashes them together with the tool name.
func argumentDigest(toolName string, args json.RawMessage) (string, error) {
	canonical := []byte("{}")
	trimmed := strings.TrimSpace(string(args))
	if trimmed != "" && trimmed != "null" {
		var value any
		if err := json.Unmarshal(args, &value); err != nil {
			return "", fmt.Errorf("arguments are not valid JSON: %w", err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		canonical = encoded
	}
	sum := sha256.New()
	sum.Write([]byte(toolName))
	sum.Write([]byte{0})
	sum.Write(canonical)
	return hex.EncodeToString(sum.Sum(nil)), nil
}
