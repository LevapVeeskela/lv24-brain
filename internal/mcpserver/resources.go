package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Resource URIs. Resources are host-controlled read-only context: every URI
// is project-scoped and capped, and every project is checked against the same
// allowlist as the tools. code:// serves one indexed symbol and is advertised
// only by a backend that can open the graph.
const (
	ResourceProjectsURI    = "memory://projects"
	ResourceReviewURI      = "memory://review"
	ResourceStateTemplate  = "memory://project/{id}/state"
	ResourceRecordTemplate = "memory://record/{id}"
	ResourceSymbolTemplate = "code://{project}/symbol/{id}"
	resourceScheme         = "memory"
	resourceCodeScheme     = "code"
)

// Resource cache hints, in milliseconds.
const (
	resourceProjectsTTLMs = 30_000
	resourceStateTTLMs    = 30_000
	resourceRecordTTLMs   = 300_000
	resourceReviewTTLMs   = 60_000
)

// Resource caps, in bytes. A body above its cap is truncated with a preamble
// that names the original size; the meta entry repeats the same facts.
const (
	resourceProjectsMaxBytes = 8 << 10
	resourceStateMaxBytes    = 16 << 10
	resourceRecordMaxBytes   = 1 << 20
	resourceReviewMaxBytes   = 64 << 10
	resourceSymbolMaxBytes   = 32 << 10
	resourceMaxProjects      = 64
	resourceMaxReviewItems   = 50
	resourceSymbolNeighbors  = 20
)

// RecordQuarantineBackend is implemented by backends that can say whether a
// record is quarantined. Record resources exclude a quarantined record unless
// the caller asks for it, matching the trusted-context rule of brain_context.
type RecordQuarantineBackend interface {
	RecordQuarantined(ctx context.Context, projectID, id string) (bool, error)
}

// resourceInfo is one entry of resources/list.
type resourceInfo struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
	Size        int    `json:"size,omitempty"`
}

// resourceTemplate is one entry of resources/templates/list.
type resourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// resourceContent is one body of resources/read.
type resourceContent struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType"`
	Text     string `json:"text"`
}

// listResources answers resources/list with the concrete resources of the
// allowed projects. The state URI is also offered as a template for a project
// that is added after the list was cached.
func (s *Server) listResources(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpcError) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	if err := decodeParams(params, &p); err != nil {
		return nil, invalidParams("resources/list params: " + err.Error())
	}
	if p.Cursor != "" {
		return nil, invalidParams("resources/list: unknown cursor")
	}
	projects, err := s.resourceProjects(ctx)
	if err != nil {
		return nil, resourceError(err)
	}
	resources := make([]resourceInfo, 0, len(projects)+2)
	resources = append(resources,
		resourceInfo{
			URI: ResourceProjectsURI, Name: "projects", Title: "Registered projects",
			Description: "Projects this server may serve, filtered by --projects.", MIMEType: "application/json",
		},
		resourceInfo{
			URI: ResourceReviewURI, Name: "review", Title: "Review findings",
			Description: "Stale or contradictory knowledge across the allowed projects.", MIMEType: "application/json",
		},
	)
	for _, project := range projects {
		resources = append(resources, resourceInfo{
			URI:  "memory://project/" + url.PathEscape(project.ID) + "/state",
			Name: "state-" + project.ID, Title: project.ID + " state",
			Description: "The curated project state.", MIMEType: "text/markdown",
		})
	}
	return marshalResult(struct {
		Resources  []resourceInfo `json:"resources"`
		TTLMs      int            `json:"ttlMs"`
		CacheScope string         `json:"cacheScope"`
	}{Resources: resources, TTLMs: resourceProjectsTTLMs, CacheScope: cacheScopePrivate})
}

// listResourceTemplates answers resources/templates/list. A record id and a
// project id are unbounded, so both are offered as URI templates.
func (s *Server) listResourceTemplates(params json.RawMessage) (json.RawMessage, *rpcError) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	if err := decodeParams(params, &p); err != nil {
		return nil, invalidParams("resources/templates/list params: " + err.Error())
	}
	if p.Cursor != "" {
		return nil, invalidParams("resources/templates/list: unknown cursor")
	}
	templates := []resourceTemplate{
		{
			URITemplate: ResourceStateTemplate, Name: "project-state",
			Description: "Curated state of one registered project.", MIMEType: "text/markdown",
		},
		{
			URITemplate: ResourceRecordTemplate, Name: "record",
			Description: "One record with frontmatter; an explicit id is an explicit request.", MIMEType: "text/markdown",
		},
	}
	if s.servesCodeResource() {
		templates = append(templates, resourceTemplate{
			URITemplate: ResourceSymbolTemplate, Name: "code-symbol",
			Description: "One indexed symbol of a project's code graph, with its source and direct neighbours.", MIMEType: "text/markdown",
		})
	}
	return marshalResult(struct {
		ResourceTemplates []resourceTemplate `json:"resourceTemplates"`
		TTLMs             int                `json:"ttlMs"`
		CacheScope        string             `json:"cacheScope"`
	}{ResourceTemplates: templates, TTLMs: resourceProjectsTTLMs, CacheScope: cacheScopePrivate})
}

