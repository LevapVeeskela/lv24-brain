package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// jsonPromote is `brain promote --json`.
type jsonPromote struct {
	ProposalID string             `json:"proposal_id"`
	Status     string             `json:"status"`
	Already    bool               `json:"already"`
	Record     *jsonPromoteRecord `json:"record"`
}

// jsonPromoteRecord identifies the curated record a promotion created.
type jsonPromoteRecord struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title"`
}

func toJSONPromote(result app.PromoteResult) jsonPromote {
	out := jsonPromote{
		ProposalID: result.Proposal.ID,
		Status:     string(result.Proposal.StatusOrPending()),
		Already:    result.Already,
	}
	if result.Record.Kind != "" || result.Record.ID != "" || result.Record.Title != "" {
		out.Record = &jsonPromoteRecord{
			Kind:  string(result.Record.Kind),
			ID:    result.Record.ID,
			Title: result.Record.Title,
		}
	}
	return out
}

// runPromote promotes or dismisses one proposal. Exit codes: 0 success or an
// idempotent rerun, 1 not found/terminal/stale/write failure, 2 bad flags or a
// missing --yes for a local-only source.
func runPromote(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("promote", stderr)
	project := fs.String("project", "", "project id or alias (default: search registered projects)")
	as := fs.String("as", "", "record kind: decision, attempt or note (default: the proposal kind)")
	status := fs.String("status", "", "decision status: proposed or accepted (default: proposed)")
	title := fs.String("title", "", "override the record title")
	dismiss := fs.Bool("dismiss", false, "dismiss the proposal instead of promoting it")
	force := fs.Bool("force", false, "promote a stale or orphaned proposal anyway")
	yes := fs.Bool("yes", false, "confirm promotion from a local-only source")
	allowProtected := fs.Bool("allow-protected", false, "write even though limits.protected_projects or limits.protected_globs protect the project")
	allowDuplicate := fs.Bool("allow-duplicate", false, "promote even though an existing record holds the same normalized text")
	dryRun := fs.Bool("dry-run", false, "show what would happen without writing")
	snooze := fs.String("snooze", "", "hide the proposal until a time: 7d, 2026-12-01 or an RFC3339 time")
	unsnooze := fs.Bool("unsnooze", false, "show a snoozed proposal again now")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "brain promote: expected exactly one proposal id")
		return 2
	}
	proposalID := strings.TrimSpace(positional[0])
	if proposalID == "" {
		fmt.Fprintln(stderr, "brain promote: the proposal id is required")
		return 2
	}
	if kind := strings.TrimSpace(*as); kind != "" && !domain.ProposalKind(kind).Valid() {
		fmt.Fprintf(stderr, "brain promote: unknown --as %q (use decision, attempt or note)\n", kind)
		return 2
	}
	switch decisionStatus := domain.DecisionStatus(strings.TrimSpace(*status)); decisionStatus {
	case "", domain.DecisionProposed, domain.DecisionAccepted:
	default:
		fmt.Fprintf(stderr, "brain promote: unknown --status %q (use proposed or accepted)\n", decisionStatus)
		return 2
	}
	if *dismiss && (strings.TrimSpace(*as) != "" || strings.TrimSpace(*status) != "" || strings.TrimSpace(*title) != "") {
		fmt.Fprintln(stderr, "brain promote: --dismiss cannot be combined with --as, --status or --title")
		return 2
	}
	snoozing := *snooze != "" || *unsnooze
	if *snooze != "" && *unsnooze {
		fmt.Fprintln(stderr, "brain promote: --snooze and --unsnooze cannot be combined")
		return 2
	}
	if snoozing && (*dismiss || strings.TrimSpace(*as) != "" || strings.TrimSpace(*status) != "" || strings.TrimSpace(*title) != "" || *dryRun) {
		fmt.Fprintln(stderr, "brain promote: --snooze/--unsnooze cannot be combined with --dismiss, --as, --status, --title or --dry-run")
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "promote", err)
	}
	if cfg.Mode == config.ModeServer {
		fmt.Fprintln(stderr, "brain promote: needs local or git mode; run it on the machine that owns the data dir")
		return 2
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "promote", err)
	}
	defer func() { _ = runtime.Close() }()

	if snoozing {
		var until time.Time
		if *snooze != "" {
			parsed, err := domain.ParseSnoozeUntil(*snooze, time.Now())
			if err != nil {
				fmt.Fprintf(stderr, "brain promote: %v\n", err)
				return 2
			}
			until = parsed
		}
		result, err := runtime.App.SnoozeProposal(context.Background(), app.SnoozeOptions{
			ProposalID: proposalID,
			ProjectID:  strings.TrimSpace(*project),
			Until:      until,
		})
		if err != nil {
			return promoteError(stderr, err)
		}
		if *asJSON {
			return writeJSONReport(stdout, stderr, "promote", toJSONSnooze(result))
		}
		printSnoozeResult(stdout, result)
		return 0
	}

	result, err := runtime.App.Promote(context.Background(), app.PromoteOptions{
		ProposalID:     proposalID,
		ProjectID:      strings.TrimSpace(*project),
		As:             domain.ProposalKind(strings.TrimSpace(*as)),
		Status:         domain.DecisionStatus(strings.TrimSpace(*status)),
		Title:          *title,
		Dismiss:        *dismiss,
		Force:          *force,
		Yes:            *yes,
		AllowProtected: *allowProtected,
		AllowDuplicate: *allowDuplicate,
		DryRun:         *dryRun,
	})
	if err != nil {
		return promoteError(stderr, err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "promote", toJSONPromote(result))
	}
	printPromoteResult(stdout, result)
	return 0
}

