package httpclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Secret store keys shared with the installer and the CLI.
const (
	// SecretSession holds the login-managed credential bundle as one JSON
	// value: {"serverUrl":...,"accessToken":...,"refreshToken":...}. A single
	// SecretStore.Get/Set keeps the server binding and the token pair atomic,
	// so a concurrent command can never observe a new binding with an old
	// token or vice versa.
	SecretSession = "server-session"
	// SecretAccessToken is the legacy static API token. It is an explicit
	// static credential: it has no refresh token and no server binding, and
	// refresh is unavailable while only this key exists.
	SecretAccessToken = "server-token"
	// SecretRefreshToken and SecretAuthServerURL were written by releases
	// that stored the login pair as three separate keys. They are never read
	// anymore; clear removes them on a best-effort basis.
	SecretRefreshToken  = "server-refresh-token"
	SecretAuthServerURL = "server-auth-url"
)

const (
	refreshLockDirName  = "lv24-brain"
	refreshLockFileName = "auth-refresh.lock"
	// defaultRefreshLockWait bounds how long one process waits for another to
	// finish rotating the shared refresh token. It must exceed the default
	// HTTP timeout (30s) so a waiter never gives up while the lock owner is
	// still allowed to finish its refresh request.
	defaultRefreshLockWait = 35 * time.Second
	// defaultRefreshLockPoll is the interval between lock acquisition attempts.
	defaultRefreshLockPoll = 50 * time.Millisecond
	// defaultRefreshLockStaleAfter allows taking over a lock left behind by a
	// crashed process. It must exceed the HTTP timeout so a slow refresh is
	// never mistaken for a dead owner.
	defaultRefreshLockStaleAfter = 10 * time.Minute
)

var (
	// ErrStaticTokenNotRevoked reports that the stored credential is a static
	// API token. Stored local tokens are cleared, but only the server can
	// revoke the API token itself.
	ErrStaticTokenNotRevoked = errors.New("httpclient: a static API token cannot be revoked by this command; revoke the API token on the server")

	// ErrWrongServer reports that stored credentials are bound to another
	// server than the configured one. The wrapped error names both URLs; it
	// never contains token material.
	ErrWrongServer = errors.New("httpclient: stored credentials belong to a different server")

	// errNoRefreshToken reports that no login-managed bundle with a refresh
	// token exists. The client then returns the original 401 instead of
	// masking it with a refresh error.
	errNoRefreshToken = errors.New("httpclient: no refresh token is stored; run brain login again")

	// errInvalidBundle reports a stored bundle that cannot be used. It never
	// includes the stored value.
	errInvalidBundle = errors.New("httpclient: stored session is invalid; run brain login again")
)