// readResource answers resources/read. Errors are JSON-RPC InvalidParams
// errors whose data is the typed refusal envelope, so a host branches on the
// same codes the tools use.
func (s *Server) readResource(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := decodeParams(params, &p); err != nil {
		return nil, invalidParams("resources/read params: " + err.Error())
	}
	uri := strings.TrimSpace(p.URI)
	if uri == "" {
		return nil, invalidParams("resources/read: uri is required")
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, resourceError(refusalInvalid("resource uri: " + err.Error()))
	}
	if parsed.Scheme == resourceCodeScheme {
		return s.readCodeSymbolResource(ctx, parsed.Host, parsed.Path)
	}
	if parsed.Scheme != resourceScheme {
		return nil, resourceError(refusalNotFound("unknown resource scheme "+parsed.Scheme, "memory://projects"))
	}
	query := parsed.Query()
	switch parsed.Host {
	case "projects":
		return s.readProjectsResource(ctx)
	case "project":
		return s.readProjectResource(ctx, parsed.Path, query)
	case "record":
		return s.readRecordResource(ctx, parsed.Path, query)
	case "review":
		return s.readReviewResource(ctx, query)
	default:
		return nil, resourceError(refusalNotFound(
			"unknown resource "+uri,
			"memory://projects, memory://project/<id>/state, memory://record/<id>, memory://review"))
	}
}

// servesCodeResource reports whether this server can serve code:// at all: the
// backend must be able to open a graph and answer Node.
func (s *Server) servesCodeResource() bool {
	_, ok := s.backend.(GraphBackend)
	return ok
}

// readCodeSymbolResource serves code://{project}/symbol/{id} from the code
// graph: one indexed symbol with its source and direct neighbours, capped and
// scoped to the same project allowlist as every other resource.
func (s *Server) readCodeSymbolResource(ctx context.Context, projectID, path string) (json.RawMessage, *rpcError) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, resourceError(refusalNotFound("a code resource needs a project", ResourceSymbolTemplate))
	}
	backend, ok := s.backend.(GraphBackend)
	if !ok {
		return nil, resourceError(refusalUnsupported(
			"code:// is not available: the configured backend does not serve a code graph",
			"memory://projects"))
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || parts[0] != "symbol" || strings.TrimSpace(parts[1]) == "" {
		return nil, resourceError(refusalNotFound("unknown code resource "+path, ResourceSymbolTemplate))
	}
	if err := s.checkResourceProject(ctx, projectID); err != nil {
		return nil, resourceError(err)
	}
	index, err := backend.OpenGraph(ctx, projectID, "")
	if err != nil {
		return nil, resourceError(graphFailure(projectID, err))
	}
	defer closeGraph(index)
	view, err := index.Node(ctx, projectID, parts[1], "", ports.NodeOpts{
		IncludeSource: true, IncludeCallers: true, IncludeCallees: true, Limit: resourceSymbolNeighbors,
	})
	if err != nil {
		return nil, resourceError(graphFailure(projectID, err))
	}
	text := renderCodeNode(view)
	body, truncated := truncateResource(text, resourceSymbolMaxBytes)
	return resourceResult(resourceCodeScheme+"://"+projectID+"/symbol/"+url.PathEscape(parts[1]),
		"text/markdown", body, len(text), resourceSymbolMaxBytes, truncated, resourceStateTTLMs)
}

// readProjectsResource returns the allowed project registry.
func (s *Server) readProjectsResource(ctx context.Context) (json.RawMessage, *rpcError) {
	projects, err := s.resourceProjects(ctx)
	if err != nil {
		return nil, resourceError(err)
	}
	total := len(projects)
	truncated := false
	if len(projects) > resourceMaxProjects {
		projects = projects[:resourceMaxProjects]
		truncated = true
	}
	payload := map[string]any{
		"projects": projects,
		"count":    len(projects),
	}
	if truncated {
		payload["truncated"] = true
		payload["total"] = total
	}
	return resourceJSON(ResourceProjectsURI, payload, resourceProjectsMaxBytes, truncated, resourceProjectsTTLMs)
}

