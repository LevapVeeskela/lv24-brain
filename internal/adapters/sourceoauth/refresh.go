package sourceoauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Refresher returns access tokens from a TokenStore and rotates credentials
// when they expire. Its mutex provides one refresh at a time per instance.
type Refresher struct {
	Provider     Provider
	ClientID     string
	ClientSecret string
	Store        TokenStore
	Client       *http.Client
	Now          func() time.Time
	mu           sync.Mutex
}

// Token returns a stored access token unless it expires within the 60-second
// safety skew. A refresh token rotation is persisted before the new token is
// returned to any caller.
func (r *Refresher) Token(ctx context.Context) (string, error) {
	if r == nil || strings.TrimSpace(r.ClientID) == "" || len(r.ClientID) > 4096 || len(r.ClientSecret) > 16<<10 || r.Store.Secrets == nil || strings.TrimSpace(r.Store.Key) == "" {
		return "", errors.New("sourceoauth: refresher is not configured")
	}
	if err := ValidateProvider(r.Provider); err != nil {
		return "", errors.New("sourceoauth: refresh provider is not configured")
	}
	bundle, err := r.Store.Load(ctx)
	if err != nil {
		return "", err
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	if tokenUsable(bundle, now()) {
		return bundle.AccessToken, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Another caller may already have refreshed while this caller waited.
	bundle, err = r.Store.Load(ctx)
	if err != nil {
		return "", err
	}
	if tokenUsable(bundle, now()) {
		return bundle.AccessToken, nil
	}
	if bundle.RefreshToken == "" {
		return "", ErrReauthRequired
	}
	values := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {bundle.RefreshToken},
	}
	refreshed, code, err := exchange(ctx, newHTTPClient(r.Client), r.Provider, r.ClientID, r.ClientSecret, values, now())
	if err != nil {
		if code == "invalid_grant" || errors.Is(err, ErrReauthRequired) {
			return "", ErrReauthRequired
		}
		return "", err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = bundle.RefreshToken
	}
	if refreshed.TokenType == "" {
		refreshed.TokenType = bundle.TokenType
	}
	if len(refreshed.Scopes) == 0 {
		refreshed.Scopes = append([]string(nil), bundle.Scopes...)
	}
	if err := r.Store.Save(ctx, refreshed); err != nil {
		return "", err
	}
	return refreshed.AccessToken, nil
}

func tokenUsable(bundle Bundle, now time.Time) bool {
	return bundle.Expiry.IsZero() || bundle.Expiry.After(now.Add(60*time.Second))
}
