package localapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type errorEnvelope struct {
	Error apiError `json:"error"`
}
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NewHandler constructs a handler for an already-bound loopback listener.
// Port must be the listener's actual port and Token must be non-empty.
func NewHandler(opts Options) (*Handler, error) {
	if opts.Token == nil || strings.TrimSpace(opts.Token.Token()) == "" {
		return nil, errors.New("localapi: token provider is required")
	}
	if opts.Port < 1 || opts.Port > 65535 {
		return nil, errors.New("localapi: valid listener port is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.StartedAt.IsZero() {
		opts.StartedAt = opts.Now().UTC()
	}
	if opts.MaxBody <= 0 {
		opts.MaxBody = DefaultMaxBodyBytes
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = 15 * time.Second
	}
	if opts.Status == "" {
		opts.Status = "running"
	}
	h := &Handler{opts: opts}
	return h, nil
}

// ServeHTTP enforces Host, Origin and bearer authentication before routing.
// It intentionally emits no request logs and no CORS headers.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.validHost(r.Host) {
		writeError(w, http.StatusForbidden, "forbidden", "request host is not allowed")
		return
	}
	_, hasOrigin := r.Header[http.CanonicalHeaderKey("Origin")]
	_, hasReferer := r.Header[http.CanonicalHeaderKey("Referer")]
	if hasOrigin || hasReferer {
		writeError(w, http.StatusForbidden, "forbidden", "browser-origin requests are not allowed")
		return
	}
	authorization := r.Header.Values("Authorization")
	if len(authorization) != 1 || !h.authorized(authorization[0]) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "unauthorized", "bearer token is required")
		return
	}
	h.route(w, r)
}

func (h *Handler) validHost(host string) bool {
	// The listener is IPv4 loopback only. Accepting localhost/::1 would let a
	// client that resolves localhost to IPv6 send the bearer token to a
	// different process bound on [::1].
	return host == net.JoinHostPort("127.0.0.1", strconv.Itoa(h.opts.Port))
}

