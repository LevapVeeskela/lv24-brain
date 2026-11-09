package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// connectorPlan is one local connector run.
type connectorPlan struct {
	label string
	opts  bootstrap.ConnectorOptions
	mail  *bootstrap.MailAccount
}

// planLocalConnectors expands a request into runs with data-derived targets:
// GitHub repositories from registered repo URLs, GitLab projects from repo
// URLs and checkout remotes, and IMAP accounts from connectors/mail.json.
// --all runs github, gitlab, and every mail account; --account selects one
// mail account.
func planLocalConnectors(ctx context.Context, cfg config.BrainConfig, name, project, account string, all bool, limit int) ([]connectorPlan, error) {
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return nil, err
	}
	projects, err := runtime.App.ListProjects(ctx)
	_ = runtime.Close()
	if err != nil {
		return nil, err
	}
	accounts, err := bootstrap.LoadMailAccounts(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	base := bootstrap.ConnectorOptions{Project: project, Limit: limit}

	var plans []connectorPlan
	addGit := func(kind string) {
		opts := base
		opts.Name = kind
		if kind == "github" {
			opts.Repos = bootstrap.GitHubRepos(projects)
		} else {
			opts.GitLabProjects = bootstrap.GitLabProjects(ctx, projects)
		}
		plans = append(plans, connectorPlan{label: kind, opts: opts})
	}
	addMail := func(selected MailSelector) error {
		matched := false
		for _, mail := range accounts {
			if !selected(mail) {
				continue
			}
			matched = true
			opts := base
			opts.Name = "imap"
			opts.Project = ""
			opts.IMAP = mail.IMAPAccount()
			plan := connectorPlan{label: "imap/" + mail.ID, opts: opts}
			if strings.TrimSpace(os.Getenv("BRAIN_IMAP_ACCOUNT")) == "" {
				plan.mail = &mail
			}
			plans = append(plans, plan)
		}
		if !matched {
			return domain.NewValidationError("imap.account", fmt.Sprintf("no matching account in %s", bootstrap.MailConfigPath))
		}
		return nil
	}

	switch {
	case all:
		addGit("github")
		addGit("gitlab")
		if len(accounts) > 0 {
			if err := addMail(func(bootstrap.MailAccount) bool { return true }); err != nil {
				return nil, err
			}
		}
	case name == "imap" && account != "":
		if err := addMail(func(m bootstrap.MailAccount) bool { return strings.EqualFold(m.ID, account) }); err != nil {
			return nil, err
		}
	case name == "imap":
		// Environment-configured single account (BRAIN_IMAP_*), as before.
		plans = append(plans, connectorPlan{label: "imap", opts: bootstrap.ConnectorOptions{Name: "imap", Limit: limit}})
	default:
		addGit(name)
	}
	return plans, nil
}

// connectorRunTimeout bounds one local connector run.
const connectorRunTimeout = 5 * time.Minute

// MailSelector filters mail.json accounts.
type MailSelector func(bootstrap.MailAccount) bool

// runLocalConnectors executes plans in order; one failing run does not stop
// the others. It reports whether every run succeeded.
func runLocalConnectors(ctx context.Context, cfg config.BrainConfig, plans []connectorPlan, since *time.Time, stdout, stderr io.Writer) bool {
	runStore, err := bootstrap.OpenRunStore(cfg)
	if err != nil {
		reportError(stderr, "connector run", err)
		return false
	}
	ok := true
	for i, plan := range plans {
		if len(plans) > 1 {
			if i > 0 {
				fmt.Fprintln(stdout)
			}
			fmt.Fprintf(stdout, "== %s\n", plan.label)
		}
		connector, err := bootstrap.BuildConnector(cfg, plan.opts, bootstrap.Options{})
		if err != nil {
			reportError(stderr, "connector run "+plan.label, err)
			ok = false
			continue
		}
		if runStore != nil {
			connector = connectors.Recording{Inner: connector, Runs: runStore}
		}
		runOptions := ports.ConnectorOptions{Since: since, Limit: plan.opts.Limit}
		if plan.mail != nil {
			statePath := strings.TrimSpace(os.Getenv("BRAIN_IMAP_STATE"))
			if statePath == "" {
				statePath = filepath.Join(cfg.DataDir, "connectors", "mail-state.json")
			}
			runOptions.Since, err = plan.mail.ImportSince(since, time.Now(), statePath)
			if err != nil {
				reportError(stderr, "connector run "+plan.label, err)
				ok = false
				continue
			}
		}
		if plan.opts.Name != "imap" && strings.TrimSpace(plan.opts.Project) != "" {
			runOptions.Projects = []string{strings.TrimSpace(plan.opts.Project)}
		}
		// Bound each run so one stuck mailbox cannot block the rest.
		runCtx, cancel := context.WithTimeout(ctx, connectorRunTimeout)
		run, err := connector.Run(runCtx, runOptions)
		cancel()
		printConnectorRun(stdout, run)
		if err != nil {
			reportError(stderr, "connector run "+plan.label, err)
			ok = false
			continue
		}
		if run.Status != domain.ConnectorSuccess {
			ok = false
		}
	}
	refreshDumpIndex(ctx, cfg, stderr)
	return ok
}

// refreshDumpIndex makes the new dumps searchable. It never fails the run:
// on error the index is marked dirty and the next search repairs it.
func refreshDumpIndex(ctx context.Context, cfg config.BrainConfig, stderr io.Writer) {
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "brain connector run: dumps not indexed: %v\n", err)
		return
	}
	defer func() { _ = runtime.Close() }()
	if _, err := runtime.App.IndexDumps(ctx); err != nil {
		fmt.Fprintf(stderr, "brain connector run: dumps not indexed (the next search retries): %v\n", err)
	}
}
