// Package update implements the LV24 Brain engine self-updater: GitHub
// Releases checks, release manifest parsing, SHA-256 verification, staged
// downloads, atomic binary replacement, and rollback to the previous binary.
//
// The updater is deliberately conservative: it never mutates brain data, it
// accepts only HTTPS release URLs (plain HTTP is allowed for loopback test
// servers), the release manifest must carry a valid Ed25519 signature from a
// key embedded in this build, and every downloaded artifact is verified
// against the signed manifest checksum before it can be applied.
package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

const (
	// DefaultRepo is the public product repository that publishes releases.
	DefaultRepo = "LevapVeeskela/lv24-brain"
	// DefaultAPIBaseURL is the GitHub REST API root.
	DefaultAPIBaseURL = "https://api.github.com"
	// EnvNoUpdate disables automatic update checks when set to a truthy value.
	EnvNoUpdate = "BRAIN_NO_UPDATE"
	// EnvUpdateDir overrides the staged-download directory.
	EnvUpdateDir = "BRAIN_UPDATE_DIR"
	// ManifestAssetName is the release asset that describes the release.
	ManifestAssetName = "release.json"
	// MaxManifestBytes bounds a release manifest read.
	MaxManifestBytes = 1 << 20
	// MaxDownloadBytes bounds one downloaded release artifact.
	MaxDownloadBytes = 256 << 20

	defaultHTTPTimeout = 60 * time.Second
)

// Stable and beta release channels.
const (
	ChannelStable = "stable"
	ChannelBeta   = "beta"
)

var (
	// ErrDisabled means automatic updates are disabled by BRAIN_NO_UPDATE.
	ErrDisabled = errors.New("update: automatic updates are disabled by " + EnvNoUpdate)
	// ErrNoManifest means the selected GitHub release has no release.json.
	ErrNoManifest = errors.New("update: release manifest " + ManifestAssetName + " not found")
	// ErrNoAsset means the manifest does not carry an artifact for this platform.
	ErrNoAsset = errors.New("update: no release asset for this platform")
	// ErrChecksum means an artifact failed SHA-256 verification.
	ErrChecksum = errors.New("update: sha256 checksum mismatch")
	// ErrNoPrevious means rollback has no previous binary recorded.
	ErrNoPrevious = errors.New("update: no previous binary to roll back to")
)

// Options configures an Updater. Zero values are replaced with production
// defaults; tests override the API base URL and the HTTP client.
type Options struct {
	Repo           string
	APIBaseURL     string
	GOOS           string
	GOARCH         string
	CurrentVersion string
	ExecutablePath string
	UpdateDir      string
	HTTPClient     *http.Client
	Now            func() time.Time
	Getenv         func(string) string
	// IgnoreDisabled lets an explicit `brain update` run even when
	// BRAIN_NO_UPDATE is set. Background checks leave it false.
	IgnoreDisabled bool
	// TrustedKeys overrides the release public keys embedded in the build
	// (tests only). Nil uses DefaultTrustedKeys.
	TrustedKeys []ed25519.PublicKey
	// Token authenticates GitHub API requests (never asset downloads) to lift
	// the shared per-IP rate limit. Empty falls back to BRAIN_GITHUB_TOKEN,
	// GITHUB_TOKEN, GH_TOKEN, then TokenSource, resolved on the first request.
	Token       string
	TokenSource func(context.Context) (string, error)
}

// Updater checks, verifies, stages and replaces the engine binary. It
// implements ports.Updater.
type Updater struct {
	repo       string
	apiBaseURL string
	goos       string
	goarch     string
	current    string
	execPath   string
	updateDir  string
	client     *http.Client
	now        func() time.Time
	getenv     func(string) string
	ignoreOff  bool
	keys       []ed25519.PublicKey
	token      *apiToken
}

var _ ports.Updater = (*Updater)(nil)

// New builds an Updater from opts.
func New(opts Options) (*Updater, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	repo := strings.TrimSpace(opts.Repo)
	if repo == "" {
		repo = DefaultRepo
	}
	if err := validateRepo(repo); err != nil {
		return nil, err
	}
	apiBase := strings.TrimRight(strings.TrimSpace(opts.APIBaseURL), "/")
	if apiBase == "" {
		apiBase = DefaultAPIBaseURL
	}
	if err := validateBaseURL(apiBase); err != nil {
		return nil, err
	}
	goos := strings.ToLower(strings.TrimSpace(opts.GOOS))
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := strings.ToLower(strings.TrimSpace(opts.GOARCH))
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	current := strings.TrimSpace(opts.CurrentVersion)
	if current == "" {
		current = version.Version
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	updateDir := strings.TrimSpace(opts.UpdateDir)
	if updateDir == "" {
		updateDir = DefaultUpdateDir(goos, getenv)
	}
	keys := opts.TrustedKeys
	if keys == nil {
		embedded, err := DefaultTrustedKeys()
		if err != nil {
			return nil, err
		}
		keys = embedded
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout:       defaultHTTPTimeout,
			CheckRedirect: secureRedirect,
		}
	}
	return &Updater{
		repo:       repo,
		apiBaseURL: apiBase,
		goos:       goos,
		goarch:     goarch,
		current:    current,
		execPath:   strings.TrimSpace(opts.ExecutablePath),
		updateDir:  updateDir,
		client:     client,
		now:        now,
		getenv:     getenv,
		ignoreOff:  opts.IgnoreDisabled,
		keys:       keys,
		token:      &apiToken{explicit: opts.Token, getenv: getenv, source: opts.TokenSource},
	}, nil
}

