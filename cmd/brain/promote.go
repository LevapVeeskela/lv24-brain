package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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
	dryRun := fs.Bool("dry-run", false, "show what would happen without writing")
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

	result, err := runtime.App.Promote(context.Background(), app.PromoteOptions{
		ProposalID: proposalID,
		ProjectID:  strings.TrimSpace(*project),
		As:         domain.ProposalKind(strings.TrimSpace(*as)),
		Status:     domain.DecisionStatus(strings.TrimSpace(*status)),
		Title:      *title,
		Dismiss:    *dismiss,
		Force:      *force,
		Yes:        *yes,
		DryRun:     *dryRun,
	})
	if err != nil {
		if errors.Is(err, domain.ErrValidation) {
			fmt.Fprintf(stderr, "brain promote: %v\n", err)
			return 2
		}
		return reportError(stderr, "promote", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "promote", toJSONPromote(result))
	}
	printPromoteResult(stdout, result)
	return 0
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
