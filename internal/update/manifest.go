package update

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Manifest is the signed/checksummed release descriptor published as
// release.json next to the binaries. It is the updater's source of truth for
// version, channel, platform assets and SHA-256 checksums.
type Manifest struct {
	Version          string          `json:"version"`
	Channel          string          `json:"channel"`
	MinEngineVersion string          `json:"min_engine_version,omitempty"`
	MinSchemaVersion int             `json:"min_schema_version,omitempty"`
	PublishedAt      string          `json:"published_at,omitempty"`
	Assets           []ManifestAsset `json:"assets"`
	ChecksumsURL     string          `json:"checksums_url,omitempty"`
	Signature        *Signature      `json:"signature,omitempty"`
}

// ManifestAsset describes one downloadable release artifact.
type ManifestAsset struct {
	Kind   string `json:"kind,omitempty"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// Signature is reserved inline signature metadata. The updater ignores it and
// instead requires the detached release.json.sig asset (see signature.go),
// because an inline signature cannot cover the bytes that contain it.
type Signature struct {
	Type  string `json:"type,omitempty"`
	URL   string `json:"url,omitempty"`
	Value string `json:"value,omitempty"`
}

// ParseManifest decodes and validates a release.json payload.
func ParseManifest(raw []byte) (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("update: parse release manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Validate checks the manifest contract used by the updater.
func (m Manifest) Validate() error {
	if strings.TrimSpace(m.Version) == "" {
		return errors.New("update: manifest version is required")
	}
	if m.Channel != "" {
		switch strings.ToLower(strings.TrimSpace(m.Channel)) {
		case ChannelStable, ChannelBeta:
		default:
			return fmt.Errorf("update: manifest channel must be %q or %q", ChannelStable, ChannelBeta)
		}
	}
	if len(m.Assets) == 0 {
		return errors.New("update: manifest has no assets")
	}
	for i, asset := range m.Assets {
		if strings.TrimSpace(asset.OS) == "" || strings.TrimSpace(asset.Arch) == "" {
			return fmt.Errorf("update: manifest asset %d is missing os or arch", i)
		}
		if strings.TrimSpace(asset.URL) == "" {
			return fmt.Errorf("update: manifest asset %d is missing url", i)
		}
		if _, err := url.Parse(asset.URL); err != nil {
			return fmt.Errorf("update: manifest asset %d url: %w", i, err)
		}
		if asset.SHA256 != "" && !validSHA256(normalizeSHA256(asset.SHA256)) {
			return fmt.Errorf("update: manifest asset %d has an invalid sha256", i)
		}
	}
	if m.ChecksumsURL != "" {
		if _, err := url.Parse(m.ChecksumsURL); err != nil {
			return fmt.Errorf("update: manifest checksums_url: %w", err)
		}
	}
	return nil
}

// Asset selects the CLI artifact for goos/goarch. Server artifacts never
// replace the CLI binary.
func (m Manifest) Asset(goos, goarch string) (ManifestAsset, error) {
	goos = strings.ToLower(strings.TrimSpace(goos))
	goarch = strings.ToLower(strings.TrimSpace(goarch))
	var fallback *ManifestAsset
	for i := range m.Assets {
		asset := m.Assets[i]
		if !strings.EqualFold(strings.TrimSpace(asset.OS), goos) ||
			!strings.EqualFold(strings.TrimSpace(asset.Arch), goarch) {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(asset.Kind))
		if kind == "server" || kind == "brain-server" {
			continue
		}
		if kind == "" || kind == "cli" || kind == "brain" {
			return asset, nil
		}
		if fallback == nil {
			copy := asset
			fallback = &copy
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return ManifestAsset{}, fmt.Errorf("%w: %s/%s", ErrNoAsset, goos, goarch)
}

func parseManifestTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func normalizeSHA256(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.IndexByte(value, ':'); idx >= 0 && strings.EqualFold(value[:idx], "sha256") {
		value = value[idx+1:]
	}
	return strings.ToLower(value)
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
