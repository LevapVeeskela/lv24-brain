package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type fixtureAsset struct {
	Name   string
	Kind   string
	Body   []byte
	OS     string
	Arch   string
	NoSHA  bool
	Digest string
	Status int
}

type fixtureRelease struct {
	Tag        string
	Version    string
	Channel    string
	Prerelease bool
	NoManifest bool
	Checksums  bool
	// Unsigned omits release.json.sig; BadSignature serves a signature by an
	// untrusted key.
	Unsigned     bool
	BadSignature bool
	Assets       []fixtureAsset
	Published    time.Time
}

func (r fixtureRelease) tag() string {
	if r.Tag != "" {
		return r.Tag
	}
	return "v" + r.Version
}

func (r fixtureRelease) published() time.Time {
	if !r.Published.IsZero() {
		return r.Published
	}
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}

// testReleaseKey signs fixture manifests; testUpdater trusts its public half.
var testReleaseKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))

// untrustedReleaseKey produces well-formed signatures nobody trusts.
var untrustedReleaseKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))

func newFixtureServer(t *testing.T, releases []fixtureRelease) *httptest.Server {
	t.Helper()
	var baseURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/LevapVeeskela/lv24-brain/releases", func(w http.ResponseWriter, _ *http.Request) {
		type asset struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Size               int64  `json:"size"`
			Digest             string `json:"digest,omitempty"`
		}
		type release struct {
			TagName     string    `json:"tag_name"`
			Draft       bool      `json:"draft"`
			Prerelease  bool      `json:"prerelease"`
			PublishedAt time.Time `json:"published_at"`
			Assets      []asset   `json:"assets"`
		}
		out := make([]release, 0, len(releases))
		for _, fixture := range releases {
			item := release{
				TagName:     fixture.tag(),
				Prerelease:  fixture.Prerelease,
				PublishedAt: fixture.published(),
			}
			if !fixture.NoManifest {
				item.Assets = append(item.Assets, asset{
					Name:               ManifestAssetName,
					BrowserDownloadURL: baseURL + "/releases/" + fixture.tag() + "/" + ManifestAssetName,
				})
				if !fixture.Unsigned {
					item.Assets = append(item.Assets, asset{
						Name:               SignatureAssetName,
						BrowserDownloadURL: baseURL + "/releases/" + fixture.tag() + "/" + SignatureAssetName,
					})
				}
			}
			for _, fixtureAsset := range fixture.Assets {
				item.Assets = append(item.Assets, asset{
					Name:               fixtureAsset.Name,
					BrowserDownloadURL: baseURL + "/assets/" + fixtureAsset.Name,
					Size:               int64(len(fixtureAsset.Body)),
					Digest:             fixtureAsset.Digest,
				})
			}
			out = append(out, item)
		}
		writeJSON(t, w, out)
	})
	for _, fixture := range releases {
		fixture := fixture
		if !fixture.NoManifest {
			// The manifest is rendered per request because baseURL is only
			// known once the server starts; signing the same bytes that are
			// served keeps the signature valid.
			render := func() []byte {
				raw, err := json.Marshal(manifestFor(baseURL, fixture))
				if err != nil {
					t.Errorf("marshal manifest: %v", err)
				}
				return raw
			}
			mux.HandleFunc("/releases/"+fixture.tag()+"/"+ManifestAssetName, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(render())
			})
			mux.HandleFunc("/releases/"+fixture.tag()+"/"+SignatureAssetName, func(w http.ResponseWriter, _ *http.Request) {
				key := testReleaseKey
				if fixture.BadSignature {
					key = untrustedReleaseKey
				}
				_, _ = w.Write(SignManifest(render(), key))
			})
		}
		if fixture.Checksums {
			mux.HandleFunc("/checksums/"+fixture.tag(), func(w http.ResponseWriter, _ *http.Request) {
				var builder strings.Builder
				for _, fixtureAsset := range fixture.Assets {
					builder.WriteString(sha256Hex(fixtureAsset.Body))
					builder.WriteString("  ")
					builder.WriteString(fixtureAsset.Name)
					builder.WriteString("\n")
				}
				_, _ = w.Write([]byte(builder.String()))
			})
		}
		for _, fixtureAsset := range fixture.Assets {
			fixtureAsset := fixtureAsset
			mux.HandleFunc("/assets/"+fixtureAsset.Name, func(w http.ResponseWriter, _ *http.Request) {
				if fixtureAsset.Status != 0 {
					http.Error(w, "fixture failure", fixtureAsset.Status)
					return
				}
				_, _ = w.Write(fixtureAsset.Body)
			})
		}
	}
	server := httptest.NewServer(mux)
	baseURL = server.URL
	t.Cleanup(server.Close)
	return server
}

