package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpclient"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func printProjectMembersUsage(w io.Writer) {
	fmt.Fprint(w, `Usage (server mode, admin):
  brain project members <id> [--json]
  brain project members <id> add <login> --role viewer|member|maintainer
  brain project members <id> remove <login>
  brain project members <id> restrict|open

A project without members is open to every user per their global role. Adding
the first member restricts it: only members and admins can access it, and the
project role is capped by the global role. Removing the last member keeps the
project restricted (admin-only); "open" reopens a project without members.
`)
}

// serverAdminClient loads the config and requires server mode.
func serverAdminClient(cfgPath, command string, stderr io.Writer) (*httpclient.Client, bool) {
	cfg, err := loadRequiredConfig(cfgPath)
	if err != nil {
		reportError(stderr, command, err)
		return nil, false
	}
	if cfg.Mode != config.ModeServer {
		fmt.Fprintf(stderr, "brain %s: server mode is required (set mode: server in brain.yaml)\n", command)
		return nil, false
	}
	client, err := bootstrap.NewServerClient(cfg)
	if err != nil {
		reportError(stderr, command, err)
		return nil, false
	}
	return client, true
}

func runProjectMembers(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project members", stderr)
	role := fs.String("role", "", "viewer, member or maintainer (add)")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) == 0 || len(positional) > 3 {
		printProjectMembersUsage(stderr)
		return 2
	}
	projectID := strings.TrimSpace(positional[0])
	action := ""
	if len(positional) > 1 {
		action = positional[1]
	}
	if (action == "add" || action == "remove") != (len(positional) == 3) {
		printProjectMembersUsage(stderr)
		return 2
	}
	client, ok := serverAdminClient(*cfgPath, "project members", stderr)
	if !ok {
		return 1
	}
	ctx := context.Background()
	switch action {
	case "":
		access, err := client.ListProjectMembers(ctx, projectID)
		if err != nil {
			return reportError(stderr, "project members", err)
		}
		return printProjectAccess(stdout, stderr, access, *asJSON)
	case "add":
		parsed, err := domain.ParseProjectRole(*role)
		if err != nil {
			fmt.Fprintln(stderr, "brain project members: --role must be viewer, member or maintainer")
			return 2
		}
		member, err := client.PutProjectMember(ctx, projectID, positional[2], parsed)
		if err != nil {
			return reportError(stderr, "project members", err)
		}
		fmt.Fprintf(stdout, "%s is %s of %s\n", member.Login, member.Role, projectID)
		return 0
	case "remove":
		if err := client.RemoveProjectMember(ctx, projectID, positional[2]); err != nil {
			return reportError(stderr, "project members", err)
		}
		fmt.Fprintf(stdout, "removed %s from %s\n", positional[2], projectID)
		return 0
	case "restrict", "open":
		access, err := client.SetProjectRestricted(ctx, projectID, action == "restrict")
		if err != nil {
			return reportError(stderr, "project members", err)
		}
		return printProjectAccess(stdout, stderr, access, *asJSON)
	default:
		fmt.Fprintf(stderr, "brain project members: unknown action %q\n\n", action)
		printProjectMembersUsage(stderr)
		return 2
	}
}

func printProjectAccess(stdout, stderr io.Writer, access httpclient.ProjectAccess, asJSON bool) int {
	if asJSON {
		if err := encodeJSON(stdout, access); err != nil {
			return reportError(stderr, "project members", err)
		}
		return 0
	}
	state := "open (no members; global roles apply)"
	if access.Restricted {
		state = "restricted (members and admins only)"
	}
	fmt.Fprintf(stdout, "%s: %s\n", access.ProjectID, state)
	for _, member := range access.Members {
		fmt.Fprintf(stdout, "  %-30s %s\n", member.Login, member.Role)
	}
	return 0
}

func printTokenUsage(w io.Writer) {
	fmt.Fprint(w, `Usage (server mode):
  brain token create --name <name> [--scope read|write|admin]... [--project <id>]...
                     [--role admin|member|viewer] [--expires-in-days N] [--json]

Without --scope the token carries its full role (unscoped). read allows GET
requests, write allows knowledge/state/project writes, admin allows admin
endpoints; write and admin imply read. --project limits the token to those
projects. The raw token is printed once.
`)
}

type repeatedFlag []string

func (r *repeatedFlag) String() string     { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(v string) error { *r = append(*r, v); return nil }

var _ flag.Value = (*repeatedFlag)(nil)

func runToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printTokenUsage(stderr)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		printTokenUsage(stdout)
		return 0
	case "create":
		return runTokenCreate(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "brain token: unknown subcommand %q\n\n", args[0])
		printTokenUsage(stderr)
		return 2
	}
}

func runTokenCreate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("token create", stderr)
	name := fs.String("name", "", "token name")
	role := fs.String("role", "", "token role (defaults to your role)")
	days := fs.Int("expires-in-days", 0, "expire after N days (0 = never)")
	asJSON := fs.Bool("json", false, "print JSON")
	var scopes, projects repeatedFlag
	fs.Var(&scopes, "scope", "read, write or admin (repeatable)")
	fs.Var(&projects, "project", "allowed project id (repeatable)")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || strings.TrimSpace(*name) == "" || *days < 0 {
		printTokenUsage(stderr)
		return 2
	}
	if _, err := domain.NormalizeTokenScopes(scopes); err != nil {
		fmt.Fprintf(stderr, "brain token create: %v\n", err)
		return 2
	}
	client, ok := serverAdminClient(*cfgPath, "token create", stderr)
	if !ok {
		return 1
	}
	req := httpclient.APITokenRequest{
		Name: strings.TrimSpace(*name), Role: strings.TrimSpace(*role),
		Scopes: scopes, ProjectIDs: projects,
	}
	if *days > 0 {
		req.ExpiresInDays = days
	}
	raw, record, err := client.CreateAPIToken(context.Background(), req)
	if err != nil {
		return reportError(stderr, "token create", err)
	}
	if *asJSON {
		if err := encodeJSON(stdout, map[string]any{"token": raw, "apiToken": record}); err != nil {
			return reportError(stderr, "token create", err)
		}
		return 0
	}
	scopeText := "unscoped"
	if len(record.Scopes) > 0 {
		scopeText = strings.Join(record.Scopes, ",")
	}
	projectText := "all projects"
	if len(record.ProjectIDs) > 0 {
		projectText = strings.Join(record.ProjectIDs, ",")
	}
	fmt.Fprintf(stdout, "token %s (%s, %s, %s)\n%s\nStore it now; it is not shown again.\n",
		record.ID, record.Role, scopeText, projectText, raw)
	return 0
}
