package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// IndexOptions controls a local index refresh.
type IndexOptions struct {
	// Rebuild records whether the caller reset the SQLite database first.
	Rebuild bool
}

// IndexResult reports how many records were indexed.
type IndexResult struct {
	Projects  int
	States    int
	Notes     int
	Decisions int
	Attempts  int
	Handoffs  int
	Dumps     int
	Documents int
	Rebuild   bool
}

// Reindex rebuilds the local search projection from Markdown files. The caller
// is responsible for deleting an existing database when Rebuild is true; this
// use case only upserts documents.
func (a *App) Reindex(ctx context.Context, opts IndexOptions) (IndexResult, error) {
	if a.cfg.Mode == config.ModeServer {
		return IndexResult{}, fmt.Errorf("%w: index is local-only in server mode", domain.ErrUnsupported)
	}
	if a.deps.Store == nil {
		return IndexResult{}, fmt.Errorf("%w: brain data store is not available; check data_dir or run brain init", domain.ErrUnsupported)
	}
	if a.deps.Index == nil {
		return IndexResult{}, fmt.Errorf("%w: local search index is not available", domain.ErrUnsupported)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	result := IndexResult{Rebuild: opts.Rebuild}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return result, err
	}
	result.Projects = len(projects)
	for _, project := range projects {
		state, stateErr := a.deps.Store.GetState(ctx, project.ID)
		switch {
		case stateErr == nil:
			if err := a.deps.Index.Upsert(ctx, stateDocument(project, state)); err != nil {
				return result, fmt.Errorf("index state for %s: %w", project.ID, err)
			}
			result.States++
			result.Documents++
		case errors.Is(stateErr, domain.ErrNotFound):
		default:
			return result, stateErr
		}

		notes, err := a.listNotes(ctx, project.ID)
		if err != nil {
			return result, err
		}
		for _, note := range notes {
			if err := a.deps.Index.Upsert(ctx, noteDocument(note)); err != nil {
				return result, fmt.Errorf("index note %s: %w", note.ID, err)
			}
			result.Notes++
			result.Documents++
		}

		decisions, err := a.listDecisions(ctx, project.ID)
		if err != nil {
			return result, err
		}
		for _, decision := range decisions {
			if err := a.deps.Index.Upsert(ctx, decisionDocument(decision)); err != nil {
				return result, fmt.Errorf("index decision %s: %w", decision.ID, err)
			}
			result.Decisions++
			result.Documents++
		}

		attempts, err := a.listAttempts(ctx, project.ID)
		if err != nil {
			return result, err
		}
		for _, attempt := range attempts {
			if err := a.deps.Index.Upsert(ctx, attemptDocument(attempt)); err != nil {
				return result, fmt.Errorf("index attempt %s: %w", attempt.ID, err)
			}
			result.Attempts++
			result.Documents++
		}

		handoffs, err := a.listHandoffs(ctx, project.ID)
		if err != nil {
			return result, err
		}
		for _, handoff := range handoffs {
			if err := a.deps.Index.Upsert(ctx, handoffDocument(handoff)); err != nil {
				return result, fmt.Errorf("index handoff %s: %w", handoff.ID, err)
			}
			result.Handoffs++
			result.Documents++
		}
	}
	dumps, err := a.indexDumps(ctx, projects)
	result.Dumps = dumps
	result.Documents += dumps
	if err != nil {
		return result, err
	}
	a.clearIndexDirty()
	return result, nil
}

// IndexDumps refreshes the connector dumps in the search index, for example
// after a connector run. It returns the number of indexed dump files.
func (a *App) IndexDumps(ctx context.Context) (int, error) {
	if a.deps.Store == nil || a.deps.Index == nil {
		return 0, nil
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	count, err := a.indexDumps(ctx, projects)
	if err != nil {
		a.markIndexDirty()
	}
	return count, err
}

func (a *App) indexDumps(ctx context.Context, projects []domain.Project) (int, error) {
	lister, ok := a.deps.Store.(ports.DumpLister)
	if !ok {
		return 0, nil
	}
	dumps, err := lister.ListDumps(ctx)
	if err != nil {
		return 0, fmt.Errorf("list connector dumps: %w", err)
	}
	byRepo := projectsByRepo(projects)
	for i, dump := range dumps {
		if err := a.deps.Index.Upsert(ctx, dumpDocument(dump, byRepo)); err != nil {
			return i, fmt.Errorf("index dump %s: %w", dump.Path, err)
		}
	}
	return len(dumps), nil
}

// dumpDocument indexes a dump under the registered project whose repository
// it mirrors ("github:owner/repo" matches a repo_url ending in owner/repo),
// else under the dump's own project id ("mail:account").
func dumpDocument(dump ports.Dump, byRepo map[string]string) ports.Document {
	projectID := dump.ProjectID
	if _, repo, ok := strings.Cut(dump.ProjectID, ":"); ok {
		if id, found := byRepo[strings.ToLower(repo)]; found {
			projectID = id
		}
	}
	return ports.Document{
		ID:        "dump:" + dump.Path,
		ProjectID: projectID,
		Kind:      KindDump,
		Title:     dump.Title,
		Body:      dump.Body,
		UpdatedAt: dump.UpdatedAt,
	}
}

// projectsByRepo maps "owner/repo" (lowercase, from repo_url) to project id.
func projectsByRepo(projects []domain.Project) map[string]string {
	byRepo := make(map[string]string, len(projects))
	for _, project := range projects {
		if repo := repoPath(project.RepoURL); repo != "" {
			byRepo[repo] = project.ID
		}
	}
	return byRepo
}

// repoPath extracts "owner/repo" from https or ssh remote URLs.
func repoPath(remote string) string {
	remote = strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	if remote == "" {
		return ""
	}
	if _, rest, ok := strings.Cut(remote, "://"); ok {
		remote = rest
		if _, path, found := strings.Cut(remote, "/"); found {
			return strings.ToLower(strings.Trim(path, "/"))
		}
		return ""
	}
	if _, path, ok := strings.Cut(remote, ":"); ok { // git@host:owner/repo
		return strings.ToLower(strings.Trim(path, "/"))
	}
	return ""
}

func stateDocument(project domain.Project, state domain.State) ports.Document {
	title := strings.TrimSpace(project.Name)
	if title == "" {
		title = project.ID
	}
	return ports.Document{
		ID:        "state:" + project.ID,
		ProjectID: project.ID,
		Kind:      "state",
		Title:     title + " state",
		Body: joinText(
			state.Goal,
			strings.Join(state.Active, "\n"),
			strings.Join(state.Blocked, "\n"),
			state.NextAction,
			strings.Join(state.OpenQuestions, "\n"),
		),
		UpdatedAt: updatedOrCreated(state.UpdatedAt, project.UpdatedAt),
	}
}
