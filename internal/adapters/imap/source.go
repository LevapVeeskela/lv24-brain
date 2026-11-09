package imap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
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
	sourceCheckpointVersion = 1
	sourceMaxFolderBytes    = 255
)

// SourceDescriptor is the item-level IMAP source contract. Mail is personal
// data and defaults to local-only publication. The source never emits
// tombstones: it does not advertise the deletes capability, and a message
// absent from a later read is never reported as deleted.
func Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider: Name, DisplayName: "IMAP",
		Capabilities:   domain.Capabilities{Backfill: true, Incremental: true, AuthModes: []domain.AuthMode{domain.AuthBasic}},
		DefaultPublish: domain.PublishLocalOnly,
		ConfigFields: []domain.ConfigField{
			{Key: "username", Label: "IMAP username", Required: true},
			{Key: "password", Label: "IMAP password", Required: true, Secret: true},
			{Key: "folders", Label: "Mail folders", Help: "Comma or newline separated; defaults to INBOX."},
			{Key: "limit", Label: "Messages per read", Help: "Optional number from 1 to 500."},
		},
		PersonalData: true,
	}
}

// Source reads bounded message snippets into raw items. Checkpoints are owned
// by the host; this source never reads or writes the legacy UID state file.
type Source struct {
	connection  domain.Connection
	address     string
	username    string
	folders     []string
	limit       int
	credentials ports.CredentialProvider
	dial        DialFunc
	now         func() time.Time
}

type sourceFolderCursor struct {
	Folder      string `json:"folder"`
	UIDValidity uint32 `json:"uidValidity"`
	LastUID     uint32 `json:"lastUid"`
}

// NewSource builds an item-level IMAP source. The password is fetched from
// CredentialProvider using field "password" for this connection.
func NewSource(connection domain.Connection, credentials ports.CredentialProvider, dial DialFunc, now func() time.Time) (*Source, error) {
	descriptor := Descriptor()
	if err := descriptor.ValidateConnection(connection); err != nil {
		return nil, err
	}
	if credentials == nil {
		return nil, domain.NewValidationError("imap.credentials", "is required")
	}
	address, err := sourceAddress(connection.BaseURL)
	if err != nil {
		return nil, err
	}
	username := strings.TrimSpace(connection.Settings["username"])
	folders, err := parseSourceFolders(connection.Settings["folders"])
	if err != nil {
		return nil, err
	}
	limit := DefaultLimit
	if value := strings.TrimSpace(connection.Settings["limit"]); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > MaxLimit {
			return nil, domain.NewValidationError("imap.limit", fmt.Sprintf("must be between 1 and %d", MaxLimit))
		}
	}
	if dial == nil {
		dial = DialTLS
	}
	if now == nil {
		now = time.Now
	}
	return &Source{connection: connection, address: address, username: username, folders: folders, limit: limit, credentials: credentials, dial: dial, now: now}, nil
}

// Descriptor returns the provider's static description.
func (s *Source) Descriptor() domain.SourceDescriptor { return Descriptor() }

// Check authenticates and selects the first configured folder without reading
// any messages.
func (s *Source) Check(ctx context.Context) error {
	password, err := s.password(ctx)
	if err != nil {
		return err
	}
	session, err := s.dial(ctx, s.address, s.username, password, nil)
	if err != nil {
		return fmt.Errorf("imap check: %s", connectors.SanitizeError(err, password))
	}
	defer logoutSource(session)
	if _, err := session.Select(ctx, s.folders[0]); err != nil {
		return fmt.Errorf("imap check: %s", connectors.SanitizeError(err, password))
	}
	return nil
}

// Read returns one bounded batch. Each folder has an independent UIDVALIDITY
// and last-UID checkpoint, encoded into the opaque host checkpoint store. A
// failed page returns no items and no checkpoints, so the host discards the
// batch and the next run re-reads from the stored UID.
func (s *Source) Read(ctx context.Context, request ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if request.Connection.ID != s.connection.ID || request.Connection.Provider != Name {
		return ports.ReadBatch{}, domain.NewValidationError("imap.connection", "does not match source connection")
	}
	if request.Limit < 0 {
		return ports.ReadBatch{}, domain.NewValidationError("imap.limit", "must not be negative")
	}
	password, err := s.password(ctx)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	session, err := s.dial(ctx, s.address, s.username, password, nil)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("imap read: %s", connectors.SanitizeError(err, password))
	}
	defer logoutSource(session)

	limit := request.Limit
	if limit == 0 || limit > s.limit {
		limit = s.limit
	}
	items := make([]domain.Item, 0, limit)
	checkpoints := make([]domain.Checkpoint, 0, len(s.folders))
	more := false
	for folderIndex, folder := range s.folders {
		if err := ctx.Err(); err != nil {
			return ports.ReadBatch{}, err
		}
		stream := sourceFolderStream(folder)
		prior, hasPrior := request.Checkpoints[stream]
		cursor, err := decodeSourceCursor(prior, hasPrior, stream, folder)
		if err != nil {
			return ports.ReadBatch{}, err
		}
		mailbox, err := session.Select(ctx, folder)
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("imap select %q: %s", folder, connectors.SanitizeError(err, password))
		}
		if !hasPrior || cursor.UIDValidity != mailbox.UIDValidity {
			cursor = sourceFolderCursor{Folder: folder, UIDValidity: mailbox.UIDValidity}
		}
		since := request.Since
		uids, err := session.Search(ctx, imapv2.UID(cursor.LastUID), sinceOrZero(since))
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("imap search %q: %s", folder, connectors.SanitizeError(err, password))
		}
		sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
		for _, uid := range uids {
			if uid <= imapv2.UID(cursor.LastUID) {
				continue
			}
			if len(items) >= limit {
				more = true
				break
			}
			message, err := session.Fetch(ctx, uid, fetchBodyBytes)
			if err != nil {
				return ports.ReadBatch{}, fmt.Errorf("imap fetch %q uid %d: %s", folder, uid, connectors.SanitizeError(err, password))
			}
			items = append(items, sourceItem(request.Connection.ID, folder, mailbox.UIDValidity, message))
			cursor.LastUID = uint32(uid)
		}
		if !more && len(items) >= limit && folderIndex < len(s.folders)-1 {
			more = true
		}
		if !more && mailbox.UIDNext > 1 && cursor.LastUID < uint32(mailbox.UIDNext-1) {
			cursor.LastUID = uint32(mailbox.UIDNext - 1)
		}
		encoded, err := json.Marshal(cursor)
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("encode imap checkpoint: %w", err)
		}
		checkpoint := domain.Checkpoint{Stream: stream, Version: sourceCheckpointVersion, Data: encoded}
		if !hasPrior || !prior.Equal(checkpoint) || len(items) > 0 {
			checkpoints = append(checkpoints, checkpoint)
		}
		if more {
			break
		}
	}
	return ports.ReadBatch{Items: items, Checkpoints: checkpoints, More: more}, nil
}

