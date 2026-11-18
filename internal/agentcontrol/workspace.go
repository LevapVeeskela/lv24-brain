package agentcontrol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/localapi"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// WorkspaceApp is the use-case slice behind the merge, digest, path and
// journal routes. It is separate from MemoryApp so a build without a record
// merger can still serve the memory surface.
type WorkspaceApp interface {
	PlanMerge(context.Context, app.MergeOptions) (ports.MergePlan, error)
	Merge(context.Context, app.MergeOptions) (app.MergeResult, error)
	RollbackMerge(context.Context, string) (app.MergeRollbackResult, error)
	Digest(context.Context, app.DigestOptions) (app.Digest, error)
	AdoptLocalPaths(context.Context, bool) (app.AdoptReport, error)
	ResolveSelector(context.Context, string) (app.Resolved, error)
	GetProject(context.Context, string) (domain.Project, error)
	SaveProject(context.Context, app.ProjectInput) (domain.Project, error)
	ReadJournal(context.Context, ports.JournalQuery) ([]ports.JournalEntry, error)
}

// workspaceService adapts the workspace use cases to the local API wire views.
// It never returns raw-zone text: merge plans, digests and the journal read
// curated data only.
type workspaceService struct {
	app     WorkspaceApp
	dataDir string
	now     func() time.Time
}

func newWorkspaceService(workspace WorkspaceApp, dataDir string, now func() time.Time) *workspaceService {
	if now == nil {
		now = time.Now
	}
	return &workspaceService{app: workspace, dataDir: dataDir, now: now}
}

// mergeTokenVersion is bumped when the opaque plan token changes shape.
const mergeTokenVersion = 1

// Token kinds. A plan token names the merge request and the fingerprint that
// was shown to the client; an apply replaces it with a manifest token so the
// same plan_id field reaches the rollback route.
const (
	mergeTokenPlan     = "plan"
	mergeTokenManifest = "manifest"
)

// mergeToken is the opaque plan_id payload. It is not signed: the bearer token
// is the authority, and every apply re-plans the request against the store.
type mergeToken struct {
	Version     int      `json:"v"`
	Kind        string   `json:"k"`
	Project     string   `json:"p"`
	Survivor    string   `json:"s,omitempty"`
	Duplicates  []string `json:"d,omitempty"`
	Fingerprint string   `json:"f,omitempty"`
	Manifest    string   `json:"m,omitempty"`
}

// stalePlanError reports that a plan token no longer matches the records. It
// unwraps to domain.ErrConflict and carries the same "stale_plan" code the file
// store uses, so the handler maps both to 409 merge_stale.
type stalePlanError struct{ operation string }

func (e *stalePlanError) Error() string {
	return "stale_plan: " + e.operation + " refused because the records changed since the plan was shown; nothing was written, run the plan again"
}

func (e *stalePlanError) Unwrap() error { return domain.ErrConflict }

func (e *stalePlanError) Code() string { return "stale_plan" }

// PlanMerge plans a merge without writing and returns the plan token.
func (s *workspaceService) PlanMerge(ctx context.Context, request localapi.MergePlanRequest) (localapi.MergePlan, error) {
	plan, err := s.app.PlanMerge(ctx, app.MergeOptions{
		ProjectID: request.ProjectID, Survivor: request.Survivor, Duplicates: request.Duplicates,
	})
	if err != nil {
		return localapi.MergePlan{}, err
	}
	token, err := encodeMergeToken(mergeToken{
		Version: mergeTokenVersion, Kind: mergeTokenPlan, Project: plan.ProjectID,
		Survivor: strings.TrimSpace(request.Survivor), Duplicates: trimmedDuplicates(request.Duplicates),
		Fingerprint: plan.Fingerprint,
	})
	if err != nil {
		return localapi.MergePlan{}, err
	}
	view := mergePlanView(plan)
	view.PlanID = token
	return view, nil
}

