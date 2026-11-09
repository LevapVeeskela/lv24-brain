package imap

// WP0.6 hardening tests for the native IMAP source: Check behavior,
// fetch-error retry, per-folder checkpoints, deletes policy, initial
// window, bounds/dates/settings/URL/credential handling. All fakes are
// deterministic; no live credentials or network.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestSourceCheckSuccessIsSideEffectFree(t *testing.T) {
	connection := sourceConnection()
	session := &fakeSession{
		states: map[string]MailboxState{"INBOX": {UIDValidity: 17, UIDNext: 3}},
		messages: map[string]map[imapv2.UID]FetchedMessage{"INBOX": {
			1: message(1, "One", "one"),
			2: message(2, "Two", "two"),
		}},
	}
	var gotAddress, gotUsername, gotPassword string
	source, err := NewSource(connection,
		sourceCredentialFake{connectionID: connection.ID, value: sourcePassword},
		func(_ context.Context, address, username, password string, _ *tls.Config) (Session, error) {
			gotAddress, gotUsername, gotPassword = address, username, password
			return session, nil
		},
		nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if gotAddress != "mail.test:993" || gotUsername != "alice" || gotPassword != sourcePassword {
		t.Fatalf("dial args = %q %q password-match=%v", gotAddress, gotUsername, gotPassword == sourcePassword)
	}
	if len(session.selects) != 1 || session.selects[0] != "INBOX" {
		t.Fatalf("selects = %v, want exactly [INBOX]", session.selects)
	}
	if len(session.searches) != 0 || len(session.fetches) != 0 {
		t.Fatalf("Check read messages: searches=%v fetches=%v", session.searches, session.fetches)
	}
	if session.logouts != 1 {
		t.Fatalf("logouts = %d, want 1", session.logouts)
	}
}

func TestSourceCheckAuthFailureHidesPassword(t *testing.T) {
	connection := sourceConnection()
	dialErr := fmt.Errorf("imap login: bad auth for alice with password %q", sourcePassword)
	source, err := NewSource(connection,
		sourceCredentialFake{connectionID: connection.ID, value: sourcePassword},
		func(context.Context, string, string, string, *tls.Config) (Session, error) {
			return nil, dialErr
		},
		nil)
	if err != nil {
		t.Fatal(err)
	}
	err = source.Check(context.Background())
	if err == nil {
		t.Fatal("Check succeeded with a failing dialer")
	}
	if strings.Contains(err.Error(), sourcePassword) {
		t.Fatalf("Check error leaked the password: %q", err)
	}
}

