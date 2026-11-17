package localapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// SyncRevisionView identifies the last completed git sync operation.
type SyncRevisionView struct {
	Kind  string    `json:"kind"`
	Value string    `json:"value"`
	At    time.Time `json:"at"`
}

// SyncView is the git sync state of the agent's data repository. It mirrors
// the CLI's `brain sync --status --json` payload so the desktop can explain
// divergence and conflicts without shelling out.
type SyncView struct {
	Provider        string            `json:"provider,omitempty"`
	Branch          string            `json:"branch,omitempty"`
	Ahead           int               `json:"ahead"`
	Behind          int               `json:"behind"`
	Changed         int               `json:"changed"`
	Dirty           bool              `json:"dirty"`
	Unmerged        bool              `json:"unmerged"`
	Conflicts       []string          `json:"conflicts"`
	ConflictRecords []string          `json:"conflict_records"`
	Recovered       bool              `json:"recovered"`
	Reindexed       bool              `json:"reindexed"`
	LastSynced      *SyncRevisionView `json:"last_synced_revision,omitempty"`
}

// SyncService is the git sync surface exposed by the local agent. Status is
// read-only; Sync commits, pulls and optionally pushes. A conflict returns a
// domain.ErrConflict-wrapped error and the current status is readable again.
type SyncService interface {
	Status(context.Context) (SyncView, error)
	Sync(context.Context, bool) (SyncView, error)
}

func (h *Handler) sync(w http.ResponseWriter, r *http.Request) {
	if h.opts.Sync == nil {
		unavailable(w)
		return
	}
	if !method(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		view, err := h.opts.Sync.Status(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "sync_status_failed", "could not read git sync status")
			return
		}
		writeJSON(w, http.StatusOK, view)
	case http.MethodPost:
		var body struct {
			Push bool `json:"push"`
		}
		if r.ContentLength != 0 {
			if !h.decode(w, r, &body) {
				return
			}
		}
		view, err := h.opts.Sync.Sync(r.Context(), body.Push)
		if err != nil {
			if errors.Is(err, domain.ErrConflict) {
				writeError(w, http.StatusConflict, "sync_conflict", "git sync stopped on unresolved conflicts; resolve them and sync again")
				return
			}
			writeError(w, http.StatusInternalServerError, "sync_failed", "git sync failed; see the agent log")
			return
		}
		writeJSON(w, http.StatusOK, view)
	}
}
