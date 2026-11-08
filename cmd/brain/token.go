package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpclient"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func printTokenUsage(w io.Writer) {
	fmt.Fprint(w, `Usage (server mode):
  brain token list [--json]
  brain token create --name <name> [--scope read|write|admin]... [--project <id>]...
                     [--role admin|member|viewer] [--expires 30d|720h] [--json]
  brain token revoke <id>

list shows token records, never secrets: admins see every user's tokens,
other users only their own. Revoked and expired tokens are listed with their
status.

Without --scope the token carries its full role (unscoped). read allows GET
requests, write allows knowledge/state/project writes, admin allows admin
endpoints; write and admin imply read. --project limits the token to those
projects. --expires takes days (30d) or a Go duration (720h); without it the
token never expires (--expires-in-days N is the older spelling). The raw token
is printed once and cannot be shown again.
`)
}

type repeatedFlag []string

func (r *repeatedFlag) String() string     { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(v string) error { *r = append(*r, v); return nil }

var _ flag.Value = (*repeatedFlag)(nil)

// maxTokenExpiry mirrors the server's ten-year cap on token lifetimes.
const maxTokenExpiry = 3650 * 24 * time.Hour

func runToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printTokenUsage(stderr)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		printTokenUsage(stdout)
		return 0
	case "list":
		return runTokenList(args[1:], stdout, stderr)
	case "create":
		return runTokenCreate(args[1:], stdout, stderr)
	case "revoke":
		return runTokenRevoke(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "brain token: unknown subcommand %q\n\n", args[0])
		printTokenUsage(stderr)
		return 2
	}
}

// tokenClient loads the config and requires server mode. It returns a
// non-zero exit code when no client is available: 2 outside server mode,
// because token management does not exist in local or git mode.
func tokenClient(cfgPath, command string, stderr io.Writer) (*httpclient.Client, int) {
	cfg, err := loadRequiredConfig(cfgPath)
	if err != nil {
		return nil, reportError(stderr, command, err)
	}
	if cfg.Mode != config.ModeServer {
		fmt.Fprintf(stderr, "brain %s: API tokens exist only in server mode (this instance is in %s mode; set mode: server in brain.yaml)\n", command, cfg.Mode)
		return nil, 2
	}
	client, err := bootstrap.NewServerClient(cfg)
	if err != nil {
		return nil, reportError(stderr, command, err)
	}
	return client, 0
}

func runTokenList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("token list", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		printTokenUsage(stderr)
		return 2
	}
	client, code := tokenClient(*cfgPath, "token list", stderr)
	if code != 0 {
		return code
	}
	// all=true: the server widens the list for admins and ignores it for
	// everyone else, who only ever see their own tokens.
	tokens, err := client.ListAPITokens(context.Background(), true)
	if err != nil {
		return reportError(stderr, "token list", err)
	}
	now := time.Now().UTC()
	if *asJSON {
		items := make([]jsonAPIToken, 0, len(tokens))
		for _, token := range tokens {
			items = append(items, toJSONAPIToken(token, now))
		}
		return writeJSONReport(stdout, stderr, "token list", items)
	}
	if len(tokens) == 0 {
		fmt.Fprintln(stdout, "no API tokens")
		return 0
	}
	table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tNAME\tOWNER\tROLE\tSCOPES\tPROJECTS\tCREATED\tEXPIRES\tLAST USED\tSTATUS")
	for _, token := range tokens {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			token.ID, token.Name, token.UserID, token.Role, tokenScopeText(token.Scopes),
			tokenProjectText(token.ProjectIDs), formatTokenTime(&token.CreatedAt, "-"),
			formatTokenTime(token.ExpiresAt, "never"), formatTokenTime(token.LastUsedAt, "never"),
			tokenStatus(token, now))
	}
	if err := table.Flush(); err != nil {
		return reportError(stderr, "token list", err)
	}
	return 0
}

func runTokenCreate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("token create", stderr)
	name := fs.String("name", "", "token name")
	role := fs.String("role", "", "token role (defaults to your role)")
	expires := fs.String("expires", "", "lifetime: days (30d) or a Go duration (720h); empty = never")
	days := fs.Int("expires-in-days", 0, "expire after N days (0 = never); older form of --expires")
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
	req := httpclient.APITokenRequest{
		Name: strings.TrimSpace(*name), Role: strings.TrimSpace(*role),
		Scopes: scopes, ProjectIDs: projects,
	}
	if err := applyTokenExpiry(&req, *expires, *days, time.Now().UTC()); err != nil {
		fmt.Fprintf(stderr, "brain token create: %v\n", err)
		return 2
	}
	client, code := tokenClient(*cfgPath, "token create", stderr)
	if code != 0 {
		return code
	}
	raw, record, err := client.CreateAPIToken(context.Background(), req)
	if err != nil {
		return reportError(stderr, "token create", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "token create", jsonCreatedToken{
			Token:    raw,
			APIToken: toJSONAPIToken(record, time.Now().UTC()),
			Notice:   "the token value is shown only once and cannot be retrieved again",
		})
	}
	fmt.Fprintf(stdout, "token %s (%s, %s, %s, expires %s)\n%s\nStore it now: this value is shown only once and cannot be shown again.\n",
		record.ID, record.Role, tokenScopeText(record.Scopes), tokenProjectText(record.ProjectIDs),
		formatTokenTime(record.ExpiresAt, "never"), raw)
	return 0
}