// ApplyMerge re-plans the token's request, refuses a changed fingerprint
// before any write and applies the merge.
func (s *workspaceService) ApplyMerge(ctx context.Context, request localapi.MergeApplyRequest) (localapi.MergeResult, error) {
	token, err := decodeMergeToken(request.PlanID)
	if err != nil {
		return localapi.MergeResult{}, err
	}
	if token.Kind != mergeTokenPlan {
		return localapi.MergeResult{}, domain.NewValidationError("plan_id", "is not a merge plan; run POST /v1/merge/plan first")
	}
	if !projectInScope(token.Project, request.AllowedProjects) {
		return localapi.MergeResult{}, domain.ErrForbidden
	}
	options := app.MergeOptions{
		ProjectID: token.Project, Survivor: token.Survivor, Duplicates: token.Duplicates, Apply: true,
	}
	fresh, err := s.app.PlanMerge(ctx, options)
	if err != nil {
		return localapi.MergeResult{}, err
	}
	if fresh.Fingerprint != token.Fingerprint {
		return localapi.MergeResult{}, &stalePlanError{operation: "merge apply"}
	}
	result, err := s.app.Merge(ctx, options)
	view := localapi.MergeResult{
		PlanID: request.PlanID, Project: token.Project, Applied: result.Applied,
		Manifest: result.Manifest, Files: mergeFileViews(result.Files),
	}
	if err != nil {
		return view, err
	}
	if result.Manifest != "" {
		rollbackToken, err := encodeMergeToken(mergeToken{
			Version: mergeTokenVersion, Kind: mergeTokenManifest,
			Project: token.Project, Manifest: result.Manifest,
		})
		if err != nil {
			return view, err
		}
		view.PlanID = rollbackToken
	}
	return view, nil
}

// RollbackMerge restores one merge. plan_id is the token returned by an apply,
// or a data-directory-relative manifest path for clients that kept it.
func (s *workspaceService) RollbackMerge(ctx context.Context, request localapi.MergeRollbackRequest) (localapi.MergeRollbackResult, error) {
	manifest, project, err := decodeRollbackReference(request.PlanID)
	if err != nil {
		return localapi.MergeRollbackResult{}, err
	}
	if !projectInScope(project, request.AllowedProjects) {
		return localapi.MergeRollbackResult{}, domain.ErrForbidden
	}
	result, err := s.app.RollbackMerge(ctx, manifest)
	view := localapi.MergeRollbackResult{
		PlanID: request.PlanID, Manifest: result.Manifest, Files: mergeFileViews(result.Files),
		Conflicts: result.Conflicts, AlreadyRolledBack: result.AlreadyRolledBack, Reindexed: result.Reindexed,
	}
	if err != nil {
		return view, err
	}
	return view, nil
}

// Digest builds the morning digest. Write persists the Markdown form; without
// it the call is a dry run.
func (s *workspaceService) Digest(ctx context.Context, request localapi.DigestRequest) (localapi.DigestView, error) {
	digest, err := s.app.Digest(ctx, app.DigestOptions{ProjectID: request.ProjectID})
	if err != nil {
		return localapi.DigestView{}, err
	}
	view := localapi.DigestView{
		GeneratedAt: digest.GeneratedAt, Since: digest.Since, DryRun: !request.Write,
		Projects: make([]localapi.DigestProjectView, 0, len(digest.Projects)),
	}
	for _, project := range digest.Projects {
		view.Projects = append(view.Projects, digestProjectView(project))
	}
	if !request.Write {
		return view, nil
	}
	writtenTo, err := s.writeDigest(digest)
	if err != nil {
		return view, err
	}
	view.WrittenTo = writtenTo
	return view, nil
}

