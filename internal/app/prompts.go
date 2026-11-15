package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// DefaultPromptListLimit bounds `brain prompt list` when no limit is passed.
const DefaultPromptListLimit = 20

// PromptInput is one prompt to save in the local-only prompt zone.
type PromptInput struct {
	ProjectID string
	Text      string
	Session   string
}

// PromptListOptions selects prompts to list. An empty ProjectID lists every
// registered project.
type PromptListOptions struct {
	ProjectID string
	// Limit bounds the result; <= 0 uses DefaultPromptListLimit.
	Limit int
}

// promptStore returns the local prompt store or reports that the current store
// has no prompt zone (server mode).
func (a *App) promptStore() (ports.PromptStore, error) {
	store, ok := a.deps.Store.(ports.PromptStore)
	if !ok {
		return nil, fmt.Errorf("%w: the prompt zone is local-only and unavailable for this store", domain.ErrUnsupported)
	}
	return store, nil
}

// requirePromptsEnabled refuses prompt work while the opt-in is off. The
// message names the configuration key, so enabling it is one edit.
func (a *App) requirePromptsEnabled() error {
	if a.cfg.Prompts.Enabled {
		return nil
	}
	return domain.NewValidationError("config.prompts.enabled",
		"the prompt zone is disabled; set prompts.enabled: true in brain.yaml to store local-only prompts")
}

// SavePrompt stores one prompt. Prompts are machine-local: they are written
// under projects/<id>/prompts/, ignored by git and never indexed.
func (a *App) SavePrompt(ctx context.Context, input PromptInput) (domain.Prompt, error) {
	if err := a.requirePromptsEnabled(); err != nil {
		return domain.Prompt{}, err
	}
	if err := a.requireStore(); err != nil {
		return domain.Prompt{}, err
	}
	store, err := a.promptStore()
	if err != nil {
		return domain.Prompt{}, err
	}
	project, err := a.ensureProject(ctx, input.ProjectID, false)
	if err != nil {
		return domain.Prompt{}, err
	}
	text := strings.TrimSpace(input.Text)
	if err := domain.ValidateRequired("prompt.text", text); err != nil {
		return domain.Prompt{}, err
	}
	prompt := domain.Prompt{
		ProjectID: project.ID,
		At:        a.now(),
		Session:   strings.TrimSpace(input.Session),
		Text:      text,
	}
	if err := store.AppendPrompt(ctx, prompt); err != nil {
		return domain.Prompt{}, err
	}
	return prompt, nil
}

// ListPrompts returns prompts newest first. It refuses while the zone is
// disabled: a disabled feature should not answer reads that imply it stores
// data.
func (a *App) ListPrompts(ctx context.Context, opts PromptListOptions) ([]domain.Prompt, error) {
	if err := a.requirePromptsEnabled(); err != nil {
		return nil, err
	}
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	store, err := a.promptStore()
	if err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultPromptListLimit
	}
	if strings.TrimSpace(opts.ProjectID) != "" {
		project, err := a.ensureProject(ctx, opts.ProjectID, false)
		if err != nil {
			return nil, err
		}
		return store.ListPrompts(ctx, project.ID, limit)
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	prompts := make([]domain.Prompt, 0, limit)
	for _, project := range projects {
		items, err := store.ListPrompts(ctx, project.ID, limit)
		if err != nil {
			return nil, err
		}
		prompts = append(prompts, items...)
	}
	sort.SliceStable(prompts, func(i, j int) bool {
		if !prompts[i].At.Equal(prompts[j].At) {
			return prompts[i].At.After(prompts[j].At)
		}
		if prompts[i].ProjectID != prompts[j].ProjectID {
			return prompts[i].ProjectID < prompts[j].ProjectID
		}
		return prompts[i].Text < prompts[j].Text
	})
	if len(prompts) > limit {
		prompts = prompts[:limit]
	}
	return prompts, nil
}

// promptRetention is the effective retention window: a positive duration, or
// zero when prompts are kept forever.
func (a *App) promptRetention() time.Duration {
	days := a.cfg.Prompts.EffectiveRetentionDays()
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

// doctorPrompts reports whether the prompt zone is enabled, that it is local
// only, and how many prompts are past the retention window. Nothing is
// deleted: retention is a report, and cleanup is a deliberate owner action.
func (a *App) doctorPrompts(ctx context.Context) DoctorCheck {
	if a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: "prompts", Status: doctorOK,
			Message: "not available in server mode: the prompt zone is machine-local"}
	}
	if !a.cfg.Prompts.Enabled {
		return DoctorCheck{Name: "prompts", Status: doctorOK,
			Message: "disabled: the local-only prompt zone is opt-in (set prompts.enabled: true)"}
	}
	store, ok := a.deps.Store.(ports.PromptStore)
	if !ok || a.deps.Store == nil {
		return DoctorCheck{Name: "prompts", Status: doctorWarn,
			Message: "enabled in configuration, but this store has no prompt zone"}
	}
	message := "enabled: local-only, never committed or indexed"
	retention := a.promptRetention()
	if retention <= 0 {
		message += "; retention: keep"
		return DoctorCheck{Name: "prompts", Status: doctorOK, Message: message}
	}
	message += fmt.Sprintf("; retention: %d days", int(retention.Hours()/24))
	cutoff := a.now().Add(-retention)
	old := 0
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return DoctorCheck{Name: "prompts", Status: doctorWarn, Message: message + "; count unavailable: " + err.Error()}
	}
	for _, project := range projects {
		prompts, err := store.ListPrompts(ctx, project.ID, 10000)
		if err != nil {
			return DoctorCheck{Name: "prompts", Status: doctorWarn, Message: message + "; count unavailable: " + err.Error()}
		}
		for _, prompt := range prompts {
			if prompt.At.Before(cutoff) {
				old++
			}
		}
	}
	if old > 0 {
		message += fmt.Sprintf("; %d prompt(s) older than the window (not deleted automatically)", old)
	}
	return DoctorCheck{Name: "prompts", Status: doctorOK, Message: message}
}
