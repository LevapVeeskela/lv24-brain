package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Tests inject an in-memory credential store without accessing the OS keychain.
var buildConnections = bootstrap.BuildConnections
var buildIngestion = bootstrap.BuildIngestion

func printConnectionUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  brain connection list [--json]
  brain connection add <provider> --id <id> [--account a] [--base-url u]
      [--project p]... [--schedule s] [--publish local-only|shared]
      [--setting key=value]... [--json]
  brain connection secret set <id> <field>
  brain connection remove <id> --yes [--json]
  brain connection migrate [--copy-legacy-secrets] [--json]
  brain connection providers [--json]
  brain connection run <id> [--since RFC3339] [--days N] [--limit N] [--json]

All commands accept --config <path> and require local or git mode.
Use providers to inspect required settings. Secret fields must be set from
stdin with connection secret set; never put them in --setting.
Remove keeps items, checkpoints and credentials. Migration copies legacy
credentials only with --copy-legacy-secrets; it never deletes legacy data.
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
	var asJSON, yes, copySecrets bool
	var id, account, baseURL, schedule, publish string
	var sinceRaw string
	var limit, days int
	var projects, settings []string
	switch command {
	case "run":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
		fs.StringVar(&sinceRaw, "since", "", "read updates at or after RFC3339 time")
		fs.IntVar(&days, "days", 0, "read updates from the last N days (alternative to --since)")
		fs.IntVar(&limit, "limit", 0, "maximum items per run (0 uses default)")
	case "list", "providers":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
	case "add":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
		fs.StringVar(&id, "id", "", "unique connection id (lowercase letters, digits and hyphens; required)")
		fs.StringVar(&account, "account", "", "account label")
		fs.StringVar(&baseURL, "base-url", "", "provider endpoint URL without credentials")
		fs.StringVar(&schedule, "schedule", "", "manual, @hourly, @daily or @every <duration> (minimum 1m)")
		fs.StringVar(&publish, "publish", "", "local-only or shared (default: provider policy)")
		fs.Func("project", "brain project id (repeatable)", func(value string) error { projects = append(projects, value); return nil })
		fs.Func("setting", "non-secret provider setting key=value (repeatable)", func(value string) error { settings = append(settings, value); return nil })
	case "remove":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
		fs.BoolVar(&yes, "yes", false, "confirm removal of connection configuration (items are retained)")
	case "migrate":
		fs.BoolVar(&asJSON, "json", false, "print JSON")
		fs.BoolVar(&copySecrets, "copy-legacy-secrets", false, "copy missing credentials from legacy secret names without overwriting")
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
	case "add", "remove", "run":
		wantArgs = 1
	case "secret set":
		wantArgs = 2
	}
	if len(positional) != wantArgs {
		fmt.Fprintf(stderr, "brain connection %s: expected %d arguments\n", command, wantArgs)
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
	runtime, err := buildConnections(cfg, bootstrap.Options{})
	if err != nil {
		return reportError(stderr, "connection "+command, err)
	}
	ctx := context.Background()
	switch command {
	case "providers":
		return connectionProviders(runtime, asJSON, stdout, stderr)
	case "list":
		return connectionList(ctx, runtime, asJSON, stdout, stderr)
	case "add":
		connection := domain.Connection{ID: id, Provider: positional[0], Account: account, BaseURL: baseURL, Projects: projects, Schedule: schedule, Publish: domain.PublishPolicy(publish), Backend: domain.BackendNative, Settings: map[string]string{}}
		return connectionAdd(ctx, runtime, connection, settings, asJSON, stdout, stderr)
	case "secret set":
		return connectionSecretSet(ctx, runtime, positional[0], positional[1], stdin, stdout, stderr)
	case "remove":
		return connectionRemove(ctx, runtime, positional[0], cfg.DataDir, *cfgPath, yes, asJSON, stdout, stderr)
	case "migrate":
		return connectionMigrate(ctx, runtime, copySecrets, asJSON, stdout, stderr)
	}
	return 2
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

func connectionList(ctx context.Context, runtime *bootstrap.ConnectionRuntime, asJSON bool, stdout, stderr io.Writer) int {
	connections, err := runtime.Connections.List(ctx)
	if err != nil {
		return reportError(stderr, "connection list", err)
	}
	views, err := connectionViews(runtime, connections)
	if err != nil {
		return reportError(stderr, "connection list", err)
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
	if err := runtime.Connections.Create(ctx, connection); err != nil {
		if errors.Is(err, domain.ErrValidation) {
			return connectionValidationError(stderr, "add", err)
		}
		return reportError(stderr, "connection add", err)
	}
	view := viewConnection(connection, entry.Descriptor)
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
	if !ok || !field.Secret {
		return connectionValidationError(stderr, "secret set", errors.New("field must be a Secret config field of "+connection.Provider))
	}
	value, err := readSecretValue(stdin)
	if err != nil {
		return reportError(stderr, "connection secret set", errors.New("value is required on stdin (read failed or empty input)"))
	}
	if err := runtime.Credentials.Set(ctx, connection.ID, field.Key, value); err != nil {
		// Secret backends may include process output or the value in errors.
		return reportError(stderr, "connection secret set", errors.New("could not store credential; check the secret backend and environment overrides"))
	}
	key, _ := domain.CredentialKey(connection.ID, field.Key)
	fmt.Fprintln(stdout, key)
	return 0
}

func connectionRemove(ctx context.Context, runtime *bootstrap.ConnectionRuntime, connectionID, dataDir, cfgPath string, yes, asJSON bool, stdout, stderr io.Writer) int {
	if !yes {
		return connectionValidationError(stderr, "remove", errors.New("pass --yes to remove connection configuration"))
	}
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

func connectionMigrate(ctx context.Context, runtime *bootstrap.ConnectionRuntime, copySecrets, asJSON bool, stdout, stderr io.Writer) int {
	migrator := runtime.Migrator
	migrator.CopyLegacySecrets = copySecrets
	report, err := migrator.MigrateLegacyConnectors(ctx)
	if err != nil {
		return reportError(stderr, "connection migrate", err)
	}
	if asJSON {
		created, err := connectionViews(runtime, report.Created)
		if err != nil {
			return reportError(stderr, "connection migrate", err)
		}
		skipped, err := connectionViews(runtime, report.Existing)
		if err != nil {
			return reportError(stderr, "connection migrate", err)
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
}

func viewConnection(connection domain.Connection, descriptor domain.SourceDescriptor) connectionView {
	backend, schedule := connection.Backend, connection.Schedule
	if backend == "" {
		backend = domain.BackendNative
	}
	if schedule == "" {
		schedule = "manual"
	}
	return connectionView{connection.ID, connection.Provider, connection.Account, connection.BaseURL, nonNilStrings(connection.Projects), schedule, connection.EffectivePublish(descriptor), backend}
}

func connectionViews(runtime *bootstrap.ConnectionRuntime, connections []domain.Connection) ([]connectionView, error) {
	views := make([]connectionView, 0, len(connections))
	for _, connection := range connections {
		entry, err := runtime.Registry.Lookup(connection.Provider)
		if err != nil {
			return nil, err
		}
		views = append(views, viewConnection(connection, entry.Descriptor))
	}
	return views, nil
}

func printConnection(w io.Writer, connection connectionView) {
	fmt.Fprintf(w, "%s provider=%s account=%q base-url=%q projects=%s schedule=%q publish=%s backend=%s\n", connection.ID, connection.Provider, connection.Account, connection.BaseURL, strings.Join(connection.Projects, ","), connection.Schedule, connection.Publish, connection.Backend)
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
