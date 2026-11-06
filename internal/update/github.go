package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	PublishedAt time.Time     `json:"published_at"`
	Assets      []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest,omitempty"`
}

func (u *Updater) listReleases(ctx context.Context) ([]githubRelease, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/releases?per_page=30", u.apiBaseURL, u.repo)
	var releases []githubRelease
	if err := u.getJSON(ctx, endpoint, &releases, MaxManifestBytes*8); err != nil {
		return nil, err
	}
	return releases, nil
}

// fetchSignedManifest downloads release.json and its detached signature from
// the same release and parses the manifest only after the signature verifies
// over the exact downloaded bytes.
func (u *Updater) fetchSignedManifest(ctx context.Context, rawURL string, assets []githubAsset) (Manifest, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return Manifest{}, ErrNoManifest
	}
	if err := validateArtifactURL(rawURL); err != nil {
		return Manifest{}, fmt.Errorf("update: manifest url: %w", err)
	}
	sigAsset, ok := assetNamed(assets, SignatureAssetName)
	if !ok || strings.TrimSpace(sigAsset.BrowserDownloadURL) == "" {
		return Manifest{}, ErrUnsigned
	}
	if err := validateArtifactURL(sigAsset.BrowserDownloadURL); err != nil {
		return Manifest{}, fmt.Errorf("update: signature url: %w", err)
	}
	payload, err := u.getBytes(ctx, rawURL, MaxManifestBytes)
	if err != nil {
		return Manifest{}, err
	}
	signature, err := u.getBytes(ctx, sigAsset.BrowserDownloadURL, maxSignatureBytes)
	if err != nil {
		return Manifest{}, err
	}
	if err := VerifyManifestSignature(payload, signature, u.keys); err != nil {
		return Manifest{}, err
	}
	return ParseManifest(payload)
}

func (u *Updater) getJSON(ctx context.Context, rawURL string, dst any, limit int64) error {
	raw, err := u.getBytes(ctx, rawURL, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("update: decode %s: %w", rawURL, err)
	}
	return nil
}

func (u *Updater) getBytes(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("update: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json, application/json")
	req.Header.Set("User-Agent", "lv24-brain-updater")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: request %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: request %s: unexpected status %s", rawURL, resp.Status)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("update: response from %s exceeds %d bytes", rawURL, limit)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("update: read %s: %w", rawURL, err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("update: response from %s exceeds %d bytes", rawURL, limit)
	}
	return raw, nil
}
