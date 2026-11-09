package sourceoauth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultFlowTimeout = 5 * time.Minute

// Flow performs an RFC 8252 loopback authorization-code flow with PKCE S256.
// Port zero selects an ephemeral loopback port. NewFlow selects the fixed
// default port for callers that do not need to override it.
type Flow struct {
	Provider     Provider
	ClientID     string
	ClientSecret string
	Scopes       []string
	Port         int
	Timeout      time.Duration
	Client       *http.Client
	Now          func() time.Time
}

// NewFlow returns a PKCE flow configured for the fixed default callback port.
func NewFlow(provider Provider, clientID string) Flow {
	return Flow{Provider: provider, ClientID: clientID, Port: DefaultLoopbackPort}
}

type callbackResult struct {
	code string
	err  error
}

// Authorize opens the system browser through open, waits for a state-validated
// loopback callback, and exchanges the authorization code. open should invoke
// the system browser, never an embedded webview.
func (f Flow) Authorize(ctx context.Context, open func(authURL string) error) (Bundle, error) {
	if open == nil || strings.TrimSpace(f.ClientID) == "" || len(f.ClientID) > 4096 || len(f.ClientSecret) > 16<<10 {
		return Bundle{}, errors.New("sourceoauth: flow is not configured")
	}
	if err := ValidateProvider(f.Provider); err != nil || !f.Provider.PKCE {
		return Bundle{}, errors.New("sourceoauth: PKCE provider is not configured")
	}
	port := f.Port
	if port < 0 || port > 65535 {
		return Bundle{}, errors.New("sourceoauth: invalid loopback port")
	}
	flowCtx, cancel := boundedContext(ctx, f.Timeout, defaultFlowTimeout)
	defer cancel()
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return Bundle{}, errors.New("sourceoauth: loopback listener could not be started (port may be busy)")
	}
	defer listener.Close()
	redirectURI := "http://" + listener.Addr().String() + "/callback"
	state, err := randomHex(32)
	if err != nil {
		return Bundle{}, err
	}
	verifier, err := pkceVerifier()
	if err != nil {
		return Bundle{}, err
	}
	callback := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Authorization response rejected.", http.StatusMethodNotAllowed)
			return
		}
		query := r.URL.Query()
		gotState := query.Get("state")
		result := callbackResult{}
		switch {
		case gotState == "" || !constantTimeEqual(gotState, state):
			result.err = errors.New("sourceoauth: OAuth state validation failed")
		case query.Get("error") != "":
			result.err = errors.New("sourceoauth: authorization was not granted")
		case query.Get("code") == "" || len(query.Get("code")) > 16<<10:
			result.err = errors.New("sourceoauth: invalid authorization response")
		default:
			result.code = query.Get("code")
		}
		if result.err != nil {
			http.Error(w, "Authorization failed. You may close this window.", http.StatusBadRequest)
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("Authorization complete. You may close this window."))
		}
		select {
		case callback <- result:
		default:
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	authorizeURL, err := url.Parse(f.Provider.AuthorizeURL)
	if err != nil { // ValidateProvider already checked it; keep failures sanitized.
		return Bundle{}, errors.New("sourceoauth: invalid authorization endpoint")
	}
	params := authorizeURL.Query()
	params.Set("client_id", f.ClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("state", state)
	params.Set("code_challenge", challenge(verifier))
	params.Set("code_challenge_method", "S256")
	if scopes := scopesOrDefault(f.Scopes, f.Provider.DefaultScopes); scopes != "" {
		params.Set("scope", scopes)
	}
	authorizeURL.RawQuery = params.Encode()
	openResult := make(chan error, 1)
	go func() { openResult <- open(authorizeURL.String()) }()
	var result callbackResult
	opened := false
	for !opened {
		select {
		case err := <-openResult:
			if err != nil {
				return Bundle{}, errors.New("sourceoauth: system browser could not be opened")
			}
			opened = true
		case result = <-callback:
			if result.err != nil {
				return Bundle{}, result.err
			}
			opened = true
		case <-flowCtx.Done():
			return Bundle{}, errors.New("sourceoauth: authorization timed out or was canceled")
		}
	}
	if result.code == "" {
		select {
		case result = <-callback:
		case <-flowCtx.Done():
			return Bundle{}, errors.New("sourceoauth: authorization timed out or was canceled")
		}
	}
	if result.err != nil {
		return Bundle{}, result.err
	}
	values := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {result.code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	bundle, _, err := exchange(flowCtx, newHTTPClient(f.Client), f.Provider, f.ClientID, f.ClientSecret, values, now())
	if err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

// DeviceFlow performs OAuth 2.0 device authorization. Sleep and Now are
// injectable for deterministic tests; production defaults are context-aware.
type DeviceFlow struct {
	Provider     Provider
	ClientID     string
	ClientSecret string
	Scopes       []string
	Client       *http.Client
	Now          func() time.Time
	Sleep        func(context.Context, time.Duration) error
	MaxDuration  time.Duration
}

type deviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

// Device requests a device code, calls prompt exactly once, then polls until
// success, denial, cancellation, or the bounded authorization lifetime ends.
func (f DeviceFlow) Device(ctx context.Context, prompt func(DevicePrompt) error) (Bundle, error) {
	if prompt == nil || strings.TrimSpace(f.ClientID) == "" || len(f.ClientID) > 4096 || len(f.ClientSecret) > 16<<10 {
		return Bundle{}, errors.New("sourceoauth: device flow is not configured")
	}
	if err := ValidateProvider(f.Provider); err != nil || !f.Provider.Device {
		return Bundle{}, errors.New("sourceoauth: device provider is not configured")
	}
	client := newHTTPClient(f.Client)
	values := url.Values{}
	if scopes := scopesOrDefault(f.Scopes, f.Provider.DefaultScopes); scopes != "" {
		values.Set("scope", scopes)
	}
	response, err := postForm(ctx, client, f.Provider.DeviceURL, values, f.ClientID, "", TokenAuthForm)
	if err != nil {
		return Bundle{}, err
	}
	body, readErr := readBounded(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return Bundle{}, errors.New("sourceoauth: device authorization request failed")
	}
	var authorization deviceAuthorization
	if err := decodeDeviceResponse(body, response.Header.Get("Content-Type"), &authorization); err != nil {
		return Bundle{}, errors.New("sourceoauth: invalid device authorization response")
	}
	if len(authorization.DeviceCode) == 0 || len(authorization.DeviceCode) > 16<<10 || len(authorization.UserCode) == 0 || len(authorization.UserCode) > 256 || len(authorization.VerificationURI) > 4096 || len(authorization.VerificationURIComplete) > 4096 || authorization.ExpiresIn <= 0 || authorization.ExpiresIn > 24*60*60 {
		return Bundle{}, errors.New("sourceoauth: invalid device authorization response")
	}
	if !validVerificationURL(authorization.VerificationURI) || (authorization.VerificationURIComplete != "" && !validVerificationURL(authorization.VerificationURIComplete)) {
		return Bundle{}, errors.New("sourceoauth: invalid device authorization response")
	}
	interval := time.Duration(authorization.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	maxDuration := f.MaxDuration
	if maxDuration <= 0 || maxDuration > 15*time.Minute {
		maxDuration = 15 * time.Minute
	}
	expires := time.Duration(authorization.ExpiresIn) * time.Second
	if expires > maxDuration {
		expires = maxDuration
	}
	parentCtx := ctx
	flowCtx, cancel := context.WithTimeout(ctx, expires)
	defer cancel()
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	deadline := now().Add(expires)
	promptInfo := DevicePrompt{
		UserCode: authorization.UserCode, VerificationURI: authorization.VerificationURI,
		VerificationURIComplete: authorization.VerificationURIComplete, ExpiresIn: expires,
	}
	if err := prompt(promptInfo); err != nil {
		return Bundle{}, errors.New("sourceoauth: device authorization prompt failed")
	}
	sleep := f.Sleep
	if sleep == nil {
		sleep = contextSleep
	}
	for {
		if parentCtx.Err() != nil {
			return Bundle{}, errors.New("sourceoauth: device authorization canceled")
		}
		if flowCtx.Err() != nil {
			return Bundle{}, errors.New("sourceoauth: device authorization expired")
		}
		if !now().Before(deadline) {
			return Bundle{}, errors.New("sourceoauth: device authorization expired")
		}
		pollValues := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {authorization.DeviceCode},
		}
		resp, requestErr := postForm(flowCtx, client, f.Provider.TokenURL, pollValues, f.ClientID, f.ClientSecret, f.Provider.TokenAuthStyle)
		if requestErr != nil {
			if parentCtx.Err() != nil {
				return Bundle{}, errors.New("sourceoauth: device authorization canceled")
			}
			if flowCtx.Err() != nil {
				return Bundle{}, errors.New("sourceoauth: device authorization expired")
			}
			return Bundle{}, requestErr
		}
		responseBody, err := readBounded(resp.Body)
		contentType := resp.Header.Get("Content-Type")
		_ = resp.Body.Close()
		if err != nil {
			return Bundle{}, err
		}
		bundle, code, parseErr := parseTokenResponse(responseBody, contentType, now())
		if parseErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return bundle, nil
		}
		switch code {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "expired_token":
			return Bundle{}, errors.New("sourceoauth: device authorization expired")
		case "access_denied":
			return Bundle{}, errors.New("sourceoauth: device authorization was denied")
		default:
			if errors.Is(parseErr, ErrReauthRequired) {
				return Bundle{}, ErrReauthRequired
			}
			return Bundle{}, errors.New("sourceoauth: device token request failed")
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return Bundle{}, errors.New("sourceoauth: device authorization expired")
		}
		wait := interval
		if wait > remaining {
			wait = remaining
		}
		if err := sleep(flowCtx, wait); err != nil {
			if parentCtx.Err() == nil && flowCtx.Err() != nil {
				return Bundle{}, errors.New("sourceoauth: device authorization expired")
			}
			return Bundle{}, errors.New("sourceoauth: device authorization canceled")
		}
	}
}

func decodeDeviceResponse(body []byte, contentType string, target *deviceAuthorization) error {
	if strings.Contains(strings.ToLower(contentType), "application/x-www-form-urlencoded") {
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return err
		}
		target.DeviceCode = values.Get("device_code")
		target.UserCode = values.Get("user_code")
		target.VerificationURI = values.Get("verification_uri")
		target.VerificationURIComplete = values.Get("verification_uri_complete")
		if raw := values.Get("expires_in"); raw != "" {
			target.ExpiresIn, err = strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return err
			}
		}
		if raw := values.Get("interval"); raw != "" {
			target.Interval, err = strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return err
			}
		}
		return nil
	}
	return json.Unmarshal(body, target)
}

func validVerificationURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		return host == "localhost" || (ip != nil && ip.IsLoopback())
	}
	return false
}

func boundedContext(parent context.Context, requested, fallback time.Duration) (context.Context, context.CancelFunc) {
	if requested <= 0 {
		requested = fallback
	}
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= requested {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, requested)
}

func contextSleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