func manifestFor(baseURL string, fixture fixtureRelease) Manifest {
	manifest := Manifest{
		Version:     fixture.Version,
		Channel:     fixture.Channel,
		PublishedAt: fixture.published().Format(time.RFC3339),
	}
	if fixture.Checksums {
		manifest.ChecksumsURL = baseURL + "/checksums/" + fixture.tag()
	}
	for _, fixtureAsset := range fixture.Assets {
		asset := ManifestAsset{
			Kind: fixtureAsset.Kind,
			OS:   fixtureAsset.OS,
			Arch: fixtureAsset.Arch,
			Name: fixtureAsset.Name,
			URL:  baseURL + "/assets/" + fixtureAsset.Name,
			Size: int64(len(fixtureAsset.Body)),
		}
		if !fixtureAsset.NoSHA {
			asset.SHA256 = sha256Hex(fixtureAsset.Body)
		}
		manifest.Assets = append(manifest.Assets, asset)
	}
	return manifest
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode fixture: %v", err)
	}
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func testUpdater(t *testing.T, serverURL string, options ...func(*Options)) *Updater {
	t.Helper()
	opts := Options{
		APIBaseURL:     serverURL,
		GOOS:           "windows",
		GOARCH:         "amd64",
		CurrentVersion: "1.0.0",
		UpdateDir:      t.TempDir(),
		Getenv:         func(string) string { return "" },
		TrustedKeys:    []ed25519.PublicKey{testReleaseKey.Public().(ed25519.PublicKey)},
	}
	for _, apply := range options {
		apply(&opts)
	}
	updater, err := New(opts)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return updater
}

func stableFixture(version string) fixtureRelease {
	return fixtureRelease{
		Version: version,
		Channel: ChannelStable,
		Assets: []fixtureAsset{{
			Name: "brain_" + version + "_windows_amd64.exe",
			Kind: "cli",
			Body: []byte("brain-" + version),
			OS:   "windows",
			Arch: "amd64",
		}},
	}
}

func betaFixture(version string) fixtureRelease {
	fixture := stableFixture(version)
	fixture.Channel = ChannelBeta
	fixture.Prerelease = true
	return fixture
}

func TestCheckStableSelectsLatestStable(t *testing.T) {
	server := newFixtureServer(t, []fixtureRelease{
		stableFixture("1.2.0"),
		stableFixture("1.3.0"),
		betaFixture("1.4.0-beta.1"),
	})
	updater := testUpdater(t, server.URL)

	release, err := updater.Check(context.Background(), ChannelStable)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if release.Version != "1.3.0" {
		t.Fatalf("Check() version = %q, want 1.3.0", release.Version)
	}
	if release.Channel != ChannelStable {
		t.Fatalf("Check() channel = %q, want stable", release.Channel)
	}
	if !strings.Contains(release.URL, "1.3.0") {
		t.Fatalf("Check() url = %q, want 1.3.0 asset", release.URL)
	}
	if release.SHA256 != sha256Hex([]byte("brain-1.3.0")) {
		t.Fatalf("Check() sha = %q", release.SHA256)
	}
}

func TestCheckBetaSelectsPrerelease(t *testing.T) {
	server := newFixtureServer(t, []fixtureRelease{
		stableFixture("1.3.0"),
		betaFixture("1.4.0-beta.1"),
	})
	updater := testUpdater(t, server.URL)

	release, err := updater.Check(context.Background(), ChannelBeta)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if release.Version != "1.4.0-beta.1" {
		t.Fatalf("Check() version = %q, want 1.4.0-beta.1", release.Version)
	}
	if release.Channel != ChannelBeta {
		t.Fatalf("Check() channel = %q, want beta", release.Channel)
	}
}

func TestCheckDisabledByEnvironment(t *testing.T) {
	server := newFixtureServer(t, []fixtureRelease{stableFixture("1.2.0")})
	updater := testUpdater(t, server.URL, func(opts *Options) {
		opts.Getenv = func(name string) string {
			if name == EnvNoUpdate {
				return "1"
			}
			return ""
		}
	})
	if _, err := updater.Check(context.Background(), ChannelStable); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Check() error = %v, want ErrDisabled", err)
	}

	explicit := testUpdater(t, server.URL, func(opts *Options) {
		opts.Getenv = func(name string) string {
			if name == EnvNoUpdate {
				return "true"
			}
			return ""
		}
		opts.IgnoreDisabled = true
	})
	if _, err := explicit.Check(context.Background(), ChannelStable); err != nil {
		t.Fatalf("explicit Check() error = %v", err)
	}
}