// jsonSnooze is `brain promote --snooze/--unsnooze --json`.
type jsonSnooze struct {
	ProposalID   string  `json:"proposal_id"`
	Status       string  `json:"status"`
	Snoozed      bool    `json:"snoozed"`
	SnoozedUntil *string `json:"snoozed_until"`
	UpdatedAt    *string `json:"updated_at"`
}

func toJSONSnooze(result app.SnoozeResult) jsonSnooze {
	return jsonSnooze{
		ProposalID:   result.Proposal.ID,
		Status:       string(result.Proposal.StatusOrPending()),
		Snoozed:      result.Snoozed,
		SnoozedUntil: jsonTime(result.Proposal.SnoozedUntil),
		UpdatedAt:    jsonTime(result.Proposal.UpdatedAt),
	}
}

func printSnoozeResult(w io.Writer, result app.SnoozeResult) {
	if result.Proposal.SnoozedUntil.IsZero() {
		fmt.Fprintf(w, "unsnoozed: %s\n", result.Proposal.ID)
		return
	}
	fmt.Fprintf(w, "snoozed: %s until %s\n", result.Proposal.ID,
		result.Proposal.SnoozedUntil.UTC().Format(time.RFC3339))
}

// promoteError maps a use-case error to the documented promote exit code: a
// validation error is a bad flag or value (2), everything else a state failure
// (1).
func promoteError(stderr io.Writer, err error) int {
	if errors.Is(err, domain.ErrValidation) {
		fmt.Fprintf(stderr, "brain promote: %v\n", err)
		return 2
	}
	return reportError(stderr, "promote", err)
}

func printPromoteResult(w io.Writer, result app.PromoteResult) {
	switch {
	case result.DryRun && result.Dismissed:
		fmt.Fprintf(w, "dry-run: would dismiss %s\n", result.Proposal.ID)
	case result.DryRun:
		fmt.Fprintf(w, "dry-run: would promote %s as %s %q (%s)\n",
			result.Proposal.ID, result.Record.Kind, result.Record.Title, result.Record.ID)
	case result.Dismissed && result.Already:
		fmt.Fprintf(w, "already dismissed: %s\n", result.Proposal.ID)
	case result.Dismissed:
		fmt.Fprintf(w, "dismissed: %s\n", result.Proposal.ID)
	case result.Already:
		fmt.Fprintf(w, "already promoted: %s -> %s %s\n", result.Proposal.ID, result.Record.Kind, result.Record.ID)
	default:
		fmt.Fprintf(w, "promoted: %s -> %s %s\n", result.Proposal.ID, result.Record.Kind, result.Record.ID)
		fmt.Fprintf(w, "title: %s\n", result.Record.Title)
	}
}