func TestSourceFetchErrorDiscardsPageAndRetriesFromStoredUID(t *testing.T) {
	connection := sourceConnection()
	session := &fakeSession{
		states: map[string]MailboxState{"INBOX": {UIDValidity: 17, UIDNext: 4}},
		messages: map[string]map[imapv2.UID]FetchedMessage{"INBOX": {
			1: message(1, "One", "one"),
			2: message(2, "Two", "two"),
			3: message(3, "Three", "three"),
		}},
		fetchErr: map[imapv2.UID]error{2: errors.New("connection dropped")},
	}
	source := newTestSource(t, connection, session)

	failed, err := source.Read(context.Background(), ports.ReadRequest{Connection: connection, Limit: 10})
	if err == nil {
		t.Fatal("Read succeeded with a failing fetch")
	}
	if strings.Contains(err.Error(), sourcePassword) {
		t.Fatalf("Read error leaked the password: %q", err)
	}
	if len(failed.Items) != 0 || len(failed.Checkpoints) != 0 {
		t.Fatalf("failed batch must be empty so the host discards it: items=%d checkpoints=%d",
			len(failed.Items), len(failed.Checkpoints))
	}
	if len(session.fetches) != 2 || session.fetches[0].UID != 1 || session.fetches[1].UID != 2 {
		t.Fatalf("fetches before failure = %+v, want [1 2]", session.fetches)
	}

	// The server recovers. The next run uses the same (empty) checkpoints
	// the host kept, so it must re-read from UID 1: no loss, no duplicate.
	delete(session.fetchErr, 2)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: connection, Limit: 10})
	if err != nil {
		t.Fatalf("retry Read: %v", err)
	}
	if len(batch.Items) != 3 || batch.More {
		t.Fatalf("retry batch = %d items more=%v, want all 3", len(batch.Items), batch.More)
	}
	for i, want := range []string{"INBOX:17:1", "INBOX:17:2", "INBOX:17:3"} {
		if batch.Items[i].Ref.ExternalID != want {
			t.Fatalf("retry item %d = %q, want %q", i, batch.Items[i].Ref.ExternalID, want)
		}
	}
	if len(session.fetches) != 5 || session.fetches[2].UID != 1 {
		t.Fatalf("retry did not restart from UID 1: fetches=%+v", session.fetches)
	}
	var cursor sourceFolderCursor
	if len(batch.Checkpoints) != 1 {
		t.Fatalf("checkpoints = %+v, want one", batch.Checkpoints)
	}
	if err := json.Unmarshal(batch.Checkpoints[0].Data, &cursor); err != nil || cursor.LastUID != 3 || cursor.UIDValidity != 17 {
		t.Fatalf("retry checkpoint = %+v err=%v", cursor, err)
	}
}

func TestSourcePerFolderCheckpointsAreIndependent(t *testing.T) {
	connection := sourceConnection()
	connection.Settings["folders"] = "INBOX, Archive"
	session := &fakeSession{
		states: map[string]MailboxState{
			"INBOX":   {UIDValidity: 17, UIDNext: 3},
			"Archive": {UIDValidity: 5, UIDNext: 2},
		},
		messages: map[string]map[imapv2.UID]FetchedMessage{
			"INBOX":   {1: message(1, "One", "one"), 2: message(2, "Two", "two")},
			"Archive": {1: message(1, "Archived", "archived")},
		},
	}
	source := newTestSource(t, connection, session)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: connection, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Items) != 3 || batch.More {
		t.Fatalf("batch = %d items more=%v, want 3 and done", len(batch.Items), batch.More)
	}
	gotIDs := map[string]bool{}
	for _, item := range batch.Items {
		gotIDs[item.Ref.ExternalID] = true
	}
	for _, want := range []string{"INBOX:17:1", "INBOX:17:2", "Archive:5:1"} {
		if !gotIDs[want] {
			t.Fatalf("external ids = %v, want %q (folder:validity:UID)", gotIDs, want)
		}
	}
	if len(batch.Checkpoints) != 2 {
		t.Fatalf("checkpoints = %+v, want one per folder", batch.Checkpoints)
	}
	streams := map[string]sourceFolderCursor{}
	for _, checkpoint := range batch.Checkpoints {
		var cursor sourceFolderCursor
		if err := json.Unmarshal(checkpoint.Data, &cursor); err != nil {
			t.Fatal(err)
		}
		streams[checkpoint.Stream] = cursor
	}
	inbox, ok := streams[sourceFolderStream("INBOX")]
	if !ok || inbox.UIDValidity != 17 || inbox.LastUID != 2 || inbox.Folder != "INBOX" {
		t.Fatalf("INBOX cursor = %+v present=%v", inbox, ok)
	}
	archive, ok := streams[sourceFolderStream("Archive")]
	if !ok || archive.UIDValidity != 5 || archive.LastUID != 1 || archive.Folder != "Archive" {
		t.Fatalf("Archive cursor = %+v present=%v", archive, ok)
	}
	if sourceFolderStream("INBOX") == sourceFolderStream("Archive") {
		t.Fatal("folder streams collide")
	}
}