// readProjectResource serves memory://project/{id}/state. Any other leaf is a
// typed NOT_FOUND; a project outside the allowlist is NOT_FOUND, never a
// redirect.
func (s *Server) readProjectResource(ctx context.Context, path string, query url.Values) (json.RawMessage, *rpcError) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "state" {
		return nil, resourceError(refusalNotFound(
			"unknown project resource "+path,
			"memory://project/<id>/state"))
	}
	projectID := parts[0]
	if err := s.checkResourceProject(ctx, projectID); err != nil {
		return nil, resourceError(err)
	}
	backend, ok := s.backend.(StateBackend)
	if !ok {
		return nil, resourceError(refusalUnsupported(
			"memory://project/"+projectID+"/state is not available: the configured backend does not expose project state",
			"brain_open(include=state)"))
	}
	state, err := backend.GetState(ctx, projectID)
	if err != nil {
		return nil, resourceError(err)
	}
	text := renderStateMarkdown(state)
	body, truncated := truncateResource(text, resourceStateMaxBytes)
	return resourceResult("memory://project/"+url.PathEscape(projectID)+"/state", "text/markdown",
		body, len(text), resourceStateMaxBytes, truncated, resourceStateTTLMs)
}

// readRecordResource serves memory://record/{id}. The project may be given as
// ?project=; without it every allowed project is probed, and the record id is
// globally unique.
func (s *Server) readRecordResource(ctx context.Context, path string, query url.Values) (json.RawMessage, *rpcError) {
	id := strings.Trim(strings.TrimSpace(path), "/")
	if id == "" {
		return nil, resourceError(refusalNotFound("a record resource needs an id", "memory://projects"))
	}
	includeUnreviewed, err := resourceBool(query, "include_unreviewed")
	if err != nil {
		return nil, resourceError(err)
	}
	backend, ok := s.backend.(RecordBackend)
	if !ok {
		return nil, resourceError(refusalUnsupported(
			"memory://record/"+id+" is not available: the configured backend cannot read records",
			"brain_search"))
	}
	projectIDs, err := s.resourceRecordProjects(ctx, query.Get("project"))
	if err != nil {
		return nil, resourceError(err)
	}
	for _, projectID := range projectIDs {
		detail, err := backend.GetRecord(ctx, projectID, id)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			return nil, resourceError(err)
		}
		if quarantine, ok := s.backend.(RecordQuarantineBackend); ok {
			quarantined, err := quarantine.RecordQuarantined(ctx, detail.ProjectID, detail.ID)
			if err != nil {
				return nil, resourceError(err)
			}
			if quarantined && !includeUnreviewed {
				return nil, resourceError(refusalNotFound(
					"record "+id+" is quarantined; pass ?include_unreviewed=true to serve it",
					"brain_judge(action=review) clears the quarantine"))
			}
		}
		text := renderRecordMarkdown(detail)
		body, truncated := truncateResource(text, resourceRecordMaxBytes)
		return resourceResult("memory://record/"+url.PathEscape(detail.ID), "text/markdown",
			body, len(text), resourceRecordMaxBytes, truncated, resourceRecordTTLMs)
	}
	return nil, resourceError(refusalNotFound(
		"record "+id+" was not found in the allowed projects",
		"brain_search to find its id"))
}

// readReviewResource serves memory://review. Without ?project= it reports
// every allowed project, bounded to the first 64 projects and 50 findings per
// project.
func (s *Server) readReviewResource(ctx context.Context, query url.Values) (json.RawMessage, *rpcError) {
	backend, ok := s.backend.(ReviewBackend)
	if !ok {
		return nil, resourceError(refusalUnsupported(
			"memory://review is not available: the configured backend cannot review projects",
			"brain_review"))
	}
	limit := resourceLimit(query, resourceMaxReviewItems)
	if project := strings.TrimSpace(query.Get("project")); project != "" {
		if err := s.checkResourceProject(ctx, project); err != nil {
			return nil, resourceError(err)
		}
		review, err := backend.Review(ctx, project)
		if err != nil {
			return nil, resourceError(err)
		}
		payload := reviewResourcePayload(review, limit)
		return resourceJSON("memory://review?project="+url.QueryEscape(project), payload,
			resourceReviewMaxBytes, payload["truncated"] == true, resourceReviewTTLMs)
	}
	projects, err := s.resourceProjects(ctx)
	if err != nil {
		return nil, resourceError(err)
	}
	total := len(projects)
	truncated := false
	if len(projects) > resourceMaxProjects {
		projects = projects[:resourceMaxProjects]
		truncated = true
	}
	rows := make([]any, 0, len(projects))
	for _, project := range projects {
		review, err := backend.Review(ctx, project.ID)
		if err != nil {
			return nil, resourceError(err)
		}
		payload := reviewResourcePayload(review, limit)
		if payload["truncated"] == true {
			truncated = true
		}
		rows = append(rows, payload)
	}
	body := map[string]any{"projects": rows, "count": len(rows)}
	if truncated {
		body["truncated"] = true
		body["total"] = total
	}
	return resourceJSON(ResourceReviewURI, body, resourceReviewMaxBytes, truncated, resourceReviewTTLMs)
}

