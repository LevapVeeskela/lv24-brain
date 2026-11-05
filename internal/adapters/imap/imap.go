// Package imap implements a read-only IMAP connector. It selects mailboxes
// read-only, fetches headers and a bounded text body with BODY.PEEK, stores a
// snippet plus envelope metadata, and never sends, deletes, moves or flags.
package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// Name is the connector source name.
	Name = "imap"
	// DefaultFolder is selected when no folders are configured.
	DefaultFolder = "INBOX"
	// DefaultLimit is the per-folder message limit.
	DefaultLimit = 50
	// MaxLimit is the largest per-folder message limit.
	MaxLimit = 500
	// DefaultSnippetRunes bounds the stored snippet length.
	DefaultSnippetRunes = 280
	// MaxSnippetRunes is the largest accepted snippet length.
	MaxSnippetRunes = 2000
	// fetchBodyBytes bounds the bytes fetched from the server for a snippet.
	fetchBodyBytes = 8192
)

// Config configures the IMAP connector.
type Config struct {
	Account      string
	Address      string
	Username     string
	PasswordKey  string
	Secrets      ports.SecretStore
	Folders      []string
	Sink         connectors.MarkdownSink
	StatePath    string
	Limit        int
	SnippetRunes int
	TLSConfig    *tls.Config
	Dial         DialFunc
	Now          func() time.Time
}

// Connector reads metadata and snippets from configured IMAP folders.
type Connector struct {
	account      string
	address      string
	username     string
	passwordKey  string
	secrets      ports.SecretStore
	folders      []string
	sink         connectors.MarkdownSink
	state        *UIDStateStore
	limit        int
	snippetRunes int
	tlsConfig    *tls.Config
	dial         DialFunc
	now          func() time.Time
}

