package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/connectionlock"
	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/gitstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Tests inject an in-memory credential store without accessing the OS keychain.
var buildConnections = bootstrap.BuildConnections
var buildIngestion = bootstrap.BuildIngestion

func printConnectionUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  brain connection list [--json]
  brain connection test <id> [--json]
  brain connection add <provider> --id <id> [--account a] [--base-url u]
      [--project p]... [--schedule s] [--publish local-only|shared]
      [--setting key=value]... [--json]
  brain connection publish <id> local-only|shared [--json]
  brain connection secret set <id> <field>
  brain connection remove <id> --yes [--json]
  brain connection migrate [--copy-legacy-secrets] [--json]
  brain connection migrate-identity [--check|--apply] [--json]
  brain connection providers [--json]
  brain connection run <id> [--since RFC3339] [--days N] [--limit N] [--json]
  brain connection auth <id> [--client-id <id>] [--scopes a,b] [--remove]

All commands accept --config <path> and require local or git mode.
list shows the newest run of each connection: last-run time, status and the
redacted cause of a failed run. test runs the provider's non-mutating check
(the same Source.Check the agent uses) and exits non-zero on failure.
Use providers to inspect required settings. Secret fields must be set from
stdin with connection secret set; never put them in --setting. A protected
identity value can be restored with: connection secret set <id> <field>.
publish records the explicit raw-zone policy: local-only untracks
connectors/<id> from git (files and history kept), shared publishes items and
keeps checkpoints local. A connection without an explicit policy keeps the
policy git already tracks, or stays local-only when nothing is tracked.
Remove keeps items, checkpoints and credentials. Migration copies legacy
credentials only with --copy-legacy-secrets; it never deletes legacy data.
Account labels and connector-declared identity settings (imap username) are
personal data: add and migrate-identity store them in the secret store as
conn/<id>/<field> and keep only a marker in the catalogue. migrate-identity is
a dry run unless --apply is passed; raw legacy values stay readable and are
never rewritten without --apply.
`)
}

func runConnection(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printConnectionUsage(stderr)
		return 2
	}
	command, rest := args[0], args[1:]
	if command == "help" || command == "-h" || command == "--help" {
		printConnectionUsage(stdout)
		return 0
	}
	if command == "secret" {
		if len(rest) == 0 || rest[0] == "--help" || rest[0] == "-h" || rest[0] == "help" {
			printConnectionUsage(stdout)
			if len(rest) == 0 {
				return 2
			}
			return 0
		}
		if rest[0] != "set" {
			fmt.Fprintln(stderr, "brain connection secret: expected set <id> <field>")
			return 2
		}
		command, rest = "secret set", rest[1:]
	}
	fs := newFlagSet("connection "+command, stderr)
	cfgPath := configFlag(fs)
	var asJSON, yes, copySecrets, applyIdentity, checkIdentity bool
	var id, account, baseURL, schedule, publish string
	var sinceRaw, clientID, scopesRaw string
	var limit, days int
	var remove bool
	var projects, settings []string
	switch command {
	case "run":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
		fs.StringVar(&sinceRaw, "since", "", "read updates at or after RFC3339 time")
		fs.IntVar(&days, "days", 0, "read updates from the last N days (alternative to --since)")
		fs.IntVar(&limit, "limit", 0, "maximum items per run (0 uses default)")
	case "auth":
		fs.StringVar(&clientID, "client-id", "", "OAuth public client ID (or BRAIN_OAUTH_CLIENT_ID)")
		fs.StringVar(&scopesRaw, "scopes", "", "comma-separated OAuth scopes (default: provider defaults)")
		fs.BoolVar(&remove, "remove", false, "remove the stored OAuth session for this connection")
	case "list", "providers", "test":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
	case "publish":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
	case "add":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
		fs.StringVar(&id, "id", "", "unique connection id (lowercase letters, digits and hyphens; required)")
		fs.StringVar(&account, "account", "", "account label")
		fs.StringVar(&baseURL, "base-url", "", "provider endpoint URL without credentials")
		fs.StringVar(&schedule, "schedule", "", "manual, @hourly, @daily or @every <duration> (minimum 1m)")
		fs.StringVar(&publish, "publish", "", "explicit raw-zone policy: local-only or shared (default: keep the policy git tracks, else local-only)")
		fs.Func("project", "brain project id (repeatable)", func(value string) error { projects = append(projects, value); return nil })
		fs.Func("setting", "non-secret provider setting key=value (repeatable)", func(value string) error { settings = append(settings, value); return nil })
	case "remove":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
		fs.BoolVar(&yes, "yes", false, "confirm removal of connection configuration (items are retained)")
	case "migrate":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
		fs.BoolVar(&copySecrets, "copy-legacy-secrets", false, "copy missing credentials from legacy secret names without overwriting")
	case "migrate-identity":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
		fs.BoolVar(&applyIdentity, "apply", false, "store raw accounts in the secret store and replace them with markers")
		fs.BoolVar(&checkIdentity, "check", false, "report only; writes nothing (default)")
	case "secret set": // Values are read only from stdin, never from flags.
	default:
		fmt.Fprintf(stderr, "brain connection: unknown subcommand %q\n", command)
		printConnectionUsage(stderr)
		return 2
	}
	positional, err := parseInterspersed(fs, rest)
	if err != nil {
		return 2
	}
	wantArgs := 0
	switch command {
	case "add", "remove", "run", "auth", "test":
		wantArgs = 1
	case "publish":
		wantArgs = 2
	case "secret set":
		wantArgs = 2
	}
	if len(positional) != wantArgs {
		fmt.Fprintf(stderr, "brain connection %s: expected %d arguments\n", command, wantArgs)
		return 2
	}
	if applyIdentity && checkIdentity {
		fmt.Fprintln(stderr, "brain connection migrate-identity: --check and --apply cannot be combined")
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "connection "+command, err)
	}
	if cfg.Mode == config.ModeServer {
		fmt.Fprintln(stderr, "brain connection: not available in server mode yet (use local or git mode)")
		return 2
	}
	if command == "auth" {
		runtime, err := buildConnections(cfg, bootstrap.Options{})
		if err != nil {
			return reportError(stderr, "connection auth", err)
		}
		return connectionAuth(context.Background(), runtime, positional[0], clientID, scopesRaw, remove, stdout, stderr)
	}
	if command == "run" {
		if limit < 0 {
			return connectionValidationError(stderr, "run", errors.New("--limit must not be negative"))
		}
		var since *time.Time
		if days < 0 || (days > 0 && strings.TrimSpace(sinceRaw) != "") {
			return connectionValidationError(stderr, "run", errors.New("--days must be positive and cannot be combined with --since"))
		}
		if days > 0 {
			cutoff := time.Now().UTC().AddDate(0, 0, -days)
			since = &cutoff
		}
		if sinceRaw != "" {
			value, err := time.Parse(time.RFC3339Nano, sinceRaw)
			if err != nil {
				return connectionValidationError(stderr, "run", errors.New("--since must be RFC3339"))
			}
			since = &value
		}
		return runConfiguredConnection(context.Background(), cfg, positional[0], since, limit, asJSON, stdout, stderr)
	}
	if command == "test" {
		return connectionTest(context.Background(), cfg, positional[0], asJSON, stdout, stderr)
	}
	runtime, err := buildConnections(cfg, bootstrap.Options{})
	if err != nil {
		return reportError(stderr, "connection "+command, err)
	}
	ctx := context.Background()
	switch command {
	case "providers":
		return connectionProviders(runtime, asJSON, stdout, stderr)
	case "list":
		return connectionList(ctx, runtime, cfg.DataDir, asJSON, stdout, stderr)
	case "add":
		connection := domain.Connection{ID: id, Provider: positional[0], Account: account, BaseURL: baseURL, Projects: projects, Schedule: schedule, Publish: domain.PublishPolicy(publish), Backend: domain.BackendNative, Settings: map[string]string{}}
		return connectionAdd(ctx, runtime, connection, settings, asJSON, stdout, stderr)
	case "secret set":
		return connectionSecretSet(ctx, runtime, positional[0], positional[1], stdin, stdout, stderr)
	case "remove":
		return connectionRemove(ctx, runtime, positional[0], cfg.DataDir, *cfgPath, yes, asJSON, stdout, stderr)
	case "publish":
		return connectionPublish(ctx, runtime, cfg, positional[0], positional[1], asJSON, stdout, stderr)
	case "migrate":
		return connectionMigrate(ctx, runtime, copySecrets, asJSON, stdout, stderr)
	case "migrate-identity":
		return connectionMigrateIdentity(ctx, runtime, applyIdentity, asJSON, stdout, stderr)
	}
	return 2
}

// connectionCheckTimeout bounds one non-mutating provider check.
const connectionCheckTimeout = 30 * time.Second

// connectionTest runs the same Source.Check the agent performs, using the
// ingestion runtime's registry and dependency set, so a manual test cannot
// drift from a real run. Failures are always reported sanitized.
func connectionTest(ctx context.Context, cfg config.BrainConfig, id string, asJSON bool, stdout, stderr io.Writer) int {
	runtime, err := buildIngestion(cfg, bootstrap.Options{})
	if err != nil {
		return reportError(stderr, "connection test", err)
	}
	defer runtime.Close()
	host := runtime.Ingestion
	connection, err := host.Connections.Get(ctx, id)
	if err != nil {
		return reportError(stderr, "connection test", err)
	}
	// Identity values are resolved here, when a check actually reads the
	// connection, never for a whole catalogue: a scheduled run may have no
	// unlocked keychain.
	connection, err = connectorbuild.ResolveIdentity(ctx, connection, host.Deps.Credentials)
	if err != nil {
		return reportConnectionFailure(stdout, stderr, id, app.DescribeConnectionCheckError(err), asJSON)
	}
	source, err := host.Registry.Build(connection, host.Deps)
	if err != nil {
		return reportConnectionFailure(stdout, stderr, id, app.DescribeConnectionBuildError(err), asJSON)
	}
	checkCtx, cancel := context.WithTimeout(ctx, connectionCheckTimeout)
	defer cancel()
	if err := source.Check(checkCtx); err != nil {
		return reportConnectionFailure(stdout, stderr, id, app.DescribeConnectionCheckError(err), asJSON)
	}
	if asJSON {
		return writeJSONReport(stdout, stderr, "connection test", struct {
			ID string `json:"id"`
			OK bool   `json:"ok"`
		}{id, true})
	}
	fmt.Fprintf(stdout, "%s: ok\n", id)
	return 0
}

func reportConnectionFailure(stdout, stderr io.Writer, id string, failure app.ConnectionFailure, asJSON bool) int {
	if asJSON {
		if code := writeJSONReport(stdout, stderr, "connection test", struct {
			ID      string `json:"id"`
			OK      bool   `json:"ok"`
			Code    string `json:"code"`
			Message string `json:"message"`
			Cause   string `json:"cause,omitempty"`
		}{id, false, failure.Code, failure.Message, failure.Cause}); code != 0 {
			return code
		}
		return 1
	}
	message := failure.Message
	if failure.Cause != "" {
		message += ": " + failure.Cause
	}
	fmt.Fprintf(stderr, "brain connection test %s: %s (%s)\n", id, message, failure.Code)
	return 1
}

func runConfiguredConnection(ctx context.Context, cfg config.BrainConfig, id string, since *time.Time, limit int, asJSON bool, stdout, stderr io.Writer) int {
	runtime, err := buildIngestion(cfg, bootstrap.Options{})
	if err != nil {
		return reportError(stderr, "connection run", err)
	}
	defer runtime.Close()
	runCtx, cancel := context.WithTimeout(ctx, connectorRunTimeout)
	defer cancel()
	result, err := runtime.Ingestion.RunConnection(runCtx, app.RunConnectionOptions{ConnectionID: id, Since: since, Limit: limit})
	if err != nil {
		// The host sanitizes persisted errors; returned adapter errors can contain credentials.
		message, _ := app.RedactIngest(err.Error())
		return reportError(stderr, "connection run", errors.New(message))
	}
	if asJSON {
		if code := writeJSONReport(stdout, stderr, "connection run", result); code != 0 {
			return code
		}
	} else if result.Busy {
		fmt.Fprintln(stdout, "connection busy")
	} else {
		printConnectorRun(stdout, result.Run)
		if result.PublishNotice != "" {
			fmt.Fprintln(stdout, result.PublishNotice)
		}
	}
	if result.Busy || result.Run.Status != domain.ConnectorSuccess {
		return 1
	}
	return 0
}

func connectionProviders(runtime *bootstrap.ConnectionRuntime, asJSON bool, stdout, stderr io.Writer) int {
	descriptors := runtime.Registry.Descriptors()
	if asJSON {
		return writeJSONReport(stdout, stderr, "connection providers", descriptors)
	}
	for _, descriptor := range descriptors {
		modes := make([]string, len(descriptor.Capabilities.AuthModes))
		for i, mode := range descriptor.Capabilities.AuthModes {
			modes[i] = string(mode)
		}
		fmt.Fprintf(stdout, "%s (%s) auth=%s default-publish=%s\n", descriptor.Provider, descriptor.DisplayName, strings.Join(modes, ","), descriptor.DefaultPublish)
		for _, field := range descriptor.ConfigFields {
			fmt.Fprintf(stdout, "  %s: %s required=%t secret=%t\n", field.Key, field.Label, field.Required, field.Secret)
		}
	}
	return 0
}

func connectionList(ctx context.Context, runtime *bootstrap.ConnectionRuntime, dataDir string, asJSON bool, stdout, stderr io.Writer) int {
	connections, err := runtime.Connections.List(ctx)
	if err != nil {
		return reportError(stderr, "connection list", err)
	}
	views, warnings, err := connectionViews(ctx, runtime, connections)
	if err != nil {
		return reportError(stderr, "connection list", err)
	}
	for _, warning := range warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warning)
	}
	if len(views) > 0 {
		store, err := fsstore.New(dataDir)
		if err != nil {
			return reportError(stderr, "connection list", err)
		}
		summaries, err := app.ConnectionRunSummaries(ctx, store, 0)
		if err != nil {
			return reportError(stderr, "connection list", err)
		}
		for i := range views {
			if summary, ok := summaries[views[i].ID]; ok {
				views[i].LastRunAt = &summary.StartedAt
				views[i].LastRunStatus = summary.Status
				views[i].LastRunError = summary.Error
			}
		}
	}
	if asJSON {
		return writeJSONReport(stdout, stderr, "connection list", views)
	}
	for _, view := range views {
		printConnection(stdout, view)
	}
	return 0
}

func connectionAdd(ctx context.Context, runtime *bootstrap.ConnectionRuntime, connection domain.Connection, settings []string, asJSON bool, stdout, stderr io.Writer) int {
	entry, err := runtime.Registry.Lookup(connection.Provider)
	if err != nil {
		return connectionValidationError(stderr, "add", err)
	}
	for _, setting := range settings {
		key, value, ok := strings.Cut(setting, "=")
		if !ok || key == "" {
			return connectionValidationError(stderr, "add", errors.New("--setting must be key=value"))
		}
		field, known := entry.Descriptor.Field(key)
		if !known {
			return connectionValidationError(stderr, "add", fmt.Errorf("unknown setting %q for %s", key, connection.Provider))
		}
		if field.Secret {
			return connectionValidationError(stderr, "add", fmt.Errorf("%s is secret; run: brain connection secret set %s %s", key, connection.ID, key))
		}
		if _, duplicate := connection.Settings[key]; duplicate {
			return connectionValidationError(stderr, "add", fmt.Errorf("duplicate setting %q", key))
		}
		connection.Settings[key] = value
	}
	if err := entry.Descriptor.ValidateConnection(connection); err != nil {
		return connectionValidationError(stderr, "add", err)
	}
	if connectorbuild.IsAccountMarker(connection.Account) {
		return connectionValidationError(stderr, "add", errors.New("--account must be the account label, not a keychain marker"))
	}
	for _, field := range connectorbuild.IdentitySettingKeys(connection.Provider) {
		if connectorbuild.IsIdentityMarker(connection.Settings[field]) {
			return connectionValidationError(stderr, "add", fmt.Errorf("%s must be a value, not a keychain marker", field))
		}
	}
	display := connection
	if len(connectorbuild.IdentityFields(connection)) > 0 {
		// Never overwrite the account secret of an existing ID, and never add a
		// second connection for the same provider, endpoint and account.
		if _, err := runtime.Connections.Get(ctx, connection.ID); err == nil {
			return connectionValidationError(stderr, "add", fmt.Errorf("connection %q already exists", connection.ID))
		} else if !errors.Is(err, domain.ErrNotFound) {
			return reportError(stderr, "connection add", err)
		}
		existing, found, err := app.FindConnectionByIdentity(ctx, runtime.Connections, connection)
		if err != nil {
			return reportError(stderr, "connection add", err)
		}
		if found {
			return connectionValidationError(stderr, "add", fmt.Errorf("connection %q already uses this provider, endpoint and account", existing.ID))
		}
		protected, err := connectorbuild.ProtectIdentity(ctx, connection, runtime.Credentials)
		if err != nil {
			return reportError(stderr, "connection add", err)
		}
		connection = protected
	}
	if err := runtime.Connections.Create(ctx, connection); err != nil {
		if errors.Is(err, domain.ErrValidation) {
			return connectionValidationError(stderr, "add", err)
		}
		return reportError(stderr, "connection add", err)
	}
	view := viewConnection(display, entry.Descriptor)
	if asJSON {
		printConnectionSecretHints(stderr, connection.ID, entry.Descriptor)
		return writeJSONReport(stdout, stderr, "connection add", view)
	}
	printConnection(stdout, view)
	printConnectionSecretHints(stdout, connection.ID, entry.Descriptor)
	return 0
}

func connectionSecretSet(ctx context.Context, runtime *bootstrap.ConnectionRuntime, connectionID, fieldKey string, stdin io.Reader, stdout, stderr io.Writer) int {
	connection, err := runtime.Connections.Get(ctx, connectionID)
	if err != nil {
		return reportError(stderr, "connection secret set", err)
	}
	entry, err := runtime.Registry.Lookup(connection.Provider)
	if err != nil {
		return reportError(stderr, "connection secret set", err)
	}
	field, ok := entry.Descriptor.Field(fieldKey)
	identity := fieldKey == connectorbuild.AccountField || connectorbuild.IsIdentitySetting(connection.Provider, fieldKey)
	if !identity && (!ok || !field.Secret) {
		return connectionValidationError(stderr, "secret set", errors.New("field must be a Secret config field, or a declared identity field of "+connection.Provider))
	}
	value, err := readSecretValue(stdin)
	if err != nil {
		return reportError(stderr, "connection secret set", errors.New("value is required on stdin (read failed or empty input)"))
	}
	if identity && identityRepairMismatch(connection, fieldKey, value) {
		return connectionValidationError(stderr, "secret set", errors.New("value does not match this connection's stored identity marker; use migrate-identity to change it"))
	}
	if err := runtime.Credentials.Set(ctx, connection.ID, fieldKey, value); err != nil {
		// Secret backends may include process output or the value in errors.
		return reportError(stderr, "connection secret set", errors.New("could not store credential; check the secret backend and environment overrides"))
	}
	key, _ := domain.CredentialKey(connection.ID, fieldKey)
	fmt.Fprintln(stdout, key)
	return 0
}

// identityRepairMismatch reports whether field is marked in the catalogue and
// value does not match that marker. A raw legacy value is unmarked, so any
// repair value is allowed.
func identityRepairMismatch(connection domain.Connection, field, value string) bool {
	if field == connectorbuild.AccountField {
		return connectorbuild.IsAccountMarker(connection.Account) && !connectorbuild.AccountMarkerMatches(connection.Account, value)
	}
	if !connectorbuild.IsSettingMarker(connection.Provider, field, connection.Settings[field]) {
		return false
	}
	return !connectorbuild.IdentitySettingMarkerMatches(connection.Provider, field, connection.Settings[field], value)
}

func connectionRemove(ctx context.Context, runtime *bootstrap.ConnectionRuntime, connectionID, dataDir, cfgPath string, yes, asJSON bool, stdout, stderr io.Writer) int {
	if !yes {
		return connectionValidationError(stderr, "remove", errors.New("pass --yes to remove connection configuration"))
	}
	locker, err := connectionlock.New(dataDir)
	if err != nil {
		return reportError(stderr, "connection remove", err)
	}
	release, err := locker.TryLock(ctx, connectionID)
	if errors.Is(err, domain.ErrConflict) {
		fmt.Fprintln(stderr, "connection remove: connection busy; wait for the active run to finish")
		return 1
	}
	if err != nil {
		return reportError(stderr, "connection remove", err)
	}
	defer func() { _ = release() }()
	if err := runtime.Connections.Delete(ctx, connectionID); err != nil {
		return reportError(stderr, "connection remove", err)
	}
	purge := fmt.Sprintf("To purge retained items and checkpoints, delete %s and run brain index --rebuild --config \"%s\". Credentials are retained separately.", filepath.Join(dataDir, "connectors", connectionID), cfgPath)
	if asJSON {
		return writeJSONReport(stdout, stderr, "connection remove", struct {
			ID            string `json:"id"`
			ItemsRetained bool   `json:"itemsRetained"`
			Purge         string `json:"purge"`
		}{connectionID, true, purge})
	}
	fmt.Fprintf(stdout, "Removed connection %s; items retained.\n%s\n", connectionID, purge)
	return 0
}

// connectionPublish records the explicit raw-zone policy of one connection and
// applies it immediately: the marker for future writes, and the git index for
// already published files. local-only untracks connectors/<id>; shared keeps
// items tracked and untracks checkpoints. Files stay on disk either way and git
// history is never rewritten. Running it again is harmless.
func connectionPublish(ctx context.Context, runtime *bootstrap.ConnectionRuntime, cfg config.BrainConfig, connectionID, policyRaw string, asJSON bool, stdout, stderr io.Writer) int {
	policy := domain.PublishPolicy(strings.TrimSpace(policyRaw))
	if !policy.Valid() {
		return connectionValidationError(stderr, "publish", fmt.Errorf("policy must be %q or %q", domain.PublishLocalOnly, domain.PublishShared))
	}
	locker, err := connectionlock.New(cfg.DataDir)
	if err != nil {
		return reportError(stderr, "connection publish", err)
	}
	release, err := locker.TryLock(ctx, connectionID)
	if errors.Is(err, domain.ErrConflict) {
		fmt.Fprintln(stderr, "connection publish: connection busy; wait for the active run to finish")
		return 1
	}
	if err != nil {
		return reportError(stderr, "connection publish", err)
	}
	defer func() { _ = release() }()
	if _, err := runtime.Connections.Get(ctx, connectionID); err != nil {
		return reportError(stderr, "connection publish", err)
	}
	store, err := fsstore.New(cfg.DataDir)
	if err != nil {
		return reportError(stderr, "connection publish", err)
	}
	var provider *gitstore.Provider
	trackedBefore, trackedAfter := 0, 0
	if cfg.Mode == config.ModeGit {
		provider, err = gitstore.Open(cfg.DataDir, gitstore.Options{})
		if err != nil {
			return reportError(stderr, "connection publish", err)
		}
		paths, err := provider.TrackedRawZonePaths(ctx, connectionID)
		if err != nil {
			return reportError(stderr, "connection publish", err)
		}
		trackedBefore = len(paths)
	}
	// The catalogue records the owner's choice first: even when a later step
	// fails, the next run converges on it.
	if _, err := runtime.Connections.Patch(ctx, connectionID, func(connection *domain.Connection) error {
		connection.Publish = policy
		return nil
	}); err != nil {
		return reportError(stderr, "connection publish", err)
	}
	if err := store.Items().SetPublishPolicy(ctx, connectionID, policy); err != nil {
		return reportError(stderr, "connection publish", err)
	}
	if provider != nil {
		if err := provider.PrepareConnectionPublish(ctx, connectionID, policy); err != nil {
			return reportError(stderr, "connection publish", err)
		}
		paths, err := provider.TrackedRawZonePaths(ctx, connectionID)
		if err != nil {
			return reportError(stderr, "connection publish", err)
		}
		trackedAfter = len(paths)
	}
	if asJSON {
		return writeJSONReport(stdout, stderr, "connection publish", struct {
			ID            string               `json:"id"`
			Publish       domain.PublishPolicy `json:"publish"`
			TrackedBefore int                  `json:"trackedBefore"`
			TrackedAfter  int                  `json:"trackedAfter"`
		}{connectionID, policy, trackedBefore, trackedAfter})
	}
	fmt.Fprintf(stdout, "%s publish=%s\n", connectionID, policy)
	if cfg.Mode != config.ModeGit {
		fmt.Fprintln(stdout, "local mode: no git index to update")
		return 0
	}
	fmt.Fprintf(stdout, "raw-zone files tracked by git: %d -> %d\n", trackedBefore, trackedAfter)
	switch {
	case trackedBefore > trackedAfter:
		fmt.Fprintln(stdout, "Untracked files stay on disk and git history is not rewritten.")
	case policy == domain.PublishShared:
		fmt.Fprintln(stdout, "Items stay published; checkpoints stay local.")
	default:
		fmt.Fprintln(stdout, "Nothing was tracked.")
	}
	return 0
}

func connectionMigrate(ctx context.Context, runtime *bootstrap.ConnectionRuntime, copySecrets, asJSON bool, stdout, stderr io.Writer) int {
	migrator := runtime.Migrator
	migrator.CopyLegacySecrets = copySecrets
	report, err := migrator.MigrateLegacyConnectors(ctx)
	if err != nil {
		return reportError(stderr, "connection migrate", err)
	}
	if asJSON {
		created, createdWarnings, err := connectionViews(ctx, runtime, report.Created)
		if err != nil {
			return reportError(stderr, "connection migrate", err)
		}
		skipped, skippedWarnings, err := connectionViews(ctx, runtime, report.Existing)
		if err != nil {
			return reportError(stderr, "connection migrate", err)
		}
		for _, warning := range append(createdWarnings, skippedWarnings...) {
			fmt.Fprintf(stderr, "warning: %s\n", warning)
		}
		return writeJSONReport(stdout, stderr, "connection migrate", struct {
			Created     []connectionView                `json:"created"`
			Skipped     []connectionView                `json:"skipped"`
			Credentials []app.LegacyCredentialMigration `json:"credentials"`
		}{created, skipped, append([]app.LegacyCredentialMigration{}, report.Credentials...)})
	}
	fmt.Fprintf(stdout, "created: %d\nskipped: %d\n", len(report.Created), len(report.Existing))
	for _, connection := range report.Created {
		fmt.Fprintf(stdout, "created %s (%s)\n", connection.ID, connection.Provider)
	}
	for _, connection := range report.Existing {
		fmt.Fprintf(stdout, "skipped %s (%s)\n", connection.ID, connection.Provider)
	}
	for _, credential := range report.Credentials {
		fmt.Fprintf(stdout, "%s %s: legacy %s (%s)\n", credential.ConnectionID, credential.Field, credential.LegacyKey, credential.Status)
	}
	return 0
}

// connectionMigrateIdentity moves connection accounts from the git catalogue
// into the secret store. Without --apply it only reports. The raw account
// remains a valid fallback, so a partial migration never breaks a connection.
func connectionMigrateIdentity(ctx context.Context, runtime *bootstrap.ConnectionRuntime, apply, asJSON bool, stdout, stderr io.Writer) int {
	migrator := app.ConnectionIdentityMigrator{Connections: runtime.Connections, Credentials: runtime.Credentials}
	report, err := migrator.Migrate(ctx, apply)
	if err != nil {
		return reportError(stderr, "connection migrate-identity", err)
	}
	if asJSON {
		return writeJSONReport(stdout, stderr, "connection migrate-identity", report)
	}
	if apply {
		fmt.Fprintln(stdout, "apply: raw accounts are moved to the secret store")
	} else {
		fmt.Fprintln(stdout, "dry run: nothing written; pass --apply to move accounts")
	}
	if len(report.Entries) == 0 {
		fmt.Fprintln(stdout, "no connections with a stored account")
		return 0
	}
	for _, entry := range report.Entries {
		fmt.Fprintf(stdout, "%s %s -> %s (%s)\n", entry.ConnectionID, entry.Field, entry.Key, entry.Status)
	}
	if report.HasProblems() {
		fmt.Fprintln(stderr, "some account markers have no usable secret; restore them with: brain connection secret set <id> account")
		return 1
	}
	return 0
}

// connectionView deliberately omits Settings: even a manually edited catalogue
// cannot make list/add print a secret smuggled into provider settings.
type connectionView struct {
	ID       string               `json:"id"`
	Provider string               `json:"provider"`
	Account  string               `json:"account"`
	BaseURL  string               `json:"baseUrl"`
	Projects []string             `json:"projects"`
	Schedule string               `json:"schedule"`
	Publish  domain.PublishPolicy `json:"publish"`
	Backend  domain.Backend       `json:"backend"`
	// LastRunAt, LastRunStatus and LastRunError are the newest recorded run of
	// the connection. They stay empty when it has never run. LastRunError is
	// already redacted by app.ConnectionRunSummaries.
	LastRunAt     *time.Time `json:"lastRunAt,omitempty"`
	LastRunStatus string     `json:"lastRunStatus,omitempty"`
	LastRunError  string     `json:"lastRunError,omitempty"`
}

func viewConnection(connection domain.Connection, descriptor domain.SourceDescriptor) connectionView {
	backend, schedule := connection.Backend, connection.Schedule
	if backend == "" {
		backend = domain.BackendNative
	}
	if schedule == "" {
		schedule = "manual"
	}
	return connectionView{
		ID:       connection.ID,
		Provider: connection.Provider,
		Account:  connection.Account,
		BaseURL:  connection.BaseURL,
		Projects: nonNilStrings(connection.Projects),
		Schedule: schedule,
		Publish:  connection.EffectivePublish(descriptor),
		Backend:  backend,
	}
}

func connectionViews(ctx context.Context, runtime *bootstrap.ConnectionRuntime, connections []domain.Connection) ([]connectionView, []string, error) {
	views := make([]connectionView, 0, len(connections))
	warnings := []string{}
	for _, connection := range connections {
		entry, err := runtime.Registry.Lookup(connection.Provider)
		if err != nil {
			return nil, nil, err
		}
		display := connection
		if resolved, err := connectorbuild.ResolveAccount(ctx, connection, runtime.Credentials); err != nil {
			// A locked or incomplete keychain must not make list/add/migrate
			// unusable. Show a placeholder and one explicit warning instead.
			display.Account = connectorbuild.DescribeAccount(connection.Account)
			warnings = append(warnings, app.SanitizeConnectorError(err.Error()))
		} else {
			display = resolved
		}
		views = append(views, viewConnection(display, entry.Descriptor))
	}
	return views, warnings, nil
}

func printConnection(w io.Writer, connection connectionView) {
	fmt.Fprintf(w, "%s provider=%s account=%q base-url=%q projects=%s schedule=%q publish=%s backend=%s\n", connection.ID, connection.Provider, connection.Account, connection.BaseURL, strings.Join(connection.Projects, ","), connection.Schedule, connection.Publish, connection.Backend)
	if connection.LastRunAt == nil {
		fmt.Fprintln(w, "  last-run=never")
		return
	}
	fmt.Fprintf(w, "  last-run=%s status=%s", connection.LastRunAt.UTC().Format(time.RFC3339), connection.LastRunStatus)
	if connection.LastRunError != "" {
		fmt.Fprintf(w, " error=%q", connection.LastRunError)
	}
	fmt.Fprintln(w)
}

func printConnectionSecretHints(w io.Writer, id string, descriptor domain.SourceDescriptor) {
	for _, field := range descriptor.ConfigFields {
		if field.Secret {
			fmt.Fprintf(w, "Set %s from stdin: brain connection secret set %s %s\n", field.Label, id, field.Key)
		}
	}
}

func connectionValidationError(w io.Writer, command string, err error) int {
	fmt.Fprintf(w, "brain connection %s: %v\n", command, err)
	return 2
}
