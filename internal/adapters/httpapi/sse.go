package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErrorCode(w, http.StatusInternalServerError, "internal", "streaming is not supported")
		return
	}
	lastSent := lastEventID(r)

	events, unsubscribe := a.broker.subscribe()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if _, err := fmt.Fprintf(w, "retry: %d\n\n", a.cfg.SSERetry.Milliseconds()); err != nil {
		return
	}
	current := int64(0)
	if revision, err := a.store.CurrentRevision(r.Context()); err == nil {
		current = revisionValue(revision)
	}
	ready, _ := json.Marshal(map[string]int64{"revision": current})
	if _, err := fmt.Fprintf(w, "event: ready\ndata: %s\n\n", ready); err != nil {
		return
	}
	flusher.Flush()

	for {
		revisions, err := a.store.ListRevisions(r.Context(), RevisionFilter{
			AfterID:   lastSent,
			Ascending: true,
			Limit:     500,
		})
		if err != nil {
			a.logger.Warn("sse replay failed", "request_id", requestIDFrom(r.Context()), "error", err)
			break
		}
		for _, record := range revisions {
			if err := writeRevisionEvent(w, record); err != nil {
				return
			}
			lastSent = record.ID
		}
		if len(revisions) < 500 {
			break
		}
	}
	flusher.Flush()

	heartbeat := time.NewTicker(a.cfg.SSEHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.broker.done:
			return
		case ev := <-events:
			if ev.Revision <= lastSent {
				continue
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: revision\ndata: %s\n\n", ev.Revision, payload); err != nil {
				return
			}
			lastSent = ev.Revision
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeRevisionEvent(w http.ResponseWriter, record RevisionRecord) error {
	payload, err := json.Marshal(event{
		Revision:   record.ID,
		EntityKind: record.EntityKind,
		EntityID:   record.EntityID,
		Action:     record.Action,
		At:         record.CreatedAt,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: revision\ndata: %s\n\n", record.ID, payload)
	return err
}

func lastEventID(r *http.Request) int64 {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("lastEventId"))
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}