func TestCheckReportsMissingManifest(t *testing.T) {
	fixture := stableFixture("1.2.0")
	fixture.NoManifest = true
	server := newFixtureServer(t, []fixtureRelease{fixture})
	updater := testUpdater(t, server.URL)

	if _, err := updater.Check(context.Background(), ChannelStable); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("Check() error = %v, want ErrNoManifest", err)
	}
}

func TestCheckRejectsUnknownChannel(t *testing.T) {
	updater := testUpdater(t, "http://127.0.0.1:1")
	if _, err := updater.Check(context.Background(), "nightly"); err == nil {
		t.Fatal("Check() error = nil, want channel validation error")
	}
}

func TestCheckHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer server.Close()
	updater := testUpdater(t, server.URL)
	if _, err := updater.Check(context.Background(), ChannelStable); err == nil {
		t.Fatal("Check() error = nil, want HTTP error")
	}
}

func TestDownloadVerifiesAndStages(t *testing.T) {
	body := []byte("release-binary")
	fixture := stableFixture("1.2.0")
	fixture.Assets[0].Body = body
	server := newFixtureServer(t, []fixtureRelease{fixture})
	updater := testUpdater(t, server.URL)

	release, err := updater.Check(context.Background(), ChannelStable)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	staged, err := updater.Download(context.Background(), release)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read staged artifact: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("staged artifact = %q, want %q", got, body)
	}
	if !strings.Contains(filepath.Base(staged), "1.2.0") {
		t.Fatalf("staged name = %q, want version", filepath.Base(staged))
	}
}

func TestDownloadRejectsChecksumMismatch(t *testing.T) {
	fixture := stableFixture("1.2.0")
	server := newFixtureServer(t, []fixtureRelease{fixture})
	updater := testUpdater(t, server.URL)

	release, err := updater.Check(context.Background(), ChannelStable)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	// Point the release at a different artifact whose bytes do not match the
	// manifest checksum captured by Check.
	tampered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tampered"))
	}))
	defer tampered.Close()
	release.URL = tampered.URL + "/tampered.exe"

	_, err = updater.Download(context.Background(), release)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("Download() error = %v, want ErrChecksum", err)
	}
	entries, readErr := os.ReadDir(updater.updateDir)
	if readErr != nil {
		t.Fatalf("read update dir: %v", readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "staged-") || strings.HasPrefix(entry.Name(), ".download-") {
			t.Fatalf("temporary download artifact left behind: %s", entry.Name())
		}
	}
}

func TestDownloadRejectsMissingChecksum(t *testing.T) {
	updater := testUpdater(t, "http://127.0.0.1:1")
	_, err := updater.Download(context.Background(), portsRelease("1.2.0", "http://127.0.0.1:1/brain.exe", ""))
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("Download() error = %v, want ErrChecksum", err)
	}
}

func TestApplyAndRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "brain.exe")
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(dir, "artifact")
	if err := os.WriteFile(artifact, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := testUpdater(t, "http://127.0.0.1:1", func(opts *Options) {
		opts.GOOS = "linux"
		opts.ExecutablePath = target
	})

	if err := updater.Apply(context.Background(), artifact); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	assertFileContent(t, target, "new-binary")
	assertFileContent(t, target+".previous", "old-binary")

	if err := updater.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	assertFileContent(t, target, "old-binary")
	assertFileContent(t, target+".previous", "old-binary")
}

func TestApplyZipArchive(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "brain.exe")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(dir, "brain.zip")
	if err := os.WriteFile(artifact, zipBytes(t, "brain.exe", []byte("zip-binary")), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := testUpdater(t, "http://127.0.0.1:1", func(opts *Options) {
		opts.GOOS = "linux"
		opts.ExecutablePath = target
	})
	if err := updater.Apply(context.Background(), artifact); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	assertFileContent(t, target, "zip-binary")
}