// CurrentVersion reports the engine version the updater was configured with.
func (u *Updater) CurrentVersion() string { return u.current }

// Check returns the newest release on channel. Stable ignores prereleases;
// beta accepts both stable and prerelease builds.
func (u *Updater) Check(ctx context.Context, channel string) (ports.ReleaseInfo, error) {
	channel, err := normalizeChannel(channel)
	if err != nil {
		return ports.ReleaseInfo{}, err
	}
	if !u.ignoreOff && envTruthy(u.getenv(EnvNoUpdate)) {
		return ports.ReleaseInfo{}, ErrDisabled
	}
	if len(u.keys) == 0 {
		return ports.ReleaseInfo{}, ErrNoTrustedKeys
	}

	releases, err := u.listReleases(ctx)
	if err != nil {
		return ports.ReleaseInfo{}, err
	}
	candidates := make([]githubRelease, 0, len(releases))
	for _, release := range releases {
		if release.Draft || strings.TrimSpace(release.TagName) == "" {
			continue
		}
		if channel == ChannelStable && release.Prerelease {
			continue
		}
		if !hasAssetNamed(release.Assets, ManifestAssetName) {
			continue
		}
		candidates = append(candidates, release)
	}
	sortReleases(candidates)
	if len(candidates) == 0 {
		return ports.ReleaseInfo{}, ErrNoManifest
	}

	var lastErr error
	for _, release := range candidates {
		asset, ok := assetNamed(release.Assets, ManifestAssetName)
		if !ok {
			lastErr = ErrNoManifest
			continue
		}
		releaseChannel := ChannelStable
		if release.Prerelease {
			releaseChannel = ChannelBeta
		}
		if channel == ChannelStable && releaseChannel != ChannelStable {
			continue
		}

		manifest, err := u.fetchSignedManifest(ctx, asset.BrowserDownloadURL, release.Assets)
		if err != nil {
			lastErr = err
			continue
		}
		if err := manifest.Validate(); err != nil {
			lastErr = err
			continue
		}
		if normalizeVersion(manifest.Version) != normalizeVersion(release.TagName) {
			lastErr = fmt.Errorf(
				"update: release tag %q does not match manifest version %q",
				release.TagName, manifest.Version,
			)
			continue
		}
		if manifest.Channel != "" && !strings.EqualFold(manifest.Channel, releaseChannel) {
			lastErr = fmt.Errorf(
				"update: manifest channel %q does not match release channel %q",
				manifest.Channel, releaseChannel,
			)
			continue
		}
		artifact, err := manifest.Asset(u.goos, u.goarch)
		if err != nil {
			return ports.ReleaseInfo{}, err
		}
		// Only the signed manifest is trusted for checksums; GitHub asset
		// digests and checksums.txt come from the same release unsigned.
		artifact.SHA256 = normalizeSHA256(artifact.SHA256)
		if !validSHA256(artifact.SHA256) {
			lastErr = fmt.Errorf("%w: release %s has no valid checksum for %s/%s",
				ErrChecksum, manifest.Version, u.goos, u.goarch)
			continue
		}
		published := release.PublishedAt
		if parsed, ok := parseManifestTime(manifest.PublishedAt); ok {
			published = parsed
		}
		return ports.ReleaseInfo{
			Version:     normalizeVersion(manifest.Version),
			Channel:     releaseChannel,
			URL:         artifact.URL,
			SHA256:      artifact.SHA256,
			PublishedAt: published,
		}, nil
	}
	if lastErr == nil {
		lastErr = ErrNoManifest
	}
	return ports.ReleaseInfo{}, lastErr
}