// resourceJSON renders a JSON payload under its byte cap. A payload that still
// exceeds the cap is cut with the same preamble as a text resource; the meta
// entry names the original size, so a host never mistakes a cut body for a
// complete answer.
func resourceJSON(uri string, payload map[string]any, cap int, truncated bool, ttlMs int) (json.RawMessage, *rpcError) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, resourceError(refusalFailed("resource encode: " + err.Error()))
	}
	text := string(data)
	if len(data) > cap {
		text, _ = truncateResource(text, cap)
		truncated = true
	}
	return resourceResult(uri, "application/json", text, len(data), cap, truncated, ttlMs)
}

// reviewResourcePayload bounds the findings of one project.
func reviewResourcePayload(review Review, limit int) map[string]any {
	findings := review.Findings
	if findings == nil {
		findings = []ReviewFinding{}
	}
	truncated := false
	if len(findings) > limit {
		findings = findings[:limit]
		truncated = true
	}
	payload := map[string]any{
		"project_id": review.ProjectID,
		"summary":    review.Summary,
		"findings":   findings,
	}
	if truncated {
		payload["truncated"] = true
		payload["finding_count"] = len(review.Findings)
	}
	return payload
}

// resourceProjects lists the allowed projects in a stable order.
func (s *Server) resourceProjects(ctx context.Context) ([]Project, error) {
	projects, err := s.backend.Projects(ctx)
	if err != nil {
		return nil, err
	}
	projects = s.policy.filterProjects(projects)
	projects = append([]Project(nil), projects...)
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	if projects == nil {
		projects = []Project{}
	}
	return projects, nil
}

// resourceRecordProjects is the project set a record lookup may probe.
func (s *Server) resourceRecordProjects(ctx context.Context, projectID string) ([]string, error) {
	if project := strings.TrimSpace(projectID); project != "" {
		if err := s.checkResourceProject(ctx, project); err != nil {
			return nil, err
		}
		return []string{project}, nil
	}
	projects, err := s.resourceProjects(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ID)
	}
	return ids, nil
}

// checkResourceProject enforces the read allowlist and the registry: a project
// outside the list, or one this brain does not know, is NOT_FOUND, never a
// redirect or a different project.
func (s *Server) checkResourceProject(ctx context.Context, projectID string) error {
	projectID = strings.TrimSpace(projectID)
	if projectID != "" && s.policy.allowsProject(projectID) {
		projects, err := s.backend.Projects(ctx)
		if err != nil {
			return err
		}
		for _, project := range projects {
			if project.ID == projectID {
				return nil
			}
		}
	}
	return refusalNotFound(
		"project "+projectID+" is not available as a resource",
		"memory://projects lists the projects this server may serve")
}

// resourceResult renders one resource read with its cap and cache hints.
func resourceResult(uri, mime, text string, bytes, cap int, truncated bool, ttlMs int) (json.RawMessage, *rpcError) {
	meta := map[string]any{
		"lv24.brain/resource": map[string]any{
			"bytes": bytes, "cap": cap, "truncated": truncated,
		},
	}
	return marshalResult(struct {
		Contents   []resourceContent `json:"contents"`
		TTLMs      int               `json:"ttlMs"`
		CacheScope string            `json:"cacheScope"`
		Meta       map[string]any    `json:"_meta,omitempty"`
	}{
		Contents:   []resourceContent{{URI: uri, MIMEType: mime, Text: text}},
		TTLMs:      ttlMs,
		CacheScope: cacheScopePrivate,
		Meta:       meta,
	})
}

// resourceError wraps a refusal as a JSON-RPC InvalidParams error. The data is
// the same five-key envelope a tool failure carries.
func resourceError(err error) *rpcError {
	refusal := refusalFrom(err)
	return &rpcError{
		Code:    codeInvalidParams,
		Message: refusal.Code + ": " + refusal.Message,
		Data:    refusal,
	}
}