// New validates cfg and builds a connector.
func New(cfg Config) (*Connector, error) {
	account := strings.TrimSpace(cfg.Account)
	if account == "" {
		return nil, domain.NewValidationError("imap.account", "is required")
	}
	address := strings.TrimSpace(cfg.Address)
	if err := validateAddress(address); err != nil {
		return nil, err
	}
	username := strings.TrimSpace(cfg.Username)
	if username == "" {
		return nil, domain.NewValidationError("imap.username", "is required")
	}
	if cfg.Secrets == nil {
		return nil, domain.NewValidationError("imap.secrets", "is required")
	}
	passwordKey := strings.TrimSpace(cfg.PasswordKey)
	if passwordKey == "" {
		passwordKey = "imap/" + connectors.Slug(account) + "/password"
	}

	folders := make([]string, 0, len(cfg.Folders))
	for _, folder := range cfg.Folders {
		folder = strings.TrimSpace(folder)
		if folder == "" {
			continue
		}
		folders = append(folders, folder)
	}
	if len(folders) == 0 {
		folders = []string{DefaultFolder}
	}
	folders = connectors.UniqueStrings(folders)

	limit := cfg.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	snippetRunes := cfg.SnippetRunes
	if snippetRunes <= 0 {
		snippetRunes = DefaultSnippetRunes
	}
	if snippetRunes > MaxSnippetRunes {
		snippetRunes = MaxSnippetRunes
	}

	dial := cfg.Dial
	if dial == nil {
		dial = DialTLS
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	var tlsConfig *tls.Config
	if cfg.TLSConfig != nil {
		tlsConfig = cfg.TLSConfig.Clone()
	}

	return &Connector{
		account:      account,
		address:      address,
		username:     username,
		passwordKey:  passwordKey,
		secrets:      cfg.Secrets,
		folders:      folders,
		sink:         cfg.Sink,
		state:        NewUIDStateStore(strings.TrimSpace(cfg.StatePath)),
		limit:        limit,
		snippetRunes: snippetRunes,
		tlsConfig:    tlsConfig,
		dial:         dial,
		now:          now,
	}, nil
}

// Name reports the connector source.
func (c *Connector) Name() string { return Name }

// Run reads new messages per folder and writes deterministic Markdown dumps.
func (c *Connector) Run(ctx context.Context, opts ports.ConnectorOptions) (domain.ConnectorRun, error) {
	started := c.now().UTC()
	run := connectors.NewRun(Name, started)

	password, err := c.secrets.Get(ctx, c.passwordKey)
	if err != nil || password == "" {
		if err == nil {
			err = fmt.Errorf("imap password is empty")
		}
		runErr := fmt.Errorf("read imap password: %w", err)
		run = connectors.FinishRun(run, nil, runErr, started)
		return run, fmt.Errorf("imap connector: %s", connectors.SanitizeError(runErr))
	}

	session, err := c.dial(ctx, c.address, c.username, password, c.tlsConfig)
	if err != nil {
		runErr := fmt.Errorf("connect: %w", err)
		run = connectors.FinishRun(run, nil, runErr, started, password)
		return run, fmt.Errorf("imap connector: %s", connectors.SanitizeError(runErr, password))
	}
	defer func() {
		logoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = session.Logout(logoutCtx)
	}()

	state, err := c.state.Load()
	if err != nil {
		runErr := fmt.Errorf("load state: %w", err)
		run = connectors.FinishRun(run, nil, runErr, started, password)
		return run, fmt.Errorf("imap connector: %s", connectors.SanitizeError(runErr, password))
	}

	account := accountState(state, c.account)
	counts := map[string]int{}
	var failures []string

	for _, folder := range c.folders {
		mailbox, err := session.Select(ctx, folder)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: select: %s", folder, connectors.SanitizeError(err, password)))
			continue
		}

		cursor := account.Folders[folder]
		if cursor.UIDValidity != mailbox.UIDValidity {
			cursor = FolderCursor{UIDValidity: mailbox.UIDValidity}
		}

		uids, err := session.Search(ctx, imapv2.UID(cursor.LastUID), sinceOrZero(opts.Since))
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: search: %s", folder, connectors.SanitizeError(err, password)))
			continue
		}
		sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
		if len(uids) > c.limit {
			uids = uids[:c.limit]
		}

		messages := make([]FetchedMessage, 0, len(uids))
		fetchFailed := false
		for _, uid := range uids {
			message, err := session.Fetch(ctx, uid, fetchBodyBytes)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: fetch uid %d: %s", folder, uid, connectors.SanitizeError(err, password)))
				fetchFailed = true
				break
			}
			message.Snippet = ExtractSnippet(message.Header, message.Body, c.snippetRunes)
			message.Header = nil
			message.Body = nil
			messages = append(messages, message)
		}

		wrote := true
		if c.sink != nil {
			content := renderFolder(c.account, folder, mailbox, cursor, messages, c.now().UTC())
			if err := c.sink.Write(ctx, dumpPath(c.account, folder), content); err != nil {
				failures = append(failures, fmt.Sprintf("%s: write markdown: %s", folder, connectors.SanitizeError(err, password)))
				wrote = false
			}
		}
		if !fetchFailed && wrote {
			next := cursor
			next.UpdatedAt = c.now().UTC()
			if len(messages) > 0 {
				next.LastUID = uint32(messages[len(messages)-1].UID)
			} else if mailbox.UIDNext > 1 {
				next.LastUID = uint32(mailbox.UIDNext) - 1
			}
			if next.LastUID < cursor.LastUID {
				next.LastUID = cursor.LastUID
			}
			account.Folders[folder] = next
			setAccountState(&state, c.account, account)
			if err := c.state.Save(state); err != nil {
				failures = append(failures, fmt.Sprintf("%s: save state: %s", folder, connectors.SanitizeError(err, password)))
			}
		}

		counts["folders"]++
		counts["messages"] += len(messages)
	}

	var runErr error
	if len(failures) > 0 {
		runErr = errors.New(strings.Join(failures, "; "))
	}
	run = connectors.FinishRun(run, counts, runErr, c.now().UTC(), password)
	if runErr != nil {
		return run, fmt.Errorf("imap connector: %s", connectors.SanitizeError(runErr, password))
	}
	return run, nil
}

func validateAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || strings.TrimSpace(host) == "" {
		return domain.NewValidationError("imap.address", "must look like host:port")
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return domain.NewValidationError("imap.address", "port must be between 1 and 65535")
	}
	return nil
}

func sinceOrZero(since *time.Time) time.Time {
	if since == nil {
		return time.Time{}
	}
	return since.UTC()
}

func dumpPath(account, folder string) string {
	accountSlug := connectors.Slug(account)
	folderSlug, err := connectors.SlugPath(folder)
	if err != nil {
		folderSlug = connectors.Slug(folder)
	}
	return "mail/" + accountSlug + "/" + folderSlug + ".md"
}