// Download verifies and stages one release artifact. The returned path is the
// verified staged artifact; Apply consumes it without deleting it.
func (u *Updater) Download(ctx context.Context, release ports.ReleaseInfo) (string, error) {
	artifactURL := strings.TrimSpace(release.URL)
	if artifactURL == "" {
		return "", errors.New("update: release url is required")
	}
	if err := validateArtifactURL(artifactURL); err != nil {
		return "", err
	}
	want := normalizeSHA256(release.SHA256)
	if !validSHA256(want) {
		return "", fmt.Errorf("%w: release %q has no valid sha256", ErrChecksum, release.Version)
	}
	if err := os.MkdirAll(u.updateDir, 0o755); err != nil {
		return "", fmt.Errorf("update: create update directory %s: %w", u.updateDir, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artifactURL, nil)
	if err != nil {
		return "", fmt.Errorf("update: build download request: %w", err)
	}
	req.Header.Set("User-Agent", "lv24-brain-updater")
	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("update: download release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("update: download release: unexpected status %s", resp.Status)
	}
	if resp.ContentLength > MaxDownloadBytes {
		return "", fmt.Errorf("update: release artifact exceeds %d bytes", MaxDownloadBytes)
	}

	tmp, err := os.CreateTemp(u.updateDir, ".download-*")
	if err != nil {
		return "", fmt.Errorf("update: create staged download: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	hasher := sha256Writer()
	written, err := copyLimited(ioMultiWriter(tmp, hasher), resp.Body, MaxDownloadBytes)
	if err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("update: stage release artifact: %w", err)
	}
	if written == 0 {
		_ = tmp.Close()
		return "", errors.New("update: release artifact is empty")
	}
	got := hexSum(hasher)
	if !equalSHA256(got, want) {
		_ = tmp.Close()
		return "", fmt.Errorf("%w: got %s, want %s", ErrChecksum, got, want)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("update: sync staged artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("update: close staged artifact: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil && runtime.GOOS != "windows" {
		return "", fmt.Errorf("update: chmod staged artifact: %w", err)
	}
	final := filepath.Join(u.updateDir, stagedFileName(release))
	if err := os.Remove(final); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("update: replace staged artifact: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", fmt.Errorf("update: finalize staged artifact: %w", err)
	}
	committed = true
	return final, nil
}

// DefaultUpdateDir returns the per-user staged-download directory.
func DefaultUpdateDir(goos string, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if custom := strings.TrimSpace(getenv(EnvUpdateDir)); custom != "" {
		return custom
	}
	if goos == "windows" {
		if base := strings.TrimSpace(getenv("LOCALAPPDATA")); base != "" {
			return filepath.Join(base, "lv24-brain", "updates")
		}
	}
	if goos != "windows" {
		if base := strings.TrimSpace(getenv("XDG_CACHE_HOME")); base != "" {
			return filepath.Join(base, "lv24-brain", "updates")
		}
	}
	if base, err := os.UserCacheDir(); err == nil && base != "" {
		return filepath.Join(base, "lv24-brain", "updates")
	}
	return filepath.Join(os.TempDir(), "lv24-brain", "updates")
}

func normalizeChannel(channel string) (string, error) {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return ChannelStable, nil
	}
	switch channel {
	case ChannelStable, ChannelBeta:
		return channel, nil
	default:
		return "", fmt.Errorf("update: channel must be %q or %q", ChannelStable, ChannelBeta)
	}
}

func envTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func validateRepo(repo string) error {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("update: repository %q must be owner/name", repo)
	}
	for _, part := range parts {
		for _, r := range part {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
				continue
			}
			return fmt.Errorf("update: repository %q contains unsupported characters", repo)
		}
	}
	return nil
}

func validateBaseURL(raw string) error {
	if strings.HasPrefix(raw, "https://") {
		return nil
	}
	if strings.HasPrefix(raw, "http://127.0.0.1") ||
		strings.HasPrefix(raw, "http://localhost") ||
		strings.HasPrefix(raw, "http://[::1]") {
		return nil
	}
	return fmt.Errorf("update: api base url must use https: %q", raw)
}

func secureRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("update: too many redirects")
	}
	return validateArtifactURL(req.URL.String())
}

func validateArtifactURL(raw string) error {
	if strings.HasPrefix(raw, "https://") {
		return nil
	}
	if strings.HasPrefix(raw, "http://127.0.0.1") ||
		strings.HasPrefix(raw, "http://localhost") ||
		strings.HasPrefix(raw, "http://[::1]") {
		return nil
	}
	return fmt.Errorf("update: release artifact url must use https: %q", raw)
}

func stagedFileName(release ports.ReleaseInfo) string {
	name := artifactNameFromURL(release.URL)
	if name == "" {
		name = "brain"
	}
	return "staged-" + sanitizeName(release.Version) + "-" + sanitizeName(name)
}

func artifactNameFromURL(raw string) string {
	trimmed := strings.TrimRight(raw, "/")
	if idx := strings.LastIndexAny(trimmed, "/\\"); idx >= 0 {
		trimmed = trimmed[idx+1:]
	}
	if idx := strings.IndexAny(trimmed, "?#"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	return trimmed
}

func sanitizeName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._")
	if out == "" {
		return "unknown"
	}
	return out
}

func hasAssetNamed(assets []githubAsset, name string) bool {
	_, ok := assetNamed(assets, name)
	return ok
}

func assetNamed(assets []githubAsset, name string) (githubAsset, bool) {
	for _, asset := range assets {
		if strings.EqualFold(strings.TrimSpace(asset.Name), name) {
			return asset, true
		}
	}
	return githubAsset{}, false
}

func sortReleases(releases []githubRelease) {
	// Newest semantic version first; newer publication breaks ties.
	sort.SliceStable(releases, func(i, j int) bool {
		cmp := CompareVersions(releases[i].TagName, releases[j].TagName)
		if cmp != 0 {
			return cmp > 0
		}
		return releases[i].PublishedAt.After(releases[j].PublishedAt)
	})
}