// sessionBundle is the single secret value written by login and refresh. The
// server URL is part of the value, so binding and tokens always change
// together.
type sessionBundle struct {
	ServerURL    string `json:"serverUrl"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

type sessionPayload struct {
	AccessToken  string      `json:"accessToken"`
	RefreshToken string      `json:"refreshToken"`
	ExpiresAt    time.Time   `json:"expiresAt"`
	User         domain.User `json:"user"`
}

func (p sessionPayload) session() ports.Session {
	return ports.Session{
		AccessToken:  p.AccessToken,
		RefreshToken: p.RefreshToken,
		ExpiresAt:    p.ExpiresAt,
		User:         p.User,
	}
}

type mePayload struct {
	User       domain.User `json:"user"`
	AuthSource string      `json:"authSource"`
	ExpiresAt  *time.Time  `json:"expiresAt,omitempty"`
}

// Auth implements ports.AuthProvider for CLI and server mode. It talks to the
// /v1/auth endpoints, persists rotated tokens in the OS secret store, and
// installs a 401 refresher on the wrapped Client.
type Auth struct {
	client  *Client
	secrets ports.SecretStore
	// serverURL is the normalized server root the stored credentials are
	// bound to. NewAuth takes it from the client so every caller, including
	// bootstrap, gets the same normalized value.
	serverURL string
	mu        sync.Mutex

	// lockPath overrides the per-user refresh lock location in tests.
	lockPath  string
	lockWait  time.Duration
	lockPoll  time.Duration
	lockStale time.Duration
}

// NewAuth wires an Auth helper to a client and a secret store. The secret store
// must outlive the process credentials; the OS backend is the default in the
// CLI.
func NewAuth(client *Client, secrets ports.SecretStore) (*Auth, error) {
	if client == nil {
		return nil, errors.New("httpclient: auth requires a client")
	}
	if secrets == nil {
		return nil, errors.New("httpclient: auth requires a secret store")
	}
	auth := &Auth{client: client, secrets: secrets, serverURL: client.BaseURL()}
	client.SetRefresher(auth.RefreshStored)
	return auth, nil
}

// Login exchanges a login and password for a token pair and persists it in one
// atomic bundle write.
func (a *Auth) Login(ctx context.Context, credentials ports.Credentials) (ports.Session, error) {
	login := strings.TrimSpace(credentials.Login)
	if login == "" {
		return ports.Session{}, domain.NewValidationError("auth.login", "is required")
	}
	if credentials.Password == "" {
		return ports.Session{}, domain.NewValidationError("auth.password", "is required")
	}
	var payload sessionPayload
	err := a.client.doAnonymous(ctx, http.MethodPost, "/v1/auth/login", nil, struct {
		Login    string `json:"login"`
		Password string `json:"password"`
	}{Login: login, Password: credentials.Password}, &payload)
	if err != nil {
		return ports.Session{}, err
	}
	access := strings.TrimSpace(payload.AccessToken)
	refresh := strings.TrimSpace(payload.RefreshToken)
	// Serialize the write with refresh rotation so a concurrent command sees
	// the complete previous bundle or the complete new one.
	release, err := a.acquireRefreshLock(ctx)
	if err != nil {
		return ports.Session{}, err
	}
	defer release()
	if err := a.persistBundle(ctx, access, refresh); err != nil {
		return ports.Session{}, err
	}
	session := payload.session()
	session.AccessToken = access
	session.RefreshToken = refresh
	return session, nil
}

// Refresh exchanges a refresh token for a new pair and persists it in one
// atomic bundle write. An empty refresh token in the response is rejected: the
// server already consumed the old token, so keeping it would trigger reuse
// detection on the next call.
func (a *Auth) Refresh(ctx context.Context, refreshToken string) (ports.Session, error) {
	token := strings.TrimSpace(refreshToken)
	if token == "" {
		return ports.Session{}, domain.NewValidationError("auth.refresh", "refresh token is required")
	}
	var payload sessionPayload
	err := a.client.doAnonymous(ctx, http.MethodPost, "/v1/auth/refresh", nil, struct {
		RefreshToken string `json:"refreshToken"`
	}{RefreshToken: token}, &payload)
	if err != nil {
		return ports.Session{}, err
	}
	access := strings.TrimSpace(payload.AccessToken)
	if access == "" {
		return ports.Session{}, domain.NewValidationError("auth.accessToken", "server returned an empty access token")
	}
	refresh := strings.TrimSpace(payload.RefreshToken)
	if refresh == "" {
		cleanupErr := a.deleteBundle(ctx)
		return ports.Session{}, errors.Join(
			errors.New("httpclient: server returned an empty refresh token; run brain login again"),
			cleanupErr,
		)
	}
	if err := a.persistBundle(ctx, access, refresh); err != nil {
		return ports.Session{}, err
	}
	session := payload.session()
	session.AccessToken = access
	session.RefreshToken = refresh
	return session, nil
}

// RefreshStored refreshes using the stored refresh token and returns the new
// access token. It is installed as the Client 401 refresher.
//
// Rotation is serialized across processes with a per-user lock file. The
// bundle is re-read after the lock is held: if another process already rotated
// it, the current access token is returned instead of rotating twice and
// revoking the user's token family.
//
// A credential 401 deletes the dead bundle; a transient 5xx or transport
// failure keeps it so the caller can retry.
func (a *Auth) RefreshStored(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	before, err := a.readBundle(ctx)
	if err != nil {
		return "", err
	}
	if before == nil {
		return "", errNoRefreshToken
	}
	if err := a.checkBundleServer(before); err != nil {
		return "", err
	}
	if before.RefreshToken == "" {
		return "", errNoRefreshToken
	}

	release, err := a.acquireRefreshLock(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	current, err := a.readBundle(ctx)
	if err != nil {
		return "", err
	}
	if current == nil {
		return "", errNoRefreshToken
	}
	if err := a.checkBundleServer(current); err != nil {
		return "", err
	}
	if current.RefreshToken != before.RefreshToken || current.AccessToken != before.AccessToken {
		// Another process rotated or replaced the bundle while this one
		// waited for the lock. Use its access token without rotating again.
		if current.AccessToken == "" {
			return "", errNoRefreshToken
		}
		return current.AccessToken, nil
	}
	session, err := a.Refresh(ctx, current.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			if clearErr := a.deleteBundle(ctx); clearErr != nil {
				return "", errors.Join(err, clearErr)
			}
		}
		return "", err
	}
	return session.AccessToken, nil
}

// Logout revokes the current server session and clears local credentials. The
// server derives the session from the bearer token, so sessionID is accepted
// only to satisfy ports.AuthProvider.
//
// The stored access token is inspected with /v1/auth/me before anything is
// rotated, so a mixed static-token plus refresh-token state is never reported
// as a successful JWT logout. Transient /me or /logout failures keep the
// bundle for a retry. A static API token cannot be revoked by this command and
// reports ErrStaticTokenNotRevoked after clearing local state.
func (a *Auth) Logout(ctx context.Context, _ string) error {
	bundle, err := a.readBundle(ctx)
	if err != nil {
		return err
	}
	if bundle != nil {
		if err := a.checkBundleServer(bundle); err != nil {
			return err
		}
	}
	static, err := a.storedToken(ctx, SecretAccessToken)
	if err != nil {
		return err
	}
	switch {
	case bundle == nil && static == "":
		// Idempotent: a second logout clears whatever legacy keys remain.
		return a.clear(ctx)
	case bundle == nil:
		return a.logoutStatic(ctx, static)
	case static == "":
		return a.logoutBundle(ctx, bundle)
	default:
		return a.logoutMixed(ctx, bundle, static)
	}
}

// logoutStatic handles a store that only has the legacy static API token.
func (a *Auth) logoutStatic(ctx context.Context, access string) error {
	me, err := a.me(ctx, access)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			return err
		}
		// Definitively rejected: clear the dead credential.
		return a.clear(ctx)
	}
	if me.AuthSource != "jwt" {
		return a.staticNotRevoked(ctx)
	}
	if err := a.logoutAccess(ctx, access); err != nil {
		return err
	}
	return a.clear(ctx)
}

// logoutBundle handles the normal login-managed bundle without a legacy static
// token.
func (a *Auth) logoutBundle(ctx context.Context, bundle *sessionBundle) error {
	access := bundle.AccessToken
	rotated := false
	me, err := a.me(ctx, access)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			return err
		}
		if bundle.RefreshToken == "" {
			// No refresh material: the access token is definitively dead.
			return a.clear(ctx)
		}
		access, err = a.RefreshStored(ctx)
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				// The refresh family is already invalid; only cleanup remains.
				return a.clear(ctx)
			}
			return err
		}
		rotated = true
		me, err = a.me(ctx, access)
		if err != nil {
			if !errors.Is(err, ErrUnauthorized) {
				return err
			}
			// A freshly rotated access token was rejected; treat the session
			// as dead instead of keeping unusable credentials.
			return a.clear(ctx)
		}
	}
	if me.AuthSource != "jwt" {
		if !rotated {
			if err := a.revokeBundleFamily(ctx, bundle); err != nil {
				return err
			}
		}
		return a.staticNotRevoked(ctx)
	}
	if err := a.logoutAccess(ctx, access); err != nil {
		return err
	}
	return a.clear(ctx)
}

// logoutMixed handles a login-managed bundle plus a legacy static token. The
// static token is inspected first: if it is live, logout must warn that this
// command cannot revoke it instead of silently rotating the refresh family and
// claiming success.
func (a *Auth) logoutMixed(ctx context.Context, bundle *sessionBundle, static string) error {
	me, err := a.me(ctx, static)
	if err != nil {
		if !errors.Is(err, ErrUnauthorized) {
			return err
		}
		// The static credential is rejected; the bundle is the remaining
		// session and still needs a normal logout.
		return a.logoutBundle(ctx, bundle)
	}
	if me.AuthSource == "jwt" {
		// A legacy release stored a JWT under server-token. Revoke it too.
		if err := a.logoutAccess(ctx, static); err != nil {
			return err
		}
	}
	if err := a.revokeBundleFamily(ctx, bundle); err != nil {
		return err
	}
	if me.AuthSource != "jwt" {
		return a.staticNotRevoked(ctx)
	}
	return a.clear(ctx)
}

// revokeBundleFamily rotates the stored bundle once and revokes the resulting
// JWT session, which also revokes the whole refresh family. A credential 401
// means the family is already gone. A transient failure returns the error and
// keeps the bundle for a retry.
func (a *Auth) revokeBundleFamily(ctx context.Context, bundle *sessionBundle) error {
	if bundle == nil || bundle.RefreshToken == "" {
		return nil
	}
	access, err := a.RefreshStored(ctx)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return nil
		}
		return err
	}
	return a.logoutAccess(ctx, access)
}

// logoutAccess revokes one JWT session. An already invalid session needs no
// further work; a transient failure keeps local credentials for a retry.
func (a *Auth) logoutAccess(ctx context.Context, access string) error {
	err := a.client.doWithToken(ctx, http.MethodPost, "/v1/auth/logout", nil, nil, nil, access)
	if err == nil || errors.Is(err, ErrUnauthorized) {
		return nil
	}
	return err
}

// staticNotRevoked clears local state and reports that the API token itself
// remains valid on the server.
func (a *Auth) staticNotRevoked(ctx context.Context) error {
	if err := a.clear(ctx); err != nil {
		return errors.Join(ErrStaticTokenNotRevoked, err)
	}
	return ErrStaticTokenNotRevoked
}

// Principal resolves the user behind an access token. An empty token reads the
// stored one.
func (a *Auth) Principal(ctx context.Context, accessToken string) (domain.User, error) {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		stored, err := a.storedAccessToken(ctx)
		if err != nil {
			return domain.User{}, err
		}
		token = stored
	}
	if token == "" {
		return domain.User{}, domain.NewValidationError("auth.accessToken", "is required")
	}
	var payload mePayload
	if err := a.client.doWithToken(ctx, http.MethodGet, "/v1/auth/me", nil, nil, &payload, token); err != nil {
		return domain.User{}, err
	}
	return payload.User, nil
}

// Token implements TokenProvider so the client always reads the current
// access token from the secret store. The login-managed bundle wins; the legacy
// server-token key remains only as an explicit static API-token fallback.
func (a *Auth) Token(ctx context.Context) (string, error) {
	return a.storedAccessToken(ctx)
}

// storedAccessToken resolves the bundle first, then the legacy static token.
func (a *Auth) storedAccessToken(ctx context.Context) (string, error) {
	bundle, err := a.readBundle(ctx)
	if err != nil {
		return "", err
	}
	if bundle != nil {
		if err := a.checkBundleServer(bundle); err != nil {
			return "", err
		}
		return bundle.AccessToken, nil
	}
	return a.storedToken(ctx, SecretAccessToken)
}

// readBundle returns nil when no bundle is stored. A corrupt bundle is an
// error, never a reason to fall back to another credential.
func (a *Auth) readBundle(ctx context.Context) (*sessionBundle, error) {
	raw, err := a.secrets.Get(ctx, SecretSession)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("httpclient: read session bundle: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var bundle sessionBundle
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil {
		return nil, errInvalidBundle
	}
	bundle.ServerURL = strings.TrimSpace(bundle.ServerURL)
	bundle.AccessToken = strings.TrimSpace(bundle.AccessToken)
	bundle.RefreshToken = strings.TrimSpace(bundle.RefreshToken)
	if bundle.AccessToken == "" {
		return nil, errInvalidBundle
	}
	return &bundle, nil
}

// checkBundleServer refuses a bundle issued by another server. Unlike the
// legacy keys, a bundle is always bound: a missing binding is invalid.
func (a *Auth) checkBundleServer(bundle *sessionBundle) error {
	if bundle == nil {
		return nil
	}
	stored := strings.TrimRight(strings.TrimSpace(bundle.ServerURL), "/")
	if stored == "" {
		return errInvalidBundle
	}
	configured := strings.TrimRight(a.serverBinding(), "/")
	if configured == "" {
		return nil
	}
	if stored != configured {
		return fmt.Errorf("%w: stored credentials are bound to %s but the configured server is %s", ErrWrongServer, stored, configured)
	}
	return nil
}

func (a *Auth) serverBinding() string {
	if binding := strings.TrimSpace(a.serverURL); binding != "" {
		return binding
	}
	if a.client == nil {
		return ""
	}
	return a.client.BaseURL()
}

func (a *Auth) storedToken(ctx context.Context, key string) (string, error) {
	value, err := a.secrets.Get(ctx, key)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("httpclient: read %s: %w", key, err)
	}
	return strings.TrimSpace(value), nil
}

func (a *Auth) me(ctx context.Context, accessToken string) (mePayload, error) {
	var payload mePayload
	if err := a.client.doWithToken(ctx, http.MethodGet, "/v1/auth/me", nil, nil, &payload, accessToken); err != nil {
		return mePayload{}, err
	}
	return payload, nil
}

func (a *Auth) refreshLockPath() string {
	if a.lockPath != "" {
		return a.lockPath
	}
	if dir, err := os.UserCacheDir(); err == nil && strings.TrimSpace(dir) != "" {
		return filepath.Join(dir, refreshLockDirName, refreshLockFileName)
	}
	return filepath.Join(perUserTempDir(), refreshLockDirName, refreshLockFileName)
}

// perUserTempDir derives a private fallback directory from the user identity so
// two OS users never share a refresh lock in the system temporary directory.
func perUserTempDir() string {
	identity := strings.TrimSpace(os.Getenv("USERNAME"))
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		identity += "|" + strings.ToLower(filepath.Clean(home))
	}
	if identity == "" {
		return os.TempDir()
	}
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%x", refreshLockDirName, sum[:8]))
}

func (a *Auth) refreshLockWait() time.Duration {
	if a.lockWait > 0 {
		return a.lockWait
	}
	return defaultRefreshLockWait
}

func (a *Auth) refreshLockPoll() time.Duration {
	if a.lockPoll > 0 {
		return a.lockPoll
	}
	return defaultRefreshLockPoll
}

func (a *Auth) refreshLockStale() time.Duration {
	if a.lockStale > 0 {
		return a.lockStale
	}
	return defaultRefreshLockStaleAfter
}

// acquireRefreshLock creates a per-user lock file shared by every brain process
// of this user. The file records an owner token, and the returned release
// function only removes the file while it still holds that token, so a
// preempted owner can never delete a new owner's lock.
func (a *Auth) acquireRefreshLock(ctx context.Context) (func(), error) {
	path := a.refreshLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("httpclient: prepare refresh lock: %w", err)
	}
	owner, err := newLockOwner()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(a.refreshLockWait())
	for {
		file, openErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if openErr == nil {
			_, writeErr := file.WriteString(owner)
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("httpclient: write refresh lock: %w", errors.Join(writeErr, closeErr))
			}
			return func() { releaseRefreshLock(path, owner) }, nil
		}
		if !errors.Is(openErr, fs.ErrExist) {
			return nil, fmt.Errorf("httpclient: acquire refresh lock: %w", openErr)
		}
		taken, takeErr := a.takeOverStaleLock(path)
		if takeErr != nil {
			return nil, takeErr
		}
		if taken {
			continue
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("httpclient: timed out waiting for the auth refresh lock %s", path)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(a.refreshLockPoll()):
		}
	}
}

// takeOverStaleLock removes an abandoned lock only when its content still
// equals the stale content that was read, so a lock taken over by another
// process in the meantime is left untouched.
func (a *Auth) takeOverStaleLock(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, nil
	}
	if time.Since(info.ModTime()) <= a.refreshLockStale() {
		return false, nil
	}
	staleOwner, err := os.ReadFile(path)
	if err != nil {
		return false, nil
	}
	currentOwner, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(currentOwner, staleOwner) {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, fmt.Errorf("httpclient: replace stale refresh lock: %w", err)
	}
	return true, nil
}

// releaseRefreshLock removes the lock only when this process still owns it.
func releaseRefreshLock(path, owner string) {
	current, err := os.ReadFile(path)
	if err != nil || string(current) != owner {
		return
	}
	_ = os.Remove(path)
}

// newLockOwner returns a process-unique token recorded in the lock file.
func newLockOwner() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("httpclient: generate refresh lock owner: %w", err)
	}
	return fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(random[:])), nil
}

// persistBundle writes binding and token pair as one secret value. If the
// write fails after the server already issued or rotated the pair, the old
// bundle is deleted (fail closed) so a consumed refresh token can never be
// reused.
func (a *Auth) persistBundle(ctx context.Context, access, refresh string) error {
	access = strings.TrimSpace(access)
	if access == "" {
		return domain.NewValidationError("auth.accessToken", "server returned an empty access token")
	}
	raw, err := json.Marshal(sessionBundle{
		ServerURL:    a.serverBinding(),
		AccessToken:  access,
		RefreshToken: strings.TrimSpace(refresh),
	})
	if err != nil {
		return fmt.Errorf("httpclient: encode session bundle: %w", err)
	}
	if err := a.secrets.Set(ctx, SecretSession, string(raw)); err != nil {
		cleanupErr := a.deleteBundle(ctx)
		return errors.Join(
			fmt.Errorf("httpclient: store session bundle: %w; run brain login again", err),
			cleanupErr,
		)
	}
	return nil
}

func (a *Auth) deleteBundle(ctx context.Context) error {
	if err := a.secrets.Delete(ctx, SecretSession); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("httpclient: clear %s: %w", SecretSession, err)
	}
	return nil
}

// clear removes the bundle and every legacy login key, best effort, joining
// all failures so the caller can report partial cleanup.
func (a *Auth) clear(ctx context.Context) error {
	var failures []error
	for _, key := range []string{SecretSession, SecretAccessToken, SecretRefreshToken, SecretAuthServerURL} {
		if err := a.secrets.Delete(ctx, key); err != nil && !errors.Is(err, domain.ErrNotFound) {
			failures = append(failures, fmt.Errorf("httpclient: clear %s: %w", key, err))
		}
	}
	return errors.Join(failures...)
}