func TestSourceUIDValidityChangeRecordsNewValidityAndRereads(t *testing.T) {
	connection := sourceConnection()
	stream := sourceFolderStream("INBOX")
	old, _ := json.Marshal(sourceFolderCursor{Folder: "INBOX", UIDValidity: 16, LastUID: 99})
	session := &fakeSession{
		states:   map[string]MailboxState{"INBOX": {UIDValidity: 17, UIDNext: 2}},
		messages: map[string]map[imapv2.UID]FetchedMessage{"INBOX": {1: message(1, "Reissued", "body")}},
	}
	batch, err := newTestSource(t, connection, session).Read(context.Background(), ports.ReadRequest{
		Connection:  connection,
		Checkpoints: map[string]domain.Checkpoint{stream: {Stream: stream, Version: sourceCheckpointVersion, Data: old}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(session.searches) != 1 || session.searches[0].After != 0 {
		t.Fatalf("search after validity change = %+v, want a reset to 0", session.searches)
	}
	if len(batch.Items) != 1 || batch.Items[0].Ref.ExternalID != "INBOX:17:1" {
		t.Fatalf("batch = %+v, want the re-read UID 1 under the new validity", batch.Items)
	}
	if len(batch.Checkpoints) != 1 {
		t.Fatalf("checkpoints = %+v, want the reset cursor", batch.Checkpoints)
	}
	var cursor sourceFolderCursor
	if err := json.Unmarshal(batch.Checkpoints[0].Data, &cursor); err != nil ||
		cursor.Folder != "INBOX" || cursor.UIDValidity != 17 || cursor.LastUID != 1 {
		t.Fatalf("reset checkpoint = %+v err=%v", cursor, err)
	}
}

func TestSourcePartialPageCheckpointCoversOnlyReturnedItems(t *testing.T) {
	connection := sourceConnection()
	session := &fakeSession{
		states: map[string]MailboxState{"INBOX": {UIDValidity: 17, UIDNext: 4}},
		messages: map[string]map[imapv2.UID]FetchedMessage{"INBOX": {
			1: message(1, "One", "one"),
			2: message(2, "Two", "two"),
			3: message(3, "Three", "three"),
		}},
	}
	source := newTestSource(t, connection, session)
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: connection, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || !first.More || first.Items[0].Ref.ExternalID != "INBOX:17:1" {
		t.Fatalf("first page = %+v more=%v", first.Items, first.More)
	}
	var cursor sourceFolderCursor
	if len(first.Checkpoints) != 1 {
		t.Fatalf("checkpoints = %+v, want the page cursor", first.Checkpoints)
	}
	if err := json.Unmarshal(first.Checkpoints[0].Data, &cursor); err != nil || cursor.LastUID != 1 {
		t.Fatalf("page checkpoint = %+v err=%v, want LastUID 1 (only returned items)", cursor, err)
	}
	second, err := source.Read(context.Background(), ports.ReadRequest{
		Connection:  connection,
		Limit:       1,
		Checkpoints: map[string]domain.Checkpoint{first.Checkpoints[0].Stream: first.Checkpoints[0]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].Ref.ExternalID != "INBOX:17:2" {
		t.Fatalf("second page = %+v, want UID 2 with no loss or duplicate", second.Items)
	}
	if len(session.searches) != 2 || session.searches[1].After != 1 {
		t.Fatalf("searches = %+v, want resume after UID 1", session.searches)
	}
}

func TestSourceDoesNotAdvertiseDeletesAndNeverTombstones(t *testing.T) {
	if Descriptor().Capabilities.Deletes {
		t.Fatal("IMAP descriptor advertises Deletes; the source must not infer deletions from absence")
	}
	connection := sourceConnection()
	session := &fakeSession{
		states: map[string]MailboxState{"INBOX": {UIDValidity: 17, UIDNext: 3}},
		messages: map[string]map[imapv2.UID]FetchedMessage{"INBOX": {
			1: message(1, "One", "one"),
			2: message(2, "Two", "two"),
		}},
	}
	source := newTestSource(t, connection, session)
	full, err := source.Read(context.Background(), ports.ReadRequest{Connection: connection, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Items) != 2 {
		t.Fatalf("backfill = %d items, want 2", len(full.Items))
	}
	// UID 2 vanishes upstream (expunged). A later read must report nothing,
	// never a tombstone: absence is not a deletion.
	delete(session.messages["INBOX"], 2)
	next, err := source.Read(context.Background(), ports.ReadRequest{
		Connection:  connection,
		Checkpoints: map[string]domain.Checkpoint{full.Checkpoints[0].Stream: full.Checkpoints[0]},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range next.Items {
		if item.Deleted {
			t.Fatalf("missing message surfaced as a tombstone: %+v", item.Ref)
		}
	}
	if len(next.Items) != 0 {
		t.Fatalf("items after expunge = %+v, want none (no resurrection, no tombstone)", next.Items)
	}
}

func TestSourceDescriptorHasNoDaysFieldAndStaysPersonalLocalOnly(t *testing.T) {
	descriptor := Descriptor()
	for _, field := range descriptor.ConfigFields {
		if field.Key == "days" {
			t.Fatal("descriptor has a days field; the initial window must stay a host/run option")
		}
	}
	password, ok := descriptor.Field("password")
	if !ok || !password.Secret || !password.Required {
		t.Fatalf("password field = %+v present=%v, want required secret", password, ok)
	}
	if !descriptor.PersonalData || descriptor.DefaultPublish != domain.PublishLocalOnly {
		t.Fatalf("descriptor = personal:%v publish:%q, want personal local-only",
			descriptor.PersonalData, descriptor.DefaultPublish)
	}
	if !descriptor.Capabilities.Backfill || !descriptor.Capabilities.Incremental {
		t.Fatalf("capabilities = %+v, want backfill+incremental without deletes", descriptor.Capabilities)
	}
}

// sinceFilteringSession is a fakeSession whose Search also applies the
// SINCE bound by internal date, like a real server.
type sinceFilteringSession struct {
	*fakeSession
}

func (s *sinceFilteringSession) Search(ctx context.Context, after imapv2.UID, since time.Time) ([]imapv2.UID, error) {
	uids, err := s.fakeSession.Search(ctx, after, since)
	if err != nil || since.IsZero() {
		return uids, err
	}
	kept := uids[:0]
	for _, uid := range uids {
		msg := s.messages[s.selected][uid]
		internal := msg.InternalDate
		if internal.IsZero() && msg.Envelope != nil {
			internal = msg.Envelope.Date
		}
		if internal.IsZero() || !internal.Before(since) {
			kept = append(kept, uid)
		}
	}
	return kept, nil
}

func TestSourceInitialWindowSinceNarrowsFirstReadOnly(t *testing.T) {
	connection := sourceConnection()
	oldDate := time.Date(2026, time.January, 2, 10, 0, 0, 0, time.UTC)
	newDate := time.Date(2026, time.November, 1, 10, 0, 0, 0, time.UTC)
	inner := &fakeSession{
		states: map[string]MailboxState{"INBOX": {UIDValidity: 17, UIDNext: 3}},
		messages: map[string]map[imapv2.UID]FetchedMessage{"INBOX": {
			1: {UID: 1, Envelope: &imapv2.Envelope{Subject: "Old", Date: oldDate}, InternalDate: oldDate, Body: []byte("old")},
			2: {UID: 2, Envelope: &imapv2.Envelope{Subject: "New", Date: newDate}, InternalDate: newDate, Body: []byte("new")},
		}},
	}
	session := &sinceFilteringSession{fakeSession: inner}
	source := newTestSource(t, connection, session)

	// The host sets Since = now - InitialWindow on the first run when no
	// checkpoint exists yet. Old history is narrowed out by design.
	since := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: connection, Since: &since})
	if err != nil {
		t.Fatal(err)
	}
	if len(inner.searches) != 1 || !inner.searches[0].Since.Equal(since) {
		t.Fatalf("searches = %+v, want Since passed to the server", inner.searches)
	}
	if len(first.Items) != 1 || first.Items[0].Ref.ExternalID != "INBOX:17:2" {
		t.Fatalf("first read = %+v, want only the in-window message", first.Items)
	}

	// Once a checkpoint exists the source resumes by UID and ignores Since,
	// so a later window never hides already-seen mail or deletes anything.
	future := time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)
	second, err := source.Read(context.Background(), ports.ReadRequest{
		Connection:  connection,
		Since:       &future,
		Checkpoints: map[string]domain.Checkpoint{first.Checkpoints[0].Stream: first.Checkpoints[0]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inner.searches) != 2 || !inner.searches[1].Since.IsZero() {
		t.Fatalf("searches = %+v, want Since ignored once the UID cursor exists", inner.searches)
	}
	if len(second.Items) != 0 {
		t.Fatalf("second read = %+v, want nothing new and no tombstones", second.Items)
	}
}

func TestSourceDoesNotTouchLegacyStateFile(t *testing.T) {
	t.Chdir(t.TempDir())
	connection := sourceConnection()
	session := &fakeSession{
		states:   map[string]MailboxState{"INBOX": {UIDValidity: 17, UIDNext: 2}},
		messages: map[string]map[imapv2.UID]FetchedMessage{"INBOX": {1: message(1, "One", "one")}},
	}
	if _, err := newTestSource(t, connection, session).Read(context.Background(), ports.ReadRequest{Connection: connection}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("source wrote files into the working directory: %v", entries)
	}
}

func TestSourceBoundsSnippetAndSanitizesUTF8(t *testing.T) {
	connection := sourceConnection()
	raw := append([]byte("hello "), 0xff, 0xfe, 0x00)
	raw = append(raw, []byte(strings.Repeat("я ", 2000))...)
	session := &fakeSession{
		states: map[string]MailboxState{"INBOX": {UIDValidity: 17, UIDNext: 2}},
		messages: map[string]map[imapv2.UID]FetchedMessage{"INBOX": {
			1: {UID: 1, Envelope: &imapv2.Envelope{Subject: "Dirty"}, Header: []byte("Content-Type: text/plain\r\n\r\n"), Body: raw},
		}},
	}
	batch, err := newTestSource(t, connection, session).Read(context.Background(), ports.ReadRequest{Connection: connection})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(batch.Items))
	}
	body := batch.Items[0].Body
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		t.Fatalf("body is not clean UTF-8: %q", body)
	}
	if n := len([]rune(body)); n > DefaultSnippetRunes {
		t.Fatalf("snippet is %d runes, want at most %d", n, DefaultSnippetRunes)
	}
	if len(session.fetches) != 1 || session.fetches[0].MaxBytes != fetchBodyBytes {
		t.Fatalf("fetches = %+v, want one bounded fetch of %d bytes", session.fetches, fetchBodyBytes)
	}
}

func TestSourceMessageDatesFallBack(t *testing.T) {
	envelope := time.Date(2026, time.November, 1, 10, 0, 0, 0, time.UTC)
	internal := time.Date(2026, time.November, 1, 10, 5, 0, 0, time.UTC)
	cases := []struct {
		name        string
		message     FetchedMessage
		wantCreated time.Time
		wantUpdated time.Time
	}{
		{"both", FetchedMessage{UID: 1, Envelope: &imapv2.Envelope{Subject: "S", Date: envelope}, InternalDate: internal}, envelope, internal},
		{"envelope only", FetchedMessage{UID: 1, Envelope: &imapv2.Envelope{Subject: "S", Date: envelope}}, envelope, envelope},
		{"internal only", FetchedMessage{UID: 1, Envelope: &imapv2.Envelope{Subject: "S"}, InternalDate: internal}, internal, internal},
		{"neither", FetchedMessage{UID: 1, Envelope: &imapv2.Envelope{Subject: "S"}}, time.Time{}, time.Time{}},
		{"no envelope", FetchedMessage{UID: 1, InternalDate: internal}, internal, internal},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			item := sourceItem("mail-work", "INBOX", 17, testCase.message)
			if !item.CreatedAt.Equal(testCase.wantCreated) || !item.UpdatedAt.Equal(testCase.wantUpdated) {
				t.Fatalf("created=%v updated=%v, want %v/%v",
					item.CreatedAt, item.UpdatedAt, testCase.wantCreated, testCase.wantUpdated)
			}
		})
	}
}

func TestSourceParsesFolderList(t *testing.T) {
	connection := sourceConnection()
	connection.Settings["folders"] = "INBOX, Archive\nSent ,, INBOX"
	source, err := NewSource(connection, sourceCredentialFake{connectionID: connection.ID, value: sourcePassword}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.folders) != 3 || source.folders[0] != "Archive" || source.folders[1] != "INBOX" || source.folders[2] != "Sent" {
		t.Fatalf("folders = %q, want [Archive INBOX Sent] deduplicated and sorted", source.folders)
	}

	connection.Settings["folders"] = ""
	source, err = NewSource(connection, sourceCredentialFake{connectionID: connection.ID, value: sourcePassword}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.folders) != 1 || source.folders[0] != DefaultFolder {
		t.Fatalf("folders = %q, want the INBOX default", source.folders)
	}

	for _, raw := range []string{"INBOX\x00evil", strings.Repeat("f", 256)} {
		connection.Settings["folders"] = raw
		if _, err := NewSource(connection, sourceCredentialFake{connectionID: connection.ID, value: sourcePassword}, nil, nil); err == nil {
			t.Fatalf("folders %q accepted", raw)
		}
	}
}

func TestSourceBaseURLForms(t *testing.T) {
	connection := sourceConnection()
	cases := []struct {
		baseURL string
		address string
		ok      bool
	}{
		{"imaps://mail.test", "mail.test:993", true},
		{"imaps://mail.test:993", "mail.test:993", true},
		{"imaps://mail.test:1993", "mail.test:1993", true},
		{"imaps://mail.test:993/", "mail.test:993", true},
		{"http://mail.test:993", "", false},
		{"imaps://user@mail.test:993", "", false},
		{"imaps://mail.test:993?x=1", "", false},
		{"imaps://mail.test:993#frag", "", false},
		{"imaps://mail.test/box", "", false},
		{"mail.test:993", "", false},
		{"", "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.baseURL, func(t *testing.T) {
			connection.BaseURL = testCase.baseURL
			source, err := NewSource(connection, sourceCredentialFake{connectionID: connection.ID, value: sourcePassword}, nil, nil)
			if !testCase.ok {
				if err == nil {
					t.Fatalf("base URL %q accepted", testCase.baseURL)
				}
				if !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("error = %v, want a validation error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("base URL %q: %v", testCase.baseURL, err)
			}
			if source.address != testCase.address {
				t.Fatalf("address = %q, want %q", source.address, testCase.address)
			}
		})
	}
}

func TestSourcePasswordComesFromCredentialField(t *testing.T) {
	connection := sourceConnection()
	source := newTestSource(t, connection, &fakeSession{states: map[string]MailboxState{"INBOX": {UIDValidity: 1}}})

	empty, err := NewSource(connection, sourceCredentialFake{connectionID: connection.ID}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Read(context.Background(), ports.ReadRequest{Connection: connection}); err == nil ||
		!strings.Contains(err.Error(), "credential is empty") {
		t.Fatalf("empty password error = %v", err)
	}

	missing, err := NewSource(connection,
		sourceCredentialFake{connectionID: "someone-else", value: sourcePassword}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Check(context.Background()); err == nil {
		t.Fatal("Check succeeded without a password for this connection")
	}
	_ = source
}