// AdoptLocalPaths records this machine as the owner of unowned local checkouts.
func (s *workspaceService) AdoptLocalPaths(ctx context.Context, apply bool) (localapi.AdoptReportView, error) {
	report, err := s.app.AdoptLocalPaths(ctx, apply)
	view := localapi.AdoptReportView{
		Machine: report.Machine, Applied: report.Applied, Stamped: report.Stamped,
		Owned: report.Owned, Missing: report.Missing,
		Projects: make([]localapi.ProjectOwnershipView, 0, len(report.Projects)),
	}
	for _, project := range report.Projects {
		entry := localapi.ProjectOwnershipView{ProjectID: project.ProjectID, Paths: make([]localapi.PathOwnershipView, 0, len(project.Paths))}
		for _, path := range project.Paths {
			entry.Paths = append(entry.Paths, localapi.PathOwnershipView{
				Path: path.Path, Machine: path.Machine, State: path.State,
			})
		}
		view.Projects = append(view.Projects, entry)
	}
	if err != nil {
		return view, err
	}
	return view, nil
}

// AddProjectPath registers one absolute checkout path on a project. Re-adding
// a registered path is an idempotent no-op; the path does not need to exist on
// this machine because a registry is shared between machines.
func (s *workspaceService) AddProjectPath(ctx context.Context, request localapi.AddProjectPathRequest) (localapi.Project, error) {
	path := strings.TrimSpace(request.Path)
	switch {
	case path == "":
		return localapi.Project{}, domain.NewValidationError("path", "is required")
	case !filepath.IsAbs(path):
		return localapi.Project{}, domain.NewValidationError("path", "must be an absolute checkout path")
	}
	path = filepath.Clean(path)
	resolved, err := s.app.ResolveSelector(ctx, request.ProjectID)
	if err != nil {
		return localapi.Project{}, err
	}
	project, err := s.app.GetProject(ctx, resolved.Project.ID)
	if err != nil {
		return localapi.Project{}, err
	}
	for _, existing := range project.Paths {
		if sameCheckoutPath(existing, path) {
			return projectView(project), nil
		}
	}
	paths := append(append([]string{}, project.Paths...), path)
	updated, err := s.app.SaveProject(ctx, app.ProjectInput{ID: project.ID, Paths: &paths})
	if err != nil {
		return localapi.Project{}, err
	}
	return projectView(updated), nil
}

// Journal reads one newest-first journal page. The service asks for one extra
// row so has_more is proven rather than guessed.
func (s *workspaceService) Journal(ctx context.Context, query localapi.JournalQuery) (localapi.JournalPage, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = 50
	}
	entries, err := s.app.ReadJournal(ctx, ports.JournalQuery{Project: query.ProjectID, Limit: limit + 1})
	if err != nil {
		return localapi.JournalPage{}, err
	}
	page := localapi.JournalPage{Limit: limit, Items: make([]localapi.JournalView, 0, limit)}
	if len(entries) > limit {
		entries = entries[:limit]
		page.HasMore = true
	}
	for _, entry := range entries {
		page.Items = append(page.Items, journalView(entry))
	}
	return page, nil
}

func mergePlanView(plan ports.MergePlan) localapi.MergePlan {
	view := localapi.MergePlan{
		Project: plan.ProjectID, Kind: plan.Kind, Survivor: mergeRecordView(plan.Survivor),
		Duplicates: make([]localapi.MergeRecordView, 0, len(plan.Duplicates)),
		Links:      make([]localapi.MergeLinkView, 0, len(plan.Links)),
		Conflicts:  append([]string{}, plan.Conflicts...),
		MergedFrom: append([]string{}, plan.MergedFrom...),
		Changed:    plan.Changed, Fingerprint: plan.Fingerprint,
	}
	for _, record := range plan.Duplicates {
		view.Duplicates = append(view.Duplicates, mergeRecordView(record))
	}
	for _, record := range plan.AlreadyMerged {
		view.AlreadyMerged = append(view.AlreadyMerged, mergeRecordView(record))
	}
	for _, link := range plan.Links {
		view.Links = append(view.Links, localapi.MergeLinkView{
			SourceKind: link.SourceKind, SourceID: link.SourceID, Field: link.Field,
			Ref: link.Ref, OldID: link.OldID, NewID: link.NewID,
		})
	}
	if view.Conflicts == nil {
		view.Conflicts = []string{}
	}
	return view
}

