package httpapi

import (
	"net/http"
)

// reviewRecord is a placeholder restored after an accidental deletion; the
// full implementation lives with the in-progress provenance work.
func (a *API) reviewRecord(w http.ResponseWriter, r *http.Request) {
	writeErrorCode(w, http.StatusNotImplemented, "not_implemented", "record review is not available")
}