func runTokenRevoke(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("token revoke", stderr)
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		printTokenUsage(stderr)
		return 2
	}
	id := strings.TrimSpace(positional[0])
	client, code := tokenClient(*cfgPath, "token revoke", stderr)
	if code != 0 {
		return code
	}
	if err := client.RevokeAPIToken(context.Background(), id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			fmt.Fprintf(stderr, "brain token revoke: no token %q\n", id)
			return 1
		}
		return reportError(stderr, "token revoke", err)
	}
	fmt.Fprintf(stdout, "revoked token %s\n", id)
	return 0
}

// applyTokenExpiry sets the request expiry from --expires or the older
// --expires-in-days. A day count is sent as expiresInDays; any other duration
// as an absolute RFC 3339 instant relative to now.
func applyTokenExpiry(req *httpclient.APITokenRequest, expires string, days int, now time.Time) error {
	expires = strings.TrimSpace(expires)
	if expires != "" && days > 0 {
		return errors.New("use either --expires or --expires-in-days, not both")
	}
	if days > 0 {
		req.ExpiresInDays = &days
		return nil
	}
	if expires == "" {
		return nil
	}
	if count, ok := strings.CutSuffix(expires, "d"); ok {
		n, err := strconv.Atoi(count)
		if err != nil || n < 1 || n > 3650 {
			return fmt.Errorf("--expires %q: days must be between 1d and 3650d", expires)
		}
		req.ExpiresInDays = &n
		return nil
	}
	duration, err := time.ParseDuration(expires)
	if err != nil {
		return fmt.Errorf("--expires %q: use days (30d) or a duration (720h)", expires)
	}
	if duration < time.Minute || duration > maxTokenExpiry {
		return fmt.Errorf("--expires %q: must be between 1m and 3650d", expires)
	}
	at := now.Add(duration).UTC().Format(time.RFC3339)
	req.ExpiresAt = &at
	return nil
}

// jsonAPIToken is a token record as agents see it. It never carries the
// secret or its hash; prefix is the display prefix the server stores.
type jsonAPIToken struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	UserID     string   `json:"user_id"`
	Role       string   `json:"role"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	ProjectIDs []string `json:"project_ids"`
	CreatedAt  *string  `json:"created_at"`
	ExpiresAt  *string  `json:"expires_at"`
	LastUsedAt *string  `json:"last_used_at"`
	RevokedAt  *string  `json:"revoked_at"`
	Revoked    bool     `json:"revoked"`
	Status     string   `json:"status"`
}

// jsonCreatedToken is the one response that carries a raw token value.
type jsonCreatedToken struct {
	Token    string       `json:"token"`
	APIToken jsonAPIToken `json:"api_token"`
	Notice   string       `json:"notice"`
}

func toJSONAPIToken(token httpclient.APIToken, now time.Time) jsonAPIToken {
	return jsonAPIToken{
		ID: token.ID, Name: token.Name, UserID: token.UserID, Role: token.Role, Prefix: token.Prefix,
		Scopes: nonNilStrings(token.Scopes), ProjectIDs: nonNilStrings(token.ProjectIDs),
		CreatedAt: jsonTime(token.CreatedAt), ExpiresAt: jsonTimePtr(token.ExpiresAt),
		LastUsedAt: jsonTimePtr(token.LastUsedAt), RevokedAt: jsonTimePtr(token.RevokedAt),
		Revoked: token.RevokedAt != nil, Status: tokenStatus(token, now),
	}
}

func jsonTimePtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	return jsonTime(*value)
}

// tokenStatus is revoked, expired or active, in that order of precedence.
func tokenStatus(token httpclient.APIToken, now time.Time) string {
	switch {
	case token.RevokedAt != nil:
		return "revoked"
	case token.ExpiresAt != nil && !token.ExpiresAt.After(now):
		return "expired"
	default:
		return "active"
	}
}

func tokenScopeText(scopes []string) string {
	if len(scopes) == 0 {
		return "unscoped"
	}
	return strings.Join(scopes, ",")
}

func tokenProjectText(projects []string) string {
	if len(projects) == 0 {
		return "all projects"
	}
	return strings.Join(projects, ",")
}

func formatTokenTime(value *time.Time, empty string) string {
	if value == nil || value.IsZero() {
		return empty
	}
	return value.UTC().Format(time.RFC3339)
}