func mergeRecordView(record ports.MergeRecord) localapi.MergeRecordView {
	return localapi.MergeRecordView{
		Kind: record.Kind, ID: record.ID, Title: record.Title, State: record.State,
	}
}

func mergeFileViews(files []ports.MergeFile) []localapi.MergeFileView {
	views := make([]localapi.MergeFileView, 0, len(files))
	for _, file := range files {
		views = append(views, localapi.MergeFileView{Path: file.Path, Change: file.Change})
	}
	return views
}

func digestProjectView(project app.DigestProject) localapi.DigestProjectView {
	view := localapi.DigestProjectView{
		Project:           localapi.Project{ID: project.Project.ID, Name: project.Project.Name},
		Changes:           project.ActivityCount(),
		Notes:             len(project.Activity.Notes),
		Decisions:         len(project.Activity.Decisions),
		Attempts:          len(project.Activity.Attempts),
		Handoffs:          len(project.Activity.Handoffs),
		Commits:           project.Commits,
		ProposedDecisions: make([]localapi.DigestDecisionView, 0, len(project.ProposedDecisions)),
		OpenQuestions:     append([]string{}, project.OpenQuestions...),
		Blocked:           append([]string{}, project.Blocked...),
	}
	if project.LatestHandoff != nil {
		view.LatestHandoff = &localapi.DigestHandoffView{
			ID: project.LatestHandoff.ID, Summary: project.LatestHandoff.Summary,
			NextAction: project.LatestHandoff.NextAction, CreatedAt: project.LatestHandoff.CreatedAt,
		}
	}
	for _, decision := range project.ProposedDecisions {
		view.ProposedDecisions = append(view.ProposedDecisions, localapi.DigestDecisionView{
			ID: decision.ID, Title: decision.Title,
		})
	}
	if len(project.Findings) > 0 {
		view.Findings = make(map[string]int, len(project.Findings))
		for kind, count := range project.Findings {
			view.Findings[string(kind)] = count
		}
	}
	if view.OpenQuestions == nil {
		view.OpenQuestions = []string{}
	}
	if view.Blocked == nil {
		view.Blocked = []string{}
	}
	return view
}

func projectView(project domain.Project) localapi.Project {
	view := localapi.Project{ID: project.ID, Name: project.Name, Paths: append([]string{}, project.Paths...)}
	if len(project.PathProvenance) > 0 {
		view.PathProvenance = make(map[string]string, len(project.PathProvenance))
		for path, machine := range project.PathProvenance {
			view.PathProvenance[path] = machine
		}
	}
	return view
}

func sameCheckoutPath(left, right string) bool {
	normalize := func(path string) string {
		return strings.ToLower(filepath.ToSlash(filepath.Clean(strings.TrimSpace(path))))
	}
	return normalize(left) == normalize(right)
}

