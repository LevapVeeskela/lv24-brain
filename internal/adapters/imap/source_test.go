package imap

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const sourcePassword = "mail-source-password"

type sourceCredentialFake struct {
	connectionID string
	field        string
	value        string
	err          error
}

func (c sourceCredentialFake) Get(_ context.Context, connectionID, field string) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	if connectionID != c.connectionID || field != "password" {
		return "", domain.ErrNotFound
	}
	return c.value, nil
}

func TestSourceDescriptorIsPersonalAndLocalOnly(t *testing.T) {
	descriptor := Descriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if descriptor.Provider != Name || descriptor.DefaultPublish != domain.PublishLocalOnly || !descriptor.PersonalData || len(descriptor.Streams) != 0 {
		t.Fatalf("descriptor = %+v", descriptor)
	}
}

func TestSourceReadPaginatesWithUIDValidityCheckpointAndStableIdentity(t *testing.T) {
	connection := sourceConnection()
	session := &fakeSession{
		states: map[string]MailboxState{"INBOX": {UIDValidity: 17, UIDNext: 4}},
		messages: map[string]map[imapv2.UID]FetchedMessage{"INBOX": {
			1: {UID: 1, Envelope: &imapv2.Envelope{Subject: "First", Date: sourceDate()}, Header: []byte("Content-Type: text/plain\r\n\r\n"), Body: []byte("hello first")},
			2: {UID: 2, Envelope: &imapv2.Envelope{Subject: "Second", Date: sourceDate()}, Header: []byte("Content-Type: text/plain\r\n\r\n"), Body: []byte("hello second")},
			3: {UID: 3, Envelope: &imapv2.Envelope{Subject: "Third", Date: sourceDate()}, Header: []byte("Content-Type: text/plain\r\n\r\n"), Body: []byte("hello third")},
		}},
	}
	source := newTestSource(t, connection, session)
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: connection, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || !first.More || len(first.Checkpoints) != 1 {
		t.Fatalf("first batch = items:%d checkpoints:%+v more:%v", len(first.Items), first.Checkpoints, first.More)
	}
	if first.Items[0].Ref.ExternalID != "INBOX:17:1" || first.Items[1].Ref.ExternalID != "INBOX:17:2" {
		t.Fatalf("external ids = %q, %q", first.Items[0].Ref.ExternalID, first.Items[1].Ref.ExternalID)
	}
	if first.Items[0].Body != "hello first" || first.Items[0].Title != "First" {
		t.Fatalf("item fields = %+v", first.Items[0])
	}
	if len(session.fetches) != 2 || session.fetches[0].MaxBytes != fetchBodyBytes {
		t.Fatalf("fetches = %+v", session.fetches)
	}
	var cursor sourceFolderCursor
	if err := json.Unmarshal(first.Checkpoints[0].Data, &cursor); err != nil || cursor.UIDValidity != 17 || cursor.LastUID != 2 {
		t.Fatalf("checkpoint = %+v err=%v", cursor, err)
	}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: connection, Limit: 2, Checkpoints: map[string]domain.Checkpoint{first.Checkpoints[0].Stream: first.Checkpoints[0]}})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.More || second.Items[0].Ref.ExternalID != "INBOX:17:3" {
		t.Fatalf("second batch = items:%+v more:%v", second.Items, second.More)
	}
}

func TestSourceReadResetsUIDWhenUIDValidityChanges(t *testing.T) {
	connection := sourceConnection()
	stream := sourceFolderStream("INBOX")
	checkpointData, _ := json.Marshal(sourceFolderCursor{Folder: "INBOX", UIDValidity: 16, LastUID: 99})
	checkpoint := domain.Checkpoint{Stream: stream, Version: sourceCheckpointVersion, Data: checkpointData}
	session := &fakeSession{
		states:   map[string]MailboxState{"INBOX": {UIDValidity: 17, UIDNext: 2}},
		messages: map[string]map[imapv2.UID]FetchedMessage{"INBOX": {1: {UID: 1, Envelope: &imapv2.Envelope{Subject: "Reissued"}, Body: []byte("body")}}},
	}
	batch, err := newTestSource(t, connection, session).Read(context.Background(), ports.ReadRequest{Connection: connection, Checkpoints: map[string]domain.Checkpoint{stream: checkpoint}})
	if err != nil {
		t.Fatal(err)
	}
	if len(session.searches) != 1 || session.searches[0].After != 0 || len(batch.Items) != 1 || batch.Items[0].Ref.ExternalID != "INBOX:17:1" {
		t.Fatalf("searches=%+v batch=%+v", session.searches, batch)
	}
}

func TestSourceRejectsForeignConnectionAndInvalidCheckpoint(t *testing.T) {
	connection := sourceConnection()
	source := newTestSource(t, connection, &fakeSession{states: map[string]MailboxState{"INBOX": {UIDValidity: 1}}})
	if _, err := source.Read(context.Background(), ports.ReadRequest{Connection: domain.Connection{ID: "other", Provider: Name}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("foreign connection error = %v", err)
	}
	stream := sourceFolderStream("INBOX")
	checkpoint := domain.Checkpoint{Stream: stream, Version: sourceCheckpointVersion + 1}
	if _, err := source.Read(context.Background(), ports.ReadRequest{Connection: connection, Checkpoints: map[string]domain.Checkpoint{stream: checkpoint}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid checkpoint error = %v", err)
	}
}

func TestNewSourceValidatesCredentialsAndSettings(t *testing.T) {
	connection := sourceConnection()
	if _, err := NewSource(connection, nil, nil, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing credentials error = %v", err)
	}
	connection.Settings = map[string]string{"username": "alice"}
	connection.BaseURL = ""
	if _, err := NewSource(connection, sourceCredentialFake{connectionID: connection.ID, value: sourcePassword}, nil, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing base URL error = %v", err)
	}
}

func sourceConnection() domain.Connection {
	return domain.Connection{ID: "mail-work", Provider: Name, Account: "work", BaseURL: "imaps://mail.test:993", Settings: map[string]string{"username": "alice", "folders": "INBOX"}}
}

func sourceDate() time.Time { return time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC) }

func newTestSource(t *testing.T, connection domain.Connection, session Session) *Source {
	t.Helper()
	source, err := NewSource(connection, sourceCredentialFake{connectionID: connection.ID, value: sourcePassword}, func(context.Context, string, string, string, *tls.Config) (Session, error) { return session, nil }, func() time.Time { return sourceDate() })
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestSourceIDDoesNotExposeFolderSeparators(t *testing.T) {
	if got := sourceExternalID("INBOX/Projects", 17, 2); strings.Contains(got, "//") || !strings.Contains(got, ":17:2") {
		t.Fatalf("external id = %q", got)
	}
}
