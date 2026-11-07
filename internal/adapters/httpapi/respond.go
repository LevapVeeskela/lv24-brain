package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

var errBodyTooLarge = errors.New("request body too large")

type errorBody struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	CurrentRevision *int64 `json:"currentRevision,omitempty"`
	MergeHint       string `json:"mergeHint,omitempty"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErrorCode(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

func writeAuthError(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="lv24-brain"`)
	writeErrorCode(w, http.StatusUnauthorized, "unauthorized", "authentication required")
}

func (a *API) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var conflict *ConflictError
	switch {
	case errors.Is(err, errBodyTooLarge):
		writeErrorCode(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large")
	case errors.As(err, &conflict):
		current := conflict.Current
		writeJSON(w, http.StatusConflict, errorEnvelope{Error: errorBody{
			Code:            "conflict",
			Message:         "revision conflict",
			CurrentRevision: &current,
			MergeHint:       "reload the record and retry with the current revision",
		}})
	case errors.Is(err, domain.ErrValidation):
		writeErrorCode(w, http.StatusBadRequest, "invalid_argument", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, domain.ErrConflict):
		writeErrorCode(w, http.StatusConflict, "conflict", "resource already exists")
	case errors.Is(err, domain.ErrLastAdmin):
		writeErrorCode(w, http.StatusConflict, "last_admin", "cannot demote or delete the last admin; promote another admin first")
	case errors.Is(err, domain.ErrUnsupported):
		writeErrorCode(w, http.StatusNotImplemented, "unsupported", err.Error())
	case errors.Is(err, auth.ErrInvalidCredentials),
		errors.Is(err, auth.ErrInvalidToken),
		errors.Is(err, auth.ErrInvalidRefresh):
		writeAuthError(w)
	case errors.Is(err, auth.ErrForbidden):
		writeErrorCode(w, http.StatusForbidden, "forbidden", "insufficient role")
	default:
		a.logger.Error("request failed",
			"request_id", requestIDFrom(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"error", err,
		)
		writeErrorCode(w, http.StatusInternalServerError, "internal", "internal server error")
	}
}

// decodeJSON reads one JSON object with a size cap and rejects unknown fields
// and trailing data.
func (a *API) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.MaxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errBodyTooLarge
		}
		if errors.Is(err, io.EOF) {
			return domain.NewValidationError("body", "is required")
		}
		return domain.NewValidationError("body", "invalid JSON")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return domain.NewValidationError("body", "must contain a single JSON object")
	}
	return nil
}

func queryInt(r *http.Request, name string) int {
	value, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(name)))
	if err != nil {
		return 0
	}
	return value
}

func queryInt64(r *http.Request, name string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get(name)), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func queryString(r *http.Request, name string) string {
	return strings.TrimSpace(r.URL.Query().Get(name))
}

func limitOffset(r *http.Request) (int, int) {
	return queryInt(r, "limit"), queryInt(r, "offset")
}