func TestApplyTarGzArchive(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "brain.exe")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(dir, "brain.tar.gz")
	if err := os.WriteFile(artifact, tarGzBytes(t, "bin/brain.exe", []byte("tar-binary")), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := testUpdater(t, "http://127.0.0.1:1", func(opts *Options) {
		opts.GOOS = "linux"
		opts.ExecutablePath = target
	})
	if err := updater.Apply(context.Background(), artifact); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	assertFileContent(t, target, "tar-binary")
}

func TestApplyRejectsRunningBinaryArtifact(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "brain.exe")
	if err := os.WriteFile(target, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := testUpdater(t, "http://127.0.0.1:1", func(opts *Options) {
		opts.ExecutablePath = target
	})
	if err := updater.Apply(context.Background(), target); err == nil {
		t.Fatal("Apply() error = nil, want same-path rejection")
	}
}

func TestRollbackWithoutPrevious(t *testing.T) {
	dir := t.TempDir()
	updater := testUpdater(t, "http://127.0.0.1:1", func(opts *Options) {
		opts.ExecutablePath = filepath.Join(dir, "brain.exe")
	})
	if err := updater.Rollback(context.Background()); !errors.Is(err, ErrNoPrevious) {
		t.Fatalf("Rollback() error = %v, want ErrNoPrevious", err)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0},
		{"1.2.4", "1.2.3", 1},
		{"1.2", "1.2.0", 0},
		{"1.10.0", "1.9.9", 1},
		{"1.2.3-beta.1", "1.2.3", -1},
		{"1.2.3-beta.2", "1.2.3-beta.1", 1},
		{"1.2.3-alpha", "1.2.3-beta", -1},
		{"1.2.3-beta.10", "1.2.3-beta.9", 1},
		{"1.2.3+build.5", "1.2.3", 0},
	}
	for _, test := range tests {
		if got := CompareVersions(test.a, test.b); got != test.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", test.a, test.b, got, test.want)
		}
	}
}

func TestManifestValidationAndAssetSelection(t *testing.T) {
	good := Manifest{
		Version: "1.2.3",
		Channel: ChannelStable,
		Assets: []ManifestAsset{
			{Kind: "server", OS: "windows", Arch: "amd64", Name: "brain-server.exe", URL: "https://example.com/server", SHA256: sha256Hex([]byte("server"))},
			{Kind: "cli", OS: "windows", Arch: "amd64", Name: "brain.exe", URL: "https://example.com/brain", SHA256: sha256Hex([]byte("cli"))},
		},
	}
	raw, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManifest(raw)
	if err != nil {
		t.Fatalf("ParseManifest() error = %v", err)
	}
	asset, err := parsed.Asset("windows", "amd64")
	if err != nil {
		t.Fatalf("Asset() error = %v", err)
	}
	if asset.Kind != "cli" {
		t.Fatalf("Asset() kind = %q, want cli", asset.Kind)
	}
	if _, err := parsed.Asset("linux", "arm64"); !errors.Is(err, ErrNoAsset) {
		t.Fatalf("Asset() error = %v, want ErrNoAsset", err)
	}

	invalid := good
	invalid.Assets = []ManifestAsset{{OS: "windows", Arch: "amd64", URL: "https://example.com/brain", SHA256: "not-a-checksum"}}
	if err := invalid.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want invalid checksum error")
	}
}

func TestSwapScriptUsesArguments(t *testing.T) {
	for _, needle := range []string{"%~1", "%~2", "move /Y", "timeout"} {
		if !strings.Contains(swapScript, needle) {
			t.Fatalf("swapScript missing %q", needle)
		}
	}
}

func TestDefaultUpdateDir(t *testing.T) {
	env := map[string]string{"LOCALAPPDATA": `C:\Users\test\AppData\Local`}
	got := DefaultUpdateDir("windows", func(name string) string { return env[name] })
	want := filepath.Join(`C:\Users\test\AppData\Local`, "lv24-brain", "updates")
	if got != want {
		t.Fatalf("DefaultUpdateDir() = %q, want %q", got, want)
	}
	env["BRAIN_UPDATE_DIR"] = filepath.Join("custom", "updates")
	if got := DefaultUpdateDir("windows", func(name string) string { return env[name] }); got != env["BRAIN_UPDATE_DIR"] {
		t.Fatalf("DefaultUpdateDir() override = %q", got)
	}
}

func zipBytes(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func tarGzBytes(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gz)
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(raw) != want {
		t.Fatalf("%s = %q, want %q", path, raw, want)
	}
}

func portsRelease(version, url, sha string) ports.ReleaseInfo {
	return ports.ReleaseInfo{Version: version, URL: url, SHA256: sha}
}