func (s *Source) password(ctx context.Context) (string, error) {
	password, err := s.credentials.Get(ctx, s.connection.ID, "password")
	if err != nil {
		return "", fmt.Errorf("read imap password: %s", connectors.SanitizeError(err))
	}
	if password == "" {
		return "", errors.New("read imap password: credential is empty")
	}
	return password, nil
}

func parseSourceFolders(raw string) ([]string, error) {
	folders := make([]string, 0)
	for _, value := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		folder := strings.TrimSpace(value)
		if folder == "" {
			continue
		}
		if len(folder) > sourceMaxFolderBytes || strings.ContainsRune(folder, '\x00') {
			return nil, domain.NewValidationError("imap.folders", "folder names must be at most 255 bytes and contain no NUL")
		}
		folders = append(folders, folder)
	}
	if len(folders) == 0 {
		return []string{DefaultFolder}, nil
	}
	return connectors.UniqueStrings(folders), nil
}

func sourceAddress(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Scheme, "imaps") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", domain.NewValidationError("imap.baseUrl", "must be imaps://host[:port]")
	}
	port := parsed.Port()
	if port == "" {
		port = "993"
	}
	address := net.JoinHostPort(parsed.Hostname(), port)
	if err := validateAddress(address); err != nil {
		return "", err
	}
	return address, nil
}

func sourceFolderStream(folder string) string {
	sum := sha256.Sum256([]byte(folder))
	return "imap-" + hex.EncodeToString(sum[:12])
}

func sourceExternalID(folder string, uidValidity uint32, uid imapv2.UID) string {
	return fmt.Sprintf("%s:%d:%d", url.QueryEscape(folder), uidValidity, uid)
}

func decodeSourceCursor(checkpoint domain.Checkpoint, exists bool, stream, folder string) (sourceFolderCursor, error) {
	if !exists {
		return sourceFolderCursor{Folder: folder}, nil
	}
	if checkpoint.Stream != stream || checkpoint.Version != sourceCheckpointVersion {
		return sourceFolderCursor{}, domain.NewValidationError("imap.checkpoint", "unsupported stream or version")
	}
	var cursor sourceFolderCursor
	if err := json.Unmarshal(checkpoint.Data, &cursor); err != nil || cursor.Folder != folder {
		return sourceFolderCursor{}, domain.NewValidationError("imap.checkpoint", "invalid folder cursor")
	}
	return cursor, nil
}

func sourceItem(connectionID, folder string, uidValidity uint32, message FetchedMessage) domain.Item {
	title := "(no subject)"
	var createdAt time.Time
	author := ""
	if message.Envelope != nil {
		if subject := strings.TrimSpace(message.Envelope.Subject); subject != "" {
			title = subject
		}
		createdAt = message.Envelope.Date
		if len(message.Envelope.Sender) > 0 {
			sender := message.Envelope.Sender[0]
			name := strings.TrimSpace(sender.Name)
			address := ""
			if sender.Mailbox != "" && sender.Host != "" {
				address = strings.TrimSpace(sender.Mailbox) + "@" + strings.TrimSpace(sender.Host)
			}
			switch {
			case name != "" && address != "":
				author = name + " <" + address + ">"
			case name != "":
				author = name
			default:
				author = address
			}
		}
	}
	updatedAt := message.InternalDate
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	if createdAt.IsZero() {
		createdAt = updatedAt
	}
	snippet := message.Snippet
	if snippet == "" {
		snippet = ExtractSnippet(message.Header, message.Body, DefaultSnippetRunes)
	}
	return domain.Item{
		Ref:       domain.ItemRef{ConnectionID: connectionID, ExternalID: sourceExternalID(folder, uidValidity, message.UID)},
		Kind:      domain.ItemMessage,
		Title:     title,
		Body:      snippet,
		Author:    author,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

func logoutSource(session Session) {
	if session == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = session.Logout(ctx)
}

var _ ports.Source = (*Source)(nil)
