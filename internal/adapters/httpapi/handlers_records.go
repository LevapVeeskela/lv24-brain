package httpapi

import (
	"context"
	"net/http"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// reviewRecordRequest is the POST .../records/{kind}/{id}/review body.
//
// A review only clears a quarantine: reviewed=false is refused, so a review can
// never re-arm one. Revision is the record revision the caller read. Zero means
// "use the revision read by this request"; the write is still a compare-and-swap
// against that value, never an unchecked last-write-wins update.
type reviewRecordRequest struct {
	Reviewed *bool          `json:"reviewed"`
	Trust    *domain.Trust  `json:"trust"`
	Origin   *domain.Origin `json:"origin"`
	Revision int64          `json:"revision"`
}

// reviewRecordShape is what the review endpoint needs to authorize and
// compare-and-swap one record before it rewrites the provenance.
type reviewRecordShape struct {
	ProjectID  string
	Revision   int64
	Provenance domain.Provenance
}

// reviewNoteResponse is the reviewed note with the flat shape the HTTP client
// decodes: the record fields plus "changed".
type reviewNoteResponse struct {
	NoteRecord
	Changed bool `json:"changed"`
}

type reviewDecisionResponse struct {
	DecisionRecord
	Changed bool `json:"changed"`
}

type reviewAttemptResponse struct {
	AttemptRecord
	Changed bool `json:"changed"`
}

type reviewHandoffResponse struct {
	HandoffRecord
	Changed bool `json:"changed"`
}

// reviewRecord marks one server record reviewed and optionally replaces its
// trust and origin. It is a provenance-only write: the body, project, ID and
// timestamps are untouched, the revision is bumped and the change is audited.
//
// The review is authorized against the record's own project, with the project
// named in the path (when given) required to match. A missing record is 404, a
// stale or unusable compare-and-swap revision is 409, and reviewing a record
// whose origin this engine does not know is 400 until the caller supplies a
// valid origin: review cannot repair a malformed origin on its own.
func (a *API) reviewRecord(w http.ResponseWriter, r *http.Request) {
	var body reviewRecordRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	if body.Reviewed != nil && !*body.Reviewed {
		a.writeError(w, r, domain.NewValidationError("reviewed",
			"a review clears a quarantine and never re-arms one"))
		return
	}
	kind := r.PathValue("kind")
	id := r.PathValue("id")
	current, err := a.loadReviewRecord(r.Context(), kind, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if projectID := r.PathValue("project"); projectID != "" && projectID != current.ProjectID {
		a.writeError(w, r, domain.ErrNotFound)
		return
	}
	if _, ok := a.requireProject(w, r, current.ProjectID, levelWrite); !ok {
		return
	}
	expected, ok := reviewExpectedRevision(w, body.Revision, current.Revision)
	if !ok {
		return
	}
	provenance := current.Provenance
	if body.Origin != nil {
		provenance.Origin = *body.Origin
	}
	if body.Trust != nil {
		provenance.Trust = *body.Trust
	}
	provenance.Reviewed = true
	provenance = provenance.Normalized()
	if err := provenance.Validate(); err != nil {
		a.writeError(w, r, err)
		return
	}
	changed := provenance != current.Provenance
	if changed {
		revision, err := a.store.UpdateRecordProvenance(r.Context(), kind, id, provenance, expected)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		a.publishProject(revision, kind, id, "review", current.ProjectID)
	}
	a.writeReviewedRecord(w, r, kind, id, changed)
}

// reviewExpectedRevision returns the compare-and-swap revision of one review.
// A client revision is accepted only when it matches the record the request just
// read; otherwise the stored revision is used. A record without a positive
// revision cannot be compare-and-swap written, so it is refused with 409 instead
// of falling back to last-write-wins.
func reviewExpectedRevision(w http.ResponseWriter, clientRevision, storedRevision int64) (int64, bool) {
	if clientRevision < 0 {
		writeErrorCode(w, http.StatusBadRequest, "invalid_argument",
			"record.revision must not be negative")
		return 0, false
	}
	if storedRevision <= 0 {
		writeErrorCode(w, http.StatusConflict, "revision_unavailable",
			"the record has no revision to compare and swap; reload it and retry")
		return 0, false
	}
	if clientRevision > 0 && clientRevision != storedRevision {
		current := storedRevision
		writeJSON(w, http.StatusConflict, errorEnvelope{Error: errorBody{
			Code:            "conflict",
			Message:         "revision conflict",
			CurrentRevision: &current,
			MergeHint:       "reload the record and retry with the current revision",
		}})
		return 0, false
	}
	return storedRevision, true
}

// loadReviewRecord reads the record a review targets, whatever its kind.
func (a *API) loadReviewRecord(ctx context.Context, kind, id string) (reviewRecordShape, error) {
	switch kind {
	case ports.RecordNote:
		record, err := a.store.GetNote(ctx, id)
		if err != nil {
			return reviewRecordShape{}, err
		}
		return reviewRecordShape{ProjectID: record.ProjectID, Revision: record.Revision, Provenance: record.Note.Provenance}, nil
	case ports.RecordDecision:
		record, err := a.store.GetDecision(ctx, id)
		if err != nil {
			return reviewRecordShape{}, err
		}
		return reviewRecordShape{ProjectID: record.ProjectID, Revision: record.Revision, Provenance: record.Decision.Provenance}, nil
	case ports.RecordAttempt:
		record, err := a.store.GetAttempt(ctx, id)
		if err != nil {
			return reviewRecordShape{}, err
		}
		return reviewRecordShape{ProjectID: record.ProjectID, Revision: record.Revision, Provenance: record.Attempt.Provenance}, nil
	case ports.RecordHandoff:
		record, err := a.store.GetHandoff(ctx, id)
		if err != nil {
			return reviewRecordShape{}, err
		}
		return reviewRecordShape{ProjectID: record.ProjectID, Revision: record.Revision, Provenance: record.Handoff.Provenance}, nil
	default:
		return reviewRecordShape{}, domain.NewValidationError("kind",
			"unknown record kind (note, decision, attempt, handoff)")
	}
}

// writeReviewedRecord reloads the reviewed record and writes it with the flat
// "changed" flag the server client reads.
func (a *API) writeReviewedRecord(w http.ResponseWriter, r *http.Request, kind, id string, changed bool) {
	ctx := r.Context()
	switch kind {
	case ports.RecordNote:
		record, err := a.store.GetNote(ctx, id)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, reviewNoteResponse{NoteRecord: record, Changed: changed})
	case ports.RecordDecision:
		record, err := a.store.GetDecision(ctx, id)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, reviewDecisionResponse{DecisionRecord: record, Changed: changed})
	case ports.RecordAttempt:
		record, err := a.store.GetAttempt(ctx, id)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, reviewAttemptResponse{AttemptRecord: record, Changed: changed})
	case ports.RecordHandoff:
		record, err := a.store.GetHandoff(ctx, id)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, reviewHandoffResponse{HandoffRecord: record, Changed: changed})
	default:
		a.writeError(w, r, domain.NewValidationError("kind",
			"unknown record kind (note, decision, attempt, handoff)"))
	}
}
