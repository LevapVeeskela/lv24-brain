package imap

// Shared ingestiontest contract suite for the native IMAP source (WP0.6).
// The fake upstream is an in-memory append-only mailbox: IMAP UIDs only
// move forward, so an "update" arrives as a new message with a new UID.
// The source declares no Deletes capability, so the deletes case is
// skipped by the suite and DeleteItem stays nil.

import (
	"context"
	"crypto/tls"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ingestiontest"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	imapSuiteConnectionID = "mail-test"
	imapSuiteBadID        = "mail-bad"
	imapSuitePassword     = "mail-suite-password"
	imapSuiteValidity     = 17
)

type imapSuiteUpstream struct {
	mu      sync.Mutex
	session *fakeSession
	nextUID imapv2.UID
	clock   time.Time
	next    int
}

func newIMAPSuiteUpstream() *imapSuiteUpstream {
	up := &imapSuiteUpstream{clock: time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)}
	up.session = &fakeSession{
		states:   map[string]MailboxState{"INBOX": {UIDValidity: imapSuiteValidity, UIDNext: 1}},
		messages: map[string]map[imapv2.UID]FetchedMessage{"INBOX": {}},
	}
	up.addLocked("First", "first body")
	up.addLocked("Second", "second body")
	return up
}

func (up *imapSuiteUpstream) addLocked(subject, body string) {
	up.mu.Lock()
	defer up.mu.Unlock()
	up.next++
	up.nextUID++
	up.clock = up.clock.Add(time.Second)
	uid := up.nextUID
	at := up.clock
	up.session.messages["INBOX"][uid] = FetchedMessage{
		UID:          uid,
		Envelope:     &imapv2.Envelope{Subject: subject, Date: at},
		InternalDate: at,
		Header:       []byte("Content-Type: text/plain\r\n\r\n"),
		Body:         []byte(body),
	}
	up.session.states["INBOX"] = MailboxState{
		UIDValidity: imapSuiteValidity,
		UIDNext:     uid + 1,
		NumMessages: uint32(len(up.session.messages["INBOX"])),
	}
}

func (up *imapSuiteUpstream) add(subject, body string) {
	up.addLocked(subject, body)
}

func (up *imapSuiteUpstream) addAuto(prefix string) {
	up.mu.Lock()
	n := up.next + 1
	up.mu.Unlock()
	up.addLocked(prefix+" "+strconv.Itoa(n), "body of "+prefix+" "+strconv.Itoa(n))
}

type imapSuiteCredentials struct{}

func (imapSuiteCredentials) Get(_ context.Context, connectionID, field string) (string, error) {
	if field != "password" {
		return "", domain.ErrNotFound
	}
	switch connectionID {
	case imapSuiteConnectionID:
		return imapSuitePassword, nil
	case imapSuiteBadID:
		return "wrong-password", nil
	default:
		return "", domain.ErrNotFound
	}
}

func TestSourceContractSuite(t *testing.T) {
	upstream := newIMAPSuiteUpstream()
	dial := func(_ context.Context, _, _, password string, _ *tls.Config) (Session, error) {
		if password != imapSuitePassword {
			return nil, errors.New("imap login: invalid credentials")
		}
		return upstream.session, nil
	}
	conn := domain.Connection{
		ID: imapSuiteConnectionID, Provider: Name,
		BaseURL:  "imaps://mail.test:993",
		Settings: map[string]string{"username": "alice", "folders": "INBOX"},
	}
	bad := conn
	bad.ID = imapSuiteBadID
	harness := ingestiontest.Harness{
		Connection:    conn,
		BadConnection: &bad,
		PageSize:      3,
		NewSource: func(connection domain.Connection) ports.Source {
			source, err := NewSource(connection, imapSuiteCredentials{}, dial, nil)
			if err != nil {
				t.Fatalf("NewSource(%s): %v", connection.ID, err)
			}
			return source
		},
		AddItem: func() { upstream.addAuto("Extra") },
		// IMAP is append-only: an update is a new message with a new UID.
		UpdateItem: func() { upstream.addAuto("Update") },
		AddMany: func(n int) {
			for range n {
				upstream.addAuto("Bulk")
			}
		},
	}
	ingestiontest.RunSourceSuite(t, harness)
}

var _ ports.CredentialProvider = imapSuiteCredentials{}