func (h *Handler) authorized(header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided := strings.TrimPrefix(header, prefix)
	expected := h.opts.Token.Token()
	if expected == "" || provided == "" {
		return false
	}
	// Hash to fixed-size values first so token length does not leak through
	// ConstantTimeCompare's length check.
	a, b := sha256.Sum256([]byte(provided)), sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	switch {
	case path == "/v1/health":
		h.health(w, r)
	case path == "/v1/connections":
		h.connections(w, r)
	case strings.HasPrefix(path, "/v1/connections/"):
		h.connectionRoute(w, r, strings.TrimPrefix(path, "/v1/connections/"))
	case path == "/v1/runs":
		h.runs(w, r)
	case strings.HasPrefix(path, "/v1/runs/"):
		h.runRoute(w, r, strings.TrimPrefix(path, "/v1/runs/"))
	case path == "/v1/schedules":
		h.schedules(w, r)
	case strings.HasPrefix(path, "/v1/schedules/"):
		h.scheduleRoute(w, r, strings.TrimPrefix(path, "/v1/schedules/"))
	case path == "/v1/projects":
		h.projects(w, r)
	case path == "/v1/settings":
		h.settings(w, r)
	case path == "/v1/engine/update":
		h.engineUpdate(w, r)
	case path == "/v1/shutdown":
		h.shutdown(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
	}
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	now := h.opts.Now().UTC()
	uptime := now.Sub(h.opts.StartedAt)
	if uptime < 0 {
		uptime = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": h.redact(h.opts.Version), "pid": h.opts.PID, "uptimeSeconds": int64(uptime.Seconds()), "status": h.redact(h.opts.Status)})
}

type connectionView struct {
	ID       string               `json:"id"`
	Provider string               `json:"provider"`
	Backend  domain.Backend       `json:"backend,omitempty"`
	Account  string               `json:"account,omitempty"`
	BaseURL  string               `json:"baseUrl,omitempty"`
	Projects []string             `json:"projects,omitempty"`
	Schedule string               `json:"schedule,omitempty"`
	Publish  domain.PublishPolicy `json:"publish,omitempty"`
	Settings map[string]string    `json:"settings,omitempty"`
}

// connectionPatch applies only the fields present in the request. Projects
// and Settings replace the stored value as a whole when present; they are not
// merged with the previous list or map.
type connectionPatch struct {
	ID       *string               `json:"id,omitempty"`
	Provider *string               `json:"provider,omitempty"`
	Backend  *domain.Backend       `json:"backend,omitempty"`
	Account  *string               `json:"account,omitempty"`
	BaseURL  *string               `json:"baseUrl,omitempty"`
	Projects *[]string             `json:"projects,omitempty"`
	Schedule *string               `json:"schedule,omitempty"`
	Publish  *domain.PublishPolicy `json:"publish,omitempty"`
	Settings *map[string]string    `json:"settings,omitempty"`
}

func applyConnectionPatch(connection *domain.Connection, patch connectionPatch) {
	if patch.Provider != nil {
		connection.Provider = *patch.Provider
	}
	if patch.Backend != nil {
		connection.Backend = *patch.Backend
	}
	if patch.Account != nil {
		connection.Account = *patch.Account
	}
	if patch.BaseURL != nil {
		connection.BaseURL = *patch.BaseURL
	}
	if patch.Projects != nil {
		connection.Projects = append([]string(nil), (*patch.Projects)...)
	}
	if patch.Schedule != nil {
		connection.Schedule = *patch.Schedule
	}
	if patch.Publish != nil {
		connection.Publish = *patch.Publish
	}
	if patch.Settings != nil {
		connection.Settings = *patch.Settings
	}
}

func (h *Handler) connections(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if h.opts.Connections == nil {
			unavailable(w)
			return
		}
		items, err := h.opts.Connections.List(r.Context())
		if err != nil {
			internal(w)
			return
		}
		views := make([]connectionView, 0, len(items))
		for _, item := range items {
			views = append(views, h.viewConnection(item))
		}
		writeJSON(w, http.StatusOK, views)
	case http.MethodPost:
		if h.opts.Connections == nil || h.opts.Sources == nil {
			unavailable(w)
			return
		}
		var item domain.Connection
		if !h.decode(w, r, &item) {
			return
		}
		if err := h.validateConnection(item); err != nil {
			writeError(w, http.StatusBadRequest, "validation", "connection is invalid or contains a secret field")
			return
		}
		if err := h.opts.Connections.Create(r.Context(), item); err != nil {
			h.writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, h.viewConnection(item))
	default:
		method(w, r, http.MethodGet, http.MethodPost)
	}
}

func (h *Handler) connectionRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts, ok := pathParts(rest)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "connection not found")
		return
	}
	id := parts[0]
	if len(parts) == 3 && parts[1] == "credentials" {
		if r.Method != http.MethodPost {
			method(w, r, http.MethodPost)
			return
		}
		h.setCredential(w, r, id, parts[2])
		return
	}
	if len(parts) == 2 && parts[1] == "test" {
		if r.Method != http.MethodPost {
			method(w, r, http.MethodPost)
			return
		}
		h.testConnection(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "run" {
		if r.Method != http.MethodPost {
			method(w, r, http.MethodPost)
			return
		}
		h.runConnection(w, r, id)
		return
	}
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if h.opts.Connections == nil {
			unavailable(w)
			return
		}
		item, err := h.opts.Connections.Get(r.Context(), id)
		if err != nil {
			h.writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, h.viewConnection(item))
	case http.MethodPatch:
		if h.opts.Connections == nil || h.opts.Sources == nil {
			unavailable(w)
			return
		}
		var patch connectionPatch
		if !h.decode(w, r, &patch) {
			return
		}
		if patch.ID != nil && *patch.ID != id {
			writeError(w, http.StatusBadRequest, "validation", "connection id cannot be changed")
			return
		}
		var validationErr error
		item, err := h.opts.Connections.Patch(r.Context(), id, func(connection *domain.Connection) error {
			applyConnectionPatch(connection, patch)
			validationErr = h.validateConnection(*connection)
			return validationErr
		})
		if err != nil {
			if validationErr != nil {
				writeError(w, http.StatusBadRequest, "validation", "connection is invalid or contains a secret field")
				return
			}
			h.writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, h.viewConnection(item))
	case http.MethodDelete:
		if h.opts.Connections == nil {
			unavailable(w)
			return
		}
		if err := h.opts.Connections.Delete(r.Context(), id); err != nil {
			h.writeServiceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		method(w, r, http.MethodGet, http.MethodPatch, http.MethodDelete)
	}
}

func (h *Handler) validateConnection(c domain.Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	descriptor, err := h.opts.Sources.Descriptor(c.Provider)
	if err != nil {
		return err
	}
	if err := descriptor.ValidateConnection(c); err != nil {
		return err
	}
	if err := domain.ValidateSchedule(c.Schedule); err != nil {
		return err
	}
	return nil
}

func (h *Handler) viewConnection(c domain.Connection) connectionView {
	view := connectionView{ID: c.ID, Provider: c.Provider, Backend: c.Backend, Account: c.Account, BaseURL: safeBaseURL(c.BaseURL),
		Projects: append([]string(nil), c.Projects...), Schedule: c.Schedule, Publish: c.Publish}
	if h.opts.Sources == nil {
		return view
	}
	if d, err := h.opts.Sources.Descriptor(c.Provider); err == nil {
		for key, value := range c.Settings {
			field, ok := d.Field(key)
			if ok && !field.Secret && !isSecretKey(key) {
				if view.Settings == nil {
					view.Settings = map[string]string{}
				}
				view.Settings[key] = h.redact(value)
			}
		}
	}
	return view
}

func (h *Handler) setCredential(w http.ResponseWriter, r *http.Request, id, field string) {
	if h.opts.Connections == nil || h.opts.Sources == nil || h.opts.Credentials == nil {
		unavailable(w)
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	if body.Value == "" {
		writeError(w, http.StatusBadRequest, "validation", "credential value is required")
		return
	}
	connection, err := h.opts.Connections.Get(r.Context(), id)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	d, err := h.opts.Sources.Descriptor(connection.Provider)
	configField, found := d.Field(field)
	if err != nil || !found || !configField.Secret {
		writeError(w, http.StatusBadRequest, "validation", "field is not a credential field")
		return
	}
	if err := h.opts.Credentials.Set(r.Context(), id, field, body.Value); err != nil {
		internal(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) testConnection(w http.ResponseWriter, r *http.Request, id string) {
	if h.opts.Connections == nil || h.opts.Sources == nil {
		unavailable(w)
		return
	}
	connection, err := h.opts.Connections.Get(r.Context(), id)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	checker, err := h.opts.Sources.Checker(r.Context(), connection)
	if err != nil {
		internal(w)
		return
	}
	if checker == nil {
		internal(w)
		return
	}
	if err := checker.Check(r.Context()); err != nil {
		internal(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) runConnection(w http.ResponseWriter, r *http.Request, id string) {
	if h.opts.Connections == nil || h.opts.Jobs == nil {
		unavailable(w)
		return
	}
	var body RunOptions
	if r.ContentLength != 0 {
		if !h.decode(w, r, &body) {
			return
		}
	}
	if body.Limit < 0 || body.Days < 0 || (body.Days > 0 && body.Since != nil) {
		writeError(w, http.StatusBadRequest, "validation", "run options are invalid")
		return
	}
	if body.Since != nil {
		value := body.Since.UTC()
		body.Since = &value
	}
	if _, err := h.opts.Connections.Get(r.Context(), id); err != nil {
		h.writeServiceError(w, err)
		return
	}
	jobID, err := h.opts.Jobs.Queue(r.Context(), id, body)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"jobId": h.redact(jobID)})
}

func (h *Handler) runs(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.opts.Runs == nil {
		unavailable(w)
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 500 {
			writeError(w, http.StatusBadRequest, "validation", "limit must be between 1 and 500")
			return
		}
		limit = value
	}
	items, err := h.opts.Runs.ListRuns(r.Context(), limit)
	if err != nil {
		internal(w)
		return
	}
	views := make([]RunView, 0, len(items))
	for _, item := range items {
		views = append(views, h.safeRun(item))
	}
	writeJSON(w, http.StatusOK, views)
}

func (h *Handler) runRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts, ok := pathParts(rest)
	if !ok || len(parts) > 2 {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "cancel" {
		if !method(w, r, http.MethodPost) {
			return
		}
		h.cancelRun(w, r, id)
		return
	}
	if h.opts.Runs == nil {
		unavailable(w)
		return
	}
	if len(parts) == 2 && parts[1] == "log" {
		if !method(w, r, http.MethodGet) {
			return
		}
		h.streamRunLog(w, r, id)
		return
	}
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	if !method(w, r, http.MethodGet) {
		return
	}
	item, err := h.opts.Runs.GetRun(r.Context(), id)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.safeRun(item))
}

func (h *Handler) cancelRun(w http.ResponseWriter, r *http.Request, id string) {
	if h.opts.Cancels == nil {
		unavailable(w)
		return
	}
	if _, err := h.opts.Cancels.CancelJob(r.Context(), id); err != nil {
		h.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "canceling"})
}

func (h *Handler) safeRun(run domain.ConnectorRun) RunView {
	view := RunView{ID: run.ID, Source: run.Source, ConnectionID: run.ConnectionID, Status: run.Status,
		StartedAt: run.StartedAt, FinishedAt: run.FinishedAt}
	view.ID = h.redact(view.ID)
	view.Source = h.redact(view.Source)
	view.ConnectionID = h.redact(view.ConnectionID)
	if run.Counts != nil {
		view.Counts = make(map[string]int, len(run.Counts))
		for key, value := range run.Counts {
			view.Counts[h.redact(key)] = value
		}
	}
	if run.Stats != nil {
		stats := *run.Stats
		view.Stats = &stats
	}
	return view
}

func (h *Handler) streamRunLog(w http.ResponseWriter, r *http.Request, id string) {
	stream, err := h.opts.Runs.OpenRunLog(r.Context(), id)
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	defer stream.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		internal(w)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	// Normal responses inherit the server's 60s write deadline; an SSE stream
	// is long-lived and manages its own lifetime through the request context.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	ticker := time.NewTicker(h.opts.Heartbeat)
	defer ticker.Stop()
	lines := 0
	for lines < MaxLogLines {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case line, open := <-stream.Lines():
			if !open {
				return
			}
			// Normalize line breaks before redaction: a credential split
			// across lines would otherwise escape the value pattern.
			raw := strings.NewReplacer("\r", " ", "\n", " ").Replace(line.Text)
			text := boundedText(h.redact(raw), MaxLogLineBytes)
			text = strings.ReplaceAll(strings.ReplaceAll(text, "\r", ""), "\n", "\\n")
			if _, err := fmt.Fprintf(w, "data: %s\n\n", text); err != nil {
				return
			}
			flusher.Flush()
			lines++
		}
	}
	// Reaching the cap closes a stream that may still have lines. Name the
	// truncation explicitly so a client cannot mistake it for the end of the log.
	if _, err := io.WriteString(w, "event: truncated\ndata: log line limit reached\n\n"); err != nil {
		return
	}
	flusher.Flush()
}

func (h *Handler) redact(value string) string {
	if token := h.opts.Token.Token(); token != "" {
		value = strings.ReplaceAll(value, token, "[redacted]")
	}
	value, _ = app.RedactIngest(value)
	if h.opts.Redact != nil {
		value = h.opts.Redact(value)
	}
	return value
}

func (h *Handler) schedules(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.opts.Schedules == nil {
		unavailable(w)
		return
	}
	items, err := h.opts.Schedules.ListSchedules(r.Context())
	if err != nil {
		internal(w)
		return
	}
	if items == nil {
		items = []Schedule{}
	}
	for i := range items {
		items[i].ConnectionID = h.redact(items[i].ConnectionID)
		if domain.ValidateSchedule(items[i].Expression) != nil {
			items[i].Expression = "manual"
		}
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) scheduleRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts, ok := pathParts(rest)
	if !ok || len(parts) != 1 {
		writeError(w, http.StatusNotFound, "not_found", "schedule not found")
		return
	}
	if !method(w, r, http.MethodPut) {
		return
	}
	if h.opts.Schedules == nil || h.opts.Connections == nil {
		unavailable(w)
		return
	}
	var body struct {
		Schedule string `json:"schedule"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	if err := domain.ValidateSchedule(body.Schedule); err != nil {
		writeError(w, http.StatusBadRequest, "validation", "schedule is invalid; use manual, @hourly, @daily, or @every with an interval of at least 1m")
		return
	}
	// SetSchedule patches the connection atomically and returns ErrNotFound
	// for an unknown ID, so a separate (racy) existence check is unnecessary.
	if err := h.opts.Schedules.SetSchedule(r.Context(), parts[0], body.Schedule); err != nil {
		h.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Schedule{ConnectionID: parts[0], Expression: body.Schedule})
}

func (h *Handler) projects(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.opts.Projects == nil {
		unavailable(w)
		return
	}
	items, err := h.opts.Projects.ListProjects(r.Context())
	if err != nil {
		internal(w)
		return
	}
	if items == nil {
		items = []Project{}
	}
	for i := range items {
		items[i].ID = h.redact(items[i].ID)
		items[i].Name = h.redact(items[i].Name)
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	if h.opts.Settings == nil {
		unavailable(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		settings, err := h.opts.Settings.GetSettings(r.Context())
		if err != nil {
			internal(w)
			return
		}
		writeJSON(w, http.StatusOK, h.safeSettings(settings))
	case http.MethodPut:
		var body map[string]string
		if !h.decode(w, r, &body) {
			return
		}
		for key := range body {
			if strings.TrimSpace(key) == "" || isSecretKey(key) {
				writeError(w, http.StatusBadRequest, "validation", "settings keys must be non-secret values")
				return
			}
		}
		if err := h.opts.Settings.PutSettings(r.Context(), body); err != nil {
			h.writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, h.safeSettings(body))
	default:
		method(w, r, http.MethodGet, http.MethodPut)
	}
}

func (h *Handler) engineUpdate(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	if h.opts.Updater == nil {
		unavailable(w)
		return
	}
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, "confirmation_required", "explicit confirmation is required")
		return
	}
	result, err := h.opts.Updater.Update(r.Context())
	if err != nil {
		internal(w)
		return
	}
	result.Version = h.redact(result.Version)
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) shutdown(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	if h.opts.Shutdown == nil {
		unavailable(w)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "shutting_down"})
	// Invoke after the response has been committed so graceful shutdown cannot
	// cut off the acknowledgement.
	go func() { _ = h.opts.Shutdown.RequestShutdown(context.Background()) }()
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	mediaType, _, err := mimeParse(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "content type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.opts.MaxBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds the configured limit")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "request body is invalid")
		}
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds the configured limit")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "request body is invalid")
		}
		return false
	}
	return true
}

func (h *Handler) writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrValidation):
		writeError(w, http.StatusBadRequest, "validation", "request is invalid")
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "resource is busy or conflicts with existing data")
	default:
		internal(w)
	}
}

func pathParts(rest string) ([]string, bool) {
	if rest == "" || strings.Contains(rest, "//") {
		return nil, false
	}
	parts := strings.Split(rest, "/")
	for index, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil || decoded == "" || decoded == "." || decoded == ".." || strings.Contains(decoded, "/") {
			return nil, false
		}
		parts[index] = decoded
	}
	return parts, true
}

// method accepts the request only when its method is in allowed. A mismatch is
// an HTTP 405 with the RFC 9110 Allow header; the sanitized error envelope and
// its stable method_not_allowed code stay unchanged.
func method(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, candidate := range allowed {
		if r.Method == candidate {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed for this endpoint")
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: apiError{Code: code, Message: message}})
}

func internal(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, "internal_error", "request could not be completed")
}
func unavailable(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, "unavailable", "endpoint dependency is not configured")
}

func (h *Handler) safeSettings(settings map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range settings {
		if !isSecretKey(key) {
			result[key] = h.redact(value)
		}
	}
	return result
}

func isSecretKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(key))
	if normalized == "auth" || normalized == "authorization" {
		return true
	}
	for _, marker := range []string{
		"secret", "token", "password", "passwd", "pwd", "credential", "apikey",
		"privatekey", "accesskey", "session", "cookie", "bearer",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func boundedText(value string, max int) string {
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func safeBaseURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func mimeParse(raw string) (string, map[string]string, error) { return mime.ParseMediaType(raw) }