func trimmedDuplicates(duplicates []string) []string {
	out := make([]string, 0, len(duplicates))
	for _, duplicate := range duplicates {
		if trimmed := strings.TrimSpace(duplicate); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func projectInScope(project string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if candidate == project {
			return true
		}
	}
	return false
}

func encodeMergeToken(token mergeToken) (string, error) {
	raw, err := json.Marshal(token)
	if err != nil {
		return "", fmt.Errorf("agentcontrol: encode merge plan: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeMergeToken(value string) (mergeToken, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64<<10 {
		return mergeToken{}, domain.NewValidationError("plan_id", "is required and must be a plan token returned by this API")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return mergeToken{}, domain.NewValidationError("plan_id", "is not a plan token returned by this API")
	}
	var token mergeToken
	if err := json.Unmarshal(raw, &token); err != nil {
		return mergeToken{}, domain.NewValidationError("plan_id", "is not a plan token returned by this API")
	}
	if token.Version != mergeTokenVersion || strings.TrimSpace(token.Project) == "" {
		return mergeToken{}, domain.NewValidationError("plan_id", "is not a plan token returned by this API")
	}
	return token, nil
}

// decodeRollbackReference accepts the manifest token an apply returned or a
// plain "merges/<name>.json" reference.
func decodeRollbackReference(value string) (manifest, project string, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", domain.NewValidationError("plan_id", "is required")
	}
	if token, decodeErr := decodeMergeToken(value); decodeErr == nil {
		if token.Kind != mergeTokenManifest || strings.TrimSpace(token.Manifest) == "" {
			return "", "", domain.NewValidationError("plan_id", "is a merge plan, not an applied merge; apply it before rolling it back")
		}
		if !validManifestRef(token.Manifest) {
			return "", "", domain.NewValidationError("plan_id", "names an invalid merge manifest")
		}
		return token.Manifest, token.Project, nil
	}
	if !validManifestRef(value) {
		return "", "", domain.NewValidationError("plan_id", "is not a plan token or merge manifest returned by this API")
	}
	return value, "", nil
}

// validManifestRef bounds a manifest reference to the merge directory: a
// relative "merges/<name>.json" path with no separators inside the name.
func validManifestRef(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "merges/") || strings.Contains(value, `\`) {
		return false
	}
	name := strings.TrimPrefix(value, "merges/")
	if !strings.HasSuffix(name, ".json") {
		return false
	}
	name = strings.TrimSuffix(name, ".json")
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/:")
}

// writeDigest saves the digest as <data_dir>/digests/YYYY-MM-DD.md, one file
// per UTC day, matching the CLI convention.
func (s *workspaceService) writeDigest(digest app.Digest) (string, error) {
	if strings.TrimSpace(s.dataDir) == "" {
		return "", fmt.Errorf("%w: digest write needs a local data directory", domain.ErrUnsupported)
	}
	dir := filepath.Join(s.dataDir, "digests")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	generated := digest.GeneratedAt
	if generated.IsZero() {
		generated = s.now().UTC()
	}
	path := filepath.Join(dir, generated.UTC().Format("2006-01-02")+".md")
	content := renderDigestMarkdown(digest)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join("digests", filepath.Base(path))), nil
}

// renderDigestMarkdown is the persisted digest. It is deliberately small: the
// desktop renders the JSON view, and this file is the same morning summary the
// CLI can leave for a human.
func renderDigestMarkdown(digest app.Digest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "brain digest — since %s\n", digest.Since.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "generated %s\n", digest.GeneratedAt.UTC().Format(time.RFC3339))
	if len(digest.Projects) == 0 {
		b.WriteString("\nnothing to report\n")
		return b.String()
	}
	for _, project := range digest.Projects {
		fmt.Fprintf(&b, "\n## %s — %d change(s)\n", project.Project.ID, project.ActivityCount())
		fmt.Fprintf(&b, "notes %d, decisions %d, attempts %d, handoffs %d, commits %d\n",
			len(project.Activity.Notes), len(project.Activity.Decisions),
			len(project.Activity.Attempts), len(project.Activity.Handoffs), project.Commits)
		if project.LatestHandoff != nil {
			fmt.Fprintf(&b, "latest handoff: %s -> next: %s\n",
				boundedLine(project.LatestHandoff.Summary), boundedLine(project.LatestHandoff.NextAction))
		}
		for _, decision := range project.ProposedDecisions {
			fmt.Fprintf(&b, "- proposed decision %s %s\n", decision.ID, boundedLine(decision.Title))
		}
		for _, question := range project.OpenQuestions {
			fmt.Fprintf(&b, "- open question: %s\n", boundedLine(question))
		}
		for _, blocked := range project.Blocked {
			fmt.Fprintf(&b, "- blocked: %s\n", boundedLine(blocked))
		}
		if count := project.FindingCount(); count > 0 {
			fmt.Fprintf(&b, "- review findings: %d\n", count)
		}
	}
	return b.String()
}

func boundedLine(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if runes := []rune(value); len(runes) > 300 {
		value = string(runes[:300])
	}
	return value
}
