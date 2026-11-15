package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Refusal codes. The set is closed: an agent branches on the code instead of
// matching English text, so a new outcome needs a new documented code.
const (
	// RefusalInvalidArguments marks arguments the tool cannot accept: unknown
	// fields, out-of-range numbers, a malformed cursor.
	RefusalInvalidArguments = "INVALID_ARGUMENTS"
	// RefusalNotFound marks a project, record or decision that does not exist.
	RefusalNotFound = "NOT_FOUND"
	// RefusalConflict marks a write that collides with existing data.
	RefusalConflict = "CONFLICT"
	// RefusalDuplicate marks a write whose normalized text already exists in
	// the same project and kind. try_instead names the existing record; an
	// identical retry cannot succeed.
	RefusalDuplicate = "DUPLICATE"
	// RefusalUnsupported marks a request the engine or the call window does not
	// serve, for example a page past the searchable window.
	RefusalUnsupported = "UNSUPPORTED"
	// RefusalDeclined marks an operation the user refused to confirm. It is not
	// an error of the request; the model must not retry it blindly.
	RefusalDeclined = "DECLINED"
	// RefusalFailed is the catch-all for an unexpected failure. It is not
	// retryable without a new observation.
	RefusalFailed = "FAILED"
)

// Refusal is the typed envelope of a failed tool call. Every brain_* tool
// returns it in structuredContent with isError true, and repeats it in the text
// content for humans.
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// TryInstead is the next action for the caller; "" when none applies.
	TryInstead string `json:"try_instead"`
	// CurrentRevision is the current revision of the changed object when the
	// backend reports one; "" when the store has no revisions (local mode).
	CurrentRevision string `json:"current_revision"`
	// Retryable reports whether an identical retry can succeed without the
	// caller changing anything.
	Retryable bool `json:"retryable"`
}

// RefusalError carries a refusal through an error return.
type RefusalError struct {
	Refusal
}

func (e *RefusalError) Error() string { return e.Refusal.Message }

// refusalInvalid builds an INVALID_ARGUMENTS refusal.
func refusalInvalid(message string) *RefusalError {
	return &RefusalError{Refusal: Refusal{
		Code:       RefusalInvalidArguments,
		Message:    message,
		TryInstead: "check the arguments against inputSchema",
	}}
}

// refusalInvalidf builds an INVALID_ARGUMENTS refusal from a format string.
func refusalInvalidf(format string, args ...any) *RefusalError {
	return refusalInvalid(fmt.Sprintf(format, args...))
}

// refusalUnsupported builds an UNSUPPORTED refusal with a recovery hint.
func refusalUnsupported(message, tryInstead string) *RefusalError {
	return &RefusalError{Refusal: Refusal{Code: RefusalUnsupported, Message: message, TryInstead: tryInstead}}
}

// refusalNotFound builds a NOT_FOUND refusal.
func refusalNotFound(message, tryInstead string) *RefusalError {
	return &RefusalError{Refusal: Refusal{Code: RefusalNotFound, Message: message, TryInstead: tryInstead}}
}

// refusalFrom converts any tool error into a refusal envelope. Domain errors
// keep their meaning; an error that already carries a refusal is returned
// unchanged.
func refusalFrom(err error) Refusal {
	var refused *RefusalError
	if errors.As(err, &refused) {
		return refused.Refusal
	}
	var duplicate *domain.DuplicateError
	if errors.As(err, &duplicate) {
		return Refusal{
			Code:       RefusalDuplicate,
			Message:    err.Error(),
			TryInstead: fmt.Sprintf("read the existing %s with brain_record id=%s, or write different content", duplicate.Kind, duplicate.ID),
		}
	}
	switch {
	case errors.Is(err, domain.ErrValidation):
		return Refusal{
			Code:       RefusalInvalidArguments,
			Message:    err.Error(),
			TryInstead: "check the arguments against inputSchema",
		}
	case errors.Is(err, domain.ErrNotFound):
		return Refusal{
			Code:       RefusalNotFound,
			Message:    err.Error(),
			TryInstead: "list ids with brain_records or find one with brain_search",
		}
	case errors.Is(err, domain.ErrConflict):
		return Refusal{
			Code:       RefusalConflict,
			Message:    err.Error(),
			TryInstead: "read the current record with brain_record, then retry the change",
			Retryable:  true,
		}
	case errors.Is(err, domain.ErrUnsupported), errors.Is(err, domain.ErrForbidden):
		return Refusal{
			Code:       RefusalUnsupported,
			Message:    err.Error(),
			TryInstead: "run the equivalent brain command in a shell where the mode allows it",
		}
	default:
		return Refusal{Code: RefusalFailed, Message: err.Error()}
	}
}

// refusalResult renders a refusal as a failed tool result: the envelope travels
// in structuredContent and the same facts in the text content, so hosts that
// only show text still say what to do next.
func refusalResult(refusal Refusal) (json.RawMessage, *rpcError) {
	text := refusal.Code + ": " + refusal.Message
	if refusal.TryInstead != "" {
		text += "\ntry instead: " + refusal.TryInstead
	}
	if refusal.CurrentRevision != "" {
		text += "\ncurrent_revision: " + refusal.CurrentRevision
	}
	if refusal.Retryable {
		text += "\nretryable: true"
	}
	return marshalResult(callResult{
		Content:           []contentItem{{Type: "text", Text: text}},
		StructuredContent: refusal,
		IsError:           true,
	})
}