// resourceBool parses a boolean query parameter.
func resourceBool(query url.Values, key string) (bool, error) {
	raw := strings.TrimSpace(query.Get(key))
	if raw == "" {
		return false, nil
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes":
		return true, nil
	case "0", "false", "no":
		return false, nil
	default:
		return false, refusalInvalid(key + " must be true or false")
	}
}

// resourceLimit clamps ?limit= instead of rejecting an oversized value.
func resourceLimit(query url.Values, maximum int) int {
	raw := strings.TrimSpace(query.Get("limit"))
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return maximum
	}
	if value > maximum {
		return maximum
	}
	return value
}

// truncateResource cuts a text body at its byte cap and prepends the byte
// count. The cut respects UTF-8 boundaries.
func truncateResource(text string, cap int) (string, bool) {
	size := len(text)
	if size <= cap {
		return text, false
	}
	cut := cap
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	preamble := fmt.Sprintf("[truncated: showing first %d of %d bytes]\n\n", cut, size)
	return preamble + text[:cut], true
}

// renderStateMarkdown renders the curated state as the resource body. The
// structured state stays authoritative; this is its Markdown projection.
func renderStateMarkdown(state State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s state\n\n", state.ProjectID)
	fmt.Fprintf(&b, "- updated: %s\n", timeText(state.UpdatedAt))
	fmt.Fprintf(&b, "- fresh_until: %s\n", timeText(state.FreshUntil))
	if state.Revision != "" {
		fmt.Fprintf(&b, "- revision: %s\n", state.Revision)
	}
	writeResourceSection(&b, "Goal", []string{state.Goal})
	writeResourceSection(&b, "Next action", []string{state.NextAction})
	writeResourceSection(&b, "Active", state.Active)
	writeResourceSection(&b, "Blocked", state.Blocked)
	writeResourceSection(&b, "Open questions", state.OpenQuestions)
	return b.String()
}

func writeResourceSection(b *strings.Builder, title string, values []string) {
	b.WriteString("\n## " + title + "\n")
	written := false
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		b.WriteString("- " + strings.TrimSpace(value) + "\n")
		written = true
	}
	if !written {
		b.WriteString("- none\n")
	}
}

// renderRecordMarkdown renders one record with YAML frontmatter. String values
// are JSON-quoted, which is valid YAML for the same scalar.
func renderRecordMarkdown(detail RecordDetail) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", strconv.Quote(detail.ID))
	fmt.Fprintf(&b, "project: %s\n", strconv.Quote(detail.ProjectID))
	fmt.Fprintf(&b, "kind: %s\n", strconv.Quote(detail.Kind))
	fmt.Fprintf(&b, "title: %s\n", strconv.Quote(detail.Title))
	if detail.Status != "" {
		fmt.Fprintf(&b, "status: %s\n", strconv.Quote(detail.Status))
	}
	fmt.Fprintf(&b, "created_at: %s\n", strconv.Quote(timeText(detail.CreatedAt)))
	fmt.Fprintf(&b, "updated_at: %s\n", strconv.Quote(timeText(detail.UpdatedAt)))
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# %s\n\n", detail.Title)
	switch detail.Kind {
	case "note":
		writeResourceBody(&b, detail.Body)
	case "decision":
		writeResourceSection(&b, "Context", []string{detail.Context})
		writeResourceSection(&b, "Alternatives", detail.Alternatives)
		writeResourceSection(&b, "Consequences", detail.Consequences)
		writeResourceSection(&b, "Evidence", detail.Evidence)
	case "attempt":
		writeResourceSection(&b, "Hypothesis", []string{detail.Hypothesis})
		writeResourceSection(&b, "Result", []string{detail.Result})
		writeResourceSection(&b, "Reason", []string{detail.Reason})
	case "handoff":
		writeResourceBody(&b, detail.Body)
		writeResourceSection(&b, "Next action", []string{detail.NextAction})
		writeResourceSection(&b, "Git context", []string{detail.GitContext})
	default:
		writeResourceBody(&b, detail.Body)
	}
	if len(detail.Links) > 0 {
		b.WriteString("\n## Links\n")
		for _, link := range detail.Links {
			fmt.Fprintf(&b, "- %s: [[%s]] (%s)\n", link.Field, link.Ref, link.State)
		}
	}
	if len(detail.Relations) > 0 {
		b.WriteString("\n## Relations\n")
		for _, relation := range detail.Relations {
			fmt.Fprintf(&b, "- %s %s (%s)\n", relation.Relation, relation.Target, relation.Status)
		}
	}
	return b.String()
}

func writeResourceBody(b *strings.Builder, body string) {
	if strings.TrimSpace(body) == "" {
		b.WriteString("_(empty)_\n")
		return
	}
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n")
}

func timeText(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339)
}
