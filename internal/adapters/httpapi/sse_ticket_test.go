package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// sseStatus opens GET /v1/events with the given query and returns the status,
// closing the stream immediately.
func sseStatus(t *testing.T, baseURL string, query url.Values) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/events?"+query.Encode(), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestEventStreamTicketIsSingleUse(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	session := loginAs(t, ts.URL, adminLogin, adminPassword)

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/events/ticket", nil, "")
	requireStatus(t, resp, payload, http.StatusUnauthorized)

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/events/ticket", nil, session.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)
	var body struct {
		Ticket    string    `json:"ticket"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.Ticket == "" || body.ExpiresAt.IsZero() {
		t.Fatalf("ticket body = %s, %v", payload, err)
	}

	if status := sseStatus(t, ts.URL, url.Values{"ticket": {body.Ticket}}); status != http.StatusOK {
		t.Fatalf("first redeem status = %d, want 200", status)
	}
	if status := sseStatus(t, ts.URL, url.Values{"ticket": {body.Ticket}}); status != http.StatusUnauthorized {
		t.Fatalf("second redeem status = %d, want 401", status)
	}
}

func TestEventStreamRejectsAccessTokenInURL(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	session := loginAs(t, ts.URL, adminLogin, adminPassword)
	if status := sseStatus(t, ts.URL, url.Values{"access_token": {session.AccessToken}}); status != http.StatusUnauthorized {
		t.Fatalf("access_token query status = %d, want 401", status)
	}
	if status := sseStatus(t, ts.URL, url.Values{"ticket": {session.AccessToken}}); status != http.StatusUnauthorized {
		t.Fatalf("access token as ticket status = %d, want 401", status)
	}
}

func TestSSETicketsExpireAndAreBounded(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	tickets := newSSETickets(clock)
	principal := auth.Principal{User: domain.User{ID: "user_1", Role: domain.RoleViewer}}
	ticket, _, err := tickets.issue(principal)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	mu.Lock()
	now = now.Add(sseTicketTTL)
	mu.Unlock()
	if _, ok := tickets.redeem(ticket); ok {
		t.Fatal("expired ticket redeemed")
	}

	for range sseTicketMax {
		if _, _, err := tickets.issue(principal); err != nil {
			t.Fatalf("issue under bound: %v", err)
		}
	}
	if _, _, err := tickets.issue(principal); err == nil {
		t.Fatal("issue beyond bound succeeded")
	}
	// Expired tickets are pruned, freeing capacity.
	mu.Lock()
	now = now.Add(sseTicketTTL)
	mu.Unlock()
	if _, _, err := tickets.issue(principal); err != nil {
		t.Fatalf("issue after expiry: %v", err)
	}
}
