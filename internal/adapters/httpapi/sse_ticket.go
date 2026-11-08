package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/auth"
)

const (
	// sseTicketTTL bounds how long an unused event-stream ticket is valid.
	sseTicketTTL = time.Minute
	// sseTicketMax bounds outstanding tickets so the store cannot grow
	// without limit.
	sseTicketMax = 4096
	// sseTicketPrefix makes tickets recognisable and distinct from JWTs and
	// API tokens.
	sseTicketPrefix = "sset_"
)

var errTooManySSETickets = errors.New("too many outstanding event-stream tickets")

// sseTickets holds short-lived, single-use credentials for GET /v1/events.
// EventSource cannot send an Authorization header, and a long-lived access
// token in the URL ends up in proxy logs and browser history; a ticket is
// useless once redeemed or after sseTicketTTL.
type sseTickets struct {
	mu      sync.Mutex
	now     func() time.Time
	tickets map[string]sseTicket
}

type sseTicket struct {
	principal auth.Principal
	grant     *tokenGrant
	expires   time.Time
}

func newSSETickets(now func() time.Time) *sseTickets {
	if now == nil {
		now = time.Now
	}
	return &sseTickets{now: now, tickets: make(map[string]sseTicket)}
}

func (s *sseTickets) issue(principal auth.Principal) (string, time.Time, error) {
	return s.issueGrant(principal, nil)
}

// issueGrant issues a ticket carrying the caller's API token grant, so a
// stream opened with it keeps the token's scopes and project allow-list.
func (s *sseTickets) issueGrant(principal auth.Principal, grant *tokenGrant) (string, time.Time, error) {
	raw, err := auth.RandomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	ticket := sseTicketPrefix + raw
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for key, existing := range s.tickets {
		if !now.Before(existing.expires) {
			delete(s.tickets, key)
		}
	}
	if len(s.tickets) >= sseTicketMax {
		return "", time.Time{}, errTooManySSETickets
	}
	expires := now.Add(sseTicketTTL)
	s.tickets[ticket] = sseTicket{principal: principal, grant: grant, expires: expires}
	return ticket, expires, nil
}

// redeem consumes a ticket and returns its principal.
func (s *sseTickets) redeem(ticket string) (auth.Principal, bool) {
	principal, _, ok := s.redeemGrant(ticket)
	return principal, ok
}

// redeemGrant consumes a ticket and returns its principal and token grant.
func (s *sseTickets) redeemGrant(ticket string) (auth.Principal, *tokenGrant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.tickets[ticket]
	if !ok {
		return auth.Principal{}, nil, false
	}
	delete(s.tickets, ticket)
	if !s.now().Before(entry.expires) {
		return auth.Principal{}, nil, false
	}
	return entry.principal, entry.grant, true
}

func (a *API) createEventTicket(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeAuthError(w)
		return
	}
	ticket, expires, err := a.sseTickets.issueGrant(principal, grantFrom(r.Context()))
	if errors.Is(err, errTooManySSETickets) {
		writeErrorCode(w, http.StatusTooManyRequests, "rate_limited", "too many outstanding event-stream tickets")
		return
	}
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ticket": ticket, "expiresAt": expires.UTC()})
}

// eventStreamPrincipal authenticates GET /v1/events: an Authorization header,
// or a single-use ticket from POST /v1/events/ticket in the query string.
func (a *API) eventStreamPrincipal(r *http.Request) (auth.Principal, *tokenGrant, error) {
	if bearerToken(r) != "" {
		return a.authenticate(r)
	}
	ticket := strings.TrimSpace(r.URL.Query().Get("ticket"))
	if ticket == "" {
		return auth.Principal{}, nil, auth.ErrInvalidToken
	}
	principal, grant, ok := a.sseTickets.redeemGrant(ticket)
	if !ok {
		return auth.Principal{}, nil, auth.ErrInvalidToken
	}
	return principal, grant, nil
}
