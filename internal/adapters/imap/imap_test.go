package imap

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const testPassword = "imap-test-password"

type fakeSecrets struct {
	value string
	err   error
}

func (f fakeSecrets) Get(ctx context.Context, key string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.value, nil
}

func (f fakeSecrets) Set(ctx context.Context, key, value string) error { return nil }
func (f fakeSecrets) Delete(ctx context.Context, key string) error     { return nil }
func (f fakeSecrets) Names(ctx context.Context) ([]string, error)      { return nil, nil }

type memorySink struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (s *memorySink) Write(ctx context.Context, path string, content []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[path] = append([]byte(nil), content...)
	return nil
}

func (s *memorySink) hasPrefix(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for path := range s.files {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func (s *memorySink) file(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.files[path]
	return string(content), ok
}

type searchCall struct {
	After imapv2.UID
	Since time.Time
}

type fetchCall struct {
	UID      imapv2.UID
	MaxBytes int64
}

type fakeSession struct {
	states   map[string]MailboxState
	messages map[string]map[imapv2.UID]FetchedMessage
	fetchErr map[imapv2.UID]error

	selected string
	selects  []string
	searches []searchCall
	fetches  []fetchCall
	logouts  int
}

func (f *fakeSession) Select(ctx context.Context, mailbox string) (MailboxState, error) {
	f.selects = append(f.selects, mailbox)
	state, ok := f.states[mailbox]
	if !ok {
		return MailboxState{}, errors.New("no such mailbox")
	}
	f.selected = mailbox
	return state, nil
}

func (f *fakeSession) Search(ctx context.Context, after imapv2.UID, since time.Time) ([]imapv2.UID, error) {
	f.searches = append(f.searches, searchCall{After: after, Since: since})
	messages := f.messages[f.selected]
	uids := make([]imapv2.UID, 0, len(messages))
	for uid := range messages {
		if uid > after {
			uids = append(uids, uid)
		}
	}
	return uids, nil
}

func (f *fakeSession) Fetch(ctx context.Context, uid imapv2.UID, maxBytes int64) (FetchedMessage, error) {
	f.fetches = append(f.fetches, fetchCall{UID: uid, MaxBytes: maxBytes})
	if err := f.fetchErr[uid]; err != nil {
		return FetchedMessage{}, err
	}
	message, ok := f.messages[f.selected][uid]
	if !ok {
		return FetchedMessage{}, errors.New("message not found")
	}
	return message, nil
}

func (f *fakeSession) Logout(ctx context.Context) error {
	f.logouts++
	return nil
}

func TestSessionHasNoMutatingOperations(t *testing.T) {
	typ := reflect.TypeOf((*Session)(nil)).Elem()
	allowed := map[string]bool{"Select": true, "Search": true, "Fetch": true, "Logout": true}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !allowed[name] {
			t.Fatalf("Session exposes unexpected method %q", name)
		}
	}
}

func TestConnectorReadsMetadataAndAdvancesUID(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "mail-state.json")
	sink := &memorySink{}
	session := &fakeSession{
		states: map[string]MailboxState{
			"INBOX": {UIDValidity: 10, UIDNext: 5, NumMessages: 2},
		},
		messages: map[string]map[imapv2.UID]FetchedMessage{
			"INBOX": {
				3: message(3, "First subject", "First message snippet. "+strings.Repeat("x", 500)+" FULL-BODY-SHOULD-NOT-APPEAR"),
				4: message(4, "Second subject", "Second message snippet."),
			},
		},
	}
	dialedPassword := ""
	fixed := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	connector, err := New(Config{
		Account:      "me",
		Address:      "imap.example.test:993",
		Username:     "me@example.test",
		Secrets:      fakeSecrets{value: testPassword},
		Folders:      []string{"INBOX"},
		Sink:         sink,
		StatePath:    statePath,
		Limit:        10,
		SnippetRunes: 40,
		Now:          func() time.Time { return fixed },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	connector.dial = func(ctx context.Context, address, username, password string, tlsConfig *tls.Config) (Session, error) {
		dialedPassword = password
		if address != "imap.example.test:993" || username != "me@example.test" {
			t.Fatalf("dial args = %q, %q", address, username)
		}
		return session, nil
	}

	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	run, err := connector.Run(context.Background(), ports.ConnectorOptions{Since: &since})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != domain.ConnectorSuccess || run.Counts["folders"] != 1 || run.Counts["messages"] != 2 {
		t.Fatalf("run = %#v", run)
	}
	if dialedPassword != testPassword {
		t.Fatalf("dial password = %q", dialedPassword)
	}
	if len(session.selects) != 1 || session.selects[0] != "INBOX" {
		t.Fatalf("selects = %v", session.selects)
	}
	if len(session.searches) != 1 || session.searches[0].After != 0 || !session.searches[0].Since.Equal(since) {
		t.Fatalf("searches = %#v", session.searches)
	}
	if len(session.fetches) != 2 || session.fetches[0].UID != 3 || session.fetches[1].UID != 4 || session.fetches[0].MaxBytes != fetchBodyBytes {
		t.Fatalf("fetches = %#v", session.fetches)
	}
	if session.logouts != 1 {
		t.Fatalf("logouts = %d", session.logouts)
	}

	content, ok := sink.file("mail/me/inbox/2026-11-05-uid3-4.md")
	if !ok {
		t.Fatalf("dump not written: %#v", sink.files)
	}
	for _, want := range []string{"UID 3", "First subject", "First message snippet", "UID 4", "Second subject", "Second message snippet"} {
		if !strings.Contains(content, want) {
			t.Fatalf("dump missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "FULL-BODY-SHOULD-NOT-APPEAR") || strings.Contains(content, testPassword) {
		t.Fatalf("dump leaked body or password:\n%s", content)
	}

	rawState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if strings.Contains(string(rawState), testPassword) {
		t.Fatalf("state leaked password: %s", rawState)
	}
	var state UIDState
	if err := json.Unmarshal(rawState, &state); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	cursor := state.Accounts["me"].Folders["INBOX"]
	if cursor.UIDValidity != 10 || cursor.LastUID != 4 || cursor.UpdatedAt.IsZero() {
		t.Fatalf("cursor = %#v", cursor)
	}

	// A second run starts after the persisted UID and, with nothing new,
	// writes nothing: the earlier dump stays as it was.
	if _, err := connector.Run(context.Background(), ports.ConnectorOptions{}); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(session.searches) != 2 || session.searches[1].After != 4 {
		t.Fatalf("second search = %#v", session.searches)
	}
	if len(sink.files) != 1 {
		t.Fatalf("idle run wrote dumps: %#v", sink.files)
	}
	if again, _ := sink.file("mail/me/inbox/2026-11-05-uid3-4.md"); again != content {
		t.Fatal("idle run changed the earlier dump")
	}
}

func TestConnectorWithoutSinkKeepsTheCursor(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "mail-state.json")
	session := &fakeSession{
		states:   map[string]MailboxState{"INBOX": {UIDValidity: 10, UIDNext: 5, NumMessages: 1}},
		messages: map[string]map[imapv2.UID]FetchedMessage{"INBOX": {3: message(3, "Subject", "Body")}},
	}
	connector, err := New(Config{
		Account: "me", Address: "imap.example.test:993", Username: "me@example.test",
		Secrets: fakeSecrets{value: testPassword}, Folders: []string{"INBOX"},
		StatePath: statePath, Limit: 10,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	connector.dial = func(context.Context, string, string, string, *tls.Config) (Session, error) { return session, nil }
	if _, err := connector.Run(context.Background(), ports.ConnectorOptions{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var state UIDState
	if raw, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
	}
	if cursor := state.Accounts["me"].Folders["INBOX"]; cursor.LastUID != 0 {
		t.Fatalf("cursor advanced although nothing was persisted: %#v", cursor)
	}
}

func TestConnectorResetsCursorOnUIDValidityChange(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "mail-state.json")
	initial := UIDState{
		Version: uidStateVersion,
		Accounts: map[string]AccountState{
			"me": {Folders: map[string]FolderCursor{
				"INBOX": {UIDValidity: 1, LastUID: 99},
			}},
		},
	}
	raw, _ := json.Marshal(initial)
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	session := &fakeSession{
		states: map[string]MailboxState{"INBOX": {UIDValidity: 2, UIDNext: 3}},
		messages: map[string]map[imapv2.UID]FetchedMessage{
			"INBOX": {2: message(2, "After reset", "Body")},
		},
	}
	connector, err := New(Config{
		Account:   "me",
		Address:   "imap.example.test:993",
		Username:  "me@example.test",
		Secrets:   fakeSecrets{value: testPassword},
		Sink:      &memorySink{},
		StatePath: statePath,
		Dial: func(ctx context.Context, address, username, password string, tlsConfig *tls.Config) (Session, error) {
			return session, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := connector.Run(context.Background(), ports.ConnectorOptions{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(session.searches) != 1 || session.searches[0].After != 0 {
		t.Fatalf("search after reset = %#v", session.searches)
	}
	loaded, err := NewUIDStateStore(statePath).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cursor := loaded.Accounts["me"].Folders["INBOX"]
	if cursor.UIDValidity != 2 || cursor.LastUID != 2 {
		t.Fatalf("cursor = %#v", cursor)
	}
}

func TestConnectorHonorsFoldersAndLimit(t *testing.T) {
	sink := &memorySink{}
	session := &fakeSession{
		states: map[string]MailboxState{
			"INBOX":   {UIDValidity: 5, UIDNext: 4},
			"Archive": {UIDValidity: 6, UIDNext: 4},
		},
		messages: map[string]map[imapv2.UID]FetchedMessage{
			"INBOX":   {1: message(1, "One", "one"), 2: message(2, "Two", "two"), 3: message(3, "Three", "three")},
			"Archive": {1: message(1, "Archived", "archived")},
		},
	}
	connector, err := New(Config{
		Account:  "me",
		Address:  "imap.example.test:993",
		Username: "me@example.test",
		Secrets:  fakeSecrets{value: testPassword},
		Folders:  []string{"INBOX", "Archive"},
		Sink:     sink,
		Limit:    2,
		Dial: func(ctx context.Context, address, username, password string, tlsConfig *tls.Config) (Session, error) {
			return session, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run, err := connector.Run(context.Background(), ports.ConnectorOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Counts["folders"] != 2 || run.Counts["messages"] != 3 {
		t.Fatalf("counts = %#v", run.Counts)
	}
	if len(session.fetches) != 3 {
		t.Fatalf("fetches = %#v", session.fetches)
	}
	if !sink.hasPrefix("mail/me/inbox/") {
		t.Fatalf("missing INBOX dump: %#v", sink.files)
	}
	if !sink.hasPrefix("mail/me/archive/") {
		t.Fatalf("missing Archive dump: %#v", sink.files)
	}
}

func TestConnectorCheckpointsAtTheLastPersistedMessageAfterFetchFailure(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "mail-state.json")
	session := &fakeSession{
		states: map[string]MailboxState{"INBOX": {UIDValidity: 7, UIDNext: 4}},
		messages: map[string]map[imapv2.UID]FetchedMessage{
			"INBOX": {1: message(1, "One", "one"), 2: message(2, "Two", "two")},
		},
		fetchErr: map[imapv2.UID]error{2: errors.New("connection dropped")},
	}
	sink := &memorySink{}
	connector, err := New(Config{
		Account:   "me",
		Address:   "imap.example.test:993",
		Username:  "me@example.test",
		Secrets:   fakeSecrets{value: testPassword},
		Sink:      sink,
		StatePath: statePath,
		Dial: func(ctx context.Context, address, username, password string, tlsConfig *tls.Config) (Session, error) {
			return session, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run, err := connector.Run(context.Background(), ports.ConnectorOptions{})
	if err == nil || run.Status != domain.ConnectorPartial {
		t.Fatalf("Run = (%#v, %v)", run, err)
	}
	if strings.Contains(run.Error, testPassword) {
		t.Fatalf("run error leaked password: %q", run.Error)
	}
	loaded, err := NewUIDStateStore(statePath).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cursor := loaded.Accounts["me"].Folders["INBOX"]
	if cursor.LastUID != 1 {
		t.Fatalf("cursor after fetch failure = %#v, want the last persisted UID 1", cursor)
	}
	if !sink.hasPrefix("mail/me/inbox/") || len(sink.files) != 1 {
		t.Fatalf("persisted batch = %#v", sink.files)
	}
}

func TestConnectorFailsClosedWithoutPassword(t *testing.T) {
	dialed := false
	connector, err := New(Config{
		Account:  "me",
		Address:  "imap.example.test:993",
		Username: "me@example.test",
		Secrets:  fakeSecrets{err: domain.ErrNotFound},
		Dial: func(ctx context.Context, address, username, password string, tlsConfig *tls.Config) (Session, error) {
			dialed = true
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run, err := connector.Run(context.Background(), ports.ConnectorOptions{})
	if err == nil || run.Status != domain.ConnectorFailed {
		t.Fatalf("Run = (%#v, %v)", run, err)
	}
	if dialed {
		t.Fatal("dial happened without a password")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	base := Config{
		Account:  "me",
		Address:  "imap.example.test:993",
		Username: "me@example.test",
		Secrets:  fakeSecrets{value: testPassword},
	}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"empty account", func() Config { c := base; c.Account = ""; return c }()},
		{"bad address", func() Config { c := base; c.Address = "localhost"; return c }()},
		{"bad port", func() Config { c := base; c.Address = "localhost:0"; return c }()},
		{"empty username", func() Config { c := base; c.Username = ""; return c }()},
		{"no secrets", func() Config { c := base; c.Secrets = nil; return c }()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := New(testCase.cfg); err == nil {
				t.Fatal("New = nil error")
			}
		})
	}
}

func message(uid imapv2.UID, subject, body string) FetchedMessage {
	return FetchedMessage{
		UID: uid,
		Envelope: &imapv2.Envelope{
			Subject:   subject,
			From:      []imapv2.Address{{Name: "Sender", Mailbox: "sender", Host: "example.test"}},
			To:        []imapv2.Address{{Name: "Me", Mailbox: "me", Host: "example.test"}},
			Date:      time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC),
			MessageID: "message-" + subject + "@example.test",
		},
		InternalDate: time.Date(2026, 11, 5, 10, 1, 0, 0, time.UTC),
		Size:         int64(len(body)),
		Header:       []byte("Content-Type: text/plain\r\n\r\n"),
		Body:         []byte(body),
	}
}

var _ ports.SecretStore = fakeSecrets{}

var _ ports.Connector = (*Connector)(nil)
