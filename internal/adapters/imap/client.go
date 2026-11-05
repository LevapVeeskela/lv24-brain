package imap

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sort"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// MailboxState is the read-only SELECT result used for UIDVALIDITY tracking.
type MailboxState struct {
	UIDValidity uint32
	UIDNext     imapv2.UID
	NumMessages uint32
}

// FetchedMessage is metadata plus a bounded header and text body. The body is
// never stored; callers immediately reduce it to a snippet.
type FetchedMessage struct {
	UID          imapv2.UID
	Envelope     *imapv2.Envelope
	InternalDate time.Time
	Size         int64
	Header       []byte
	Body         []byte
	// Snippet is populated by the connector from Header and Body; the raw
	// bytes are cleared immediately after extraction.
	Snippet string
}

// Session is the minimal read-only IMAP surface used by the connector. It has
// no send, delete, move, append, copy or flag operations by construction.
type Session interface {
	Select(ctx context.Context, mailbox string) (MailboxState, error)
	Search(ctx context.Context, after imapv2.UID, since time.Time) ([]imapv2.UID, error)
	Fetch(ctx context.Context, uid imapv2.UID, maxBytes int64) (FetchedMessage, error)
	Logout(ctx context.Context) error
}

// DialFunc opens an authenticated read-only session.
type DialFunc func(ctx context.Context, address, username, password string, tlsConfig *tls.Config) (Session, error)

// DialTLS opens a TLS IMAP connection and logs in. It never sends a mutating
// command.
func DialTLS(ctx context.Context, address, username, password string, tlsConfig *tls.Config) (Session, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("imap address: %w", err)
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("connect imap: %w", err)
	}

	config := &tls.Config{}
	if tlsConfig != nil {
		config = tlsConfig.Clone()
	}
	if config.ServerName == "" {
		config.ServerName = host
	}
	if config.MinVersion == 0 {
		config.MinVersion = tls.VersionTLS12
	}
	tlsConn := tls.Client(conn, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("imap tls handshake: %w", err)
	}

	client := imapclient.New(tlsConn, &imapclient.Options{TLSConfig: config, Dialer: dialer})
	session := &realSession{client: client}
	if _, err := await(ctx, client, func() (struct{}, error) {
		return struct{}{}, client.WaitGreeting()
	}); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("imap greeting: %w", err)
	}
	if _, err := await(ctx, client, func() (struct{}, error) {
		return struct{}{}, client.Login(username, password).Wait()
	}); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("imap login: %w", err)
	}
	return session, nil
}

type realSession struct {
	client *imapclient.Client
}

func (s *realSession) Select(ctx context.Context, mailbox string) (MailboxState, error) {
	data, err := await(ctx, s.client, func() (*imapv2.SelectData, error) {
		return s.client.Select(mailbox, &imapv2.SelectOptions{ReadOnly: true}).Wait()
	})
	if err != nil {
		return MailboxState{}, err
	}
	if data == nil {
		return MailboxState{}, fmt.Errorf("imap select %q returned no data", mailbox)
	}
	return MailboxState{
		UIDValidity: data.UIDValidity,
		UIDNext:     data.UIDNext,
		NumMessages: data.NumMessages,
	}, nil
}

func (s *realSession) Search(ctx context.Context, after imapv2.UID, since time.Time) ([]imapv2.UID, error) {
	criteria := &imapv2.SearchCriteria{}
	if after > 0 {
		set := imapv2.UIDSet{}
		set.AddRange(after+1, 0)
		criteria.UID = []imapv2.UIDSet{set}
	}
	if !since.IsZero() {
		criteria.Since = since.UTC()
	}
	data, err := await(ctx, s.client, func() (*imapv2.SearchData, error) {
		return s.client.UIDSearch(criteria, &imapv2.SearchOptions{}).Wait()
	})
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("imap search returned no data")
	}
	uids := data.AllUIDs()
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	return uids, nil
}

func (s *realSession) Fetch(ctx context.Context, uid imapv2.UID, maxBytes int64) (FetchedMessage, error) {
	if maxBytes <= 0 {
		maxBytes = 8192
	}
	headerSection := &imapv2.FetchItemBodySection{
		Specifier: imapv2.PartSpecifierHeader,
		Peek:      true,
		Partial:   &imapv2.SectionPartial{Offset: 0, Size: maxBytes},
	}
	textSection := &imapv2.FetchItemBodySection{
		Specifier: imapv2.PartSpecifierText,
		Peek:      true,
		Partial:   &imapv2.SectionPartial{Offset: 0, Size: maxBytes},
	}
	options := &imapv2.FetchOptions{
		Envelope:     true,
		InternalDate: true,
		RFC822Size:   true,
		UID:          true,
		BodySection:  []*imapv2.FetchItemBodySection{headerSection, textSection},
	}
	messages, err := await(ctx, s.client, func() ([]*imapclient.FetchMessageBuffer, error) {
		return s.client.Fetch(imapv2.UIDSetNum(uid), options).Collect()
	})
	if err != nil {
		return FetchedMessage{}, err
	}
	for _, message := range messages {
		if message == nil || message.UID != uid {
			continue
		}
		return FetchedMessage{
			UID:          message.UID,
			Envelope:     message.Envelope,
			InternalDate: message.InternalDate,
			Size:         message.RFC822Size,
			Header:       message.FindBodySection(headerSection),
			Body:         message.FindBodySection(textSection),
		}, nil
	}
	return FetchedMessage{}, fmt.Errorf("imap fetch uid %d: message not found", uid)
}

func (s *realSession) Logout(ctx context.Context) error {
	if s.client == nil {
		return nil
	}
	select {
	case <-s.client.Closed():
		return nil
	default:
	}
	_, logoutErr := await(ctx, s.client, func() (struct{}, error) {
		return struct{}{}, s.client.Logout().Wait()
	})
	closeErr := s.client.Close()
	if logoutErr != nil {
		return logoutErr
	}
	return closeErr
}

// await runs one blocking IMAP command and closes the client if ctx ends so a
// hung server cannot ignore cancellation.
func await[T any](ctx context.Context, client *imapclient.Client, fn func() (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := fn()
		done <- result{value: value, err: err}
	}()
	select {
	case out := <-done:
		return out.value, out.err
	case <-ctx.Done():
		_ = client.Close()
		<-done
		var zero T
		return zero, ctx.Err()
	}
}
