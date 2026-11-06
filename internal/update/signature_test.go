package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"
)

func TestCheckRejectsUnsignedRelease(t *testing.T) {
	fixture := stableFixture("1.2.0")
	fixture.Unsigned = true
	server := newFixtureServer(t, []fixtureRelease{fixture})
	if _, err := testUpdater(t, server.URL).Check(context.Background(), ChannelStable); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("Check() error = %v, want ErrUnsigned", err)
	}
}

func TestCheckRejectsUntrustedSignature(t *testing.T) {
	fixture := stableFixture("1.2.0")
	fixture.BadSignature = true
	server := newFixtureServer(t, []fixtureRelease{fixture})
	if _, err := testUpdater(t, server.URL).Check(context.Background(), ChannelStable); !errors.Is(err, ErrSignature) {
		t.Fatalf("Check() error = %v, want ErrSignature", err)
	}
}

func TestCheckFallsBackPastBadReleaseToSignedOne(t *testing.T) {
	bad := stableFixture("1.3.0")
	bad.BadSignature = true
	good := stableFixture("1.2.0")
	server := newFixtureServer(t, []fixtureRelease{bad, good})
	release, err := testUpdater(t, server.URL).Check(context.Background(), ChannelStable)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if release.Version != "1.2.0" {
		t.Fatalf("Check() version = %q, want the signed 1.2.0", release.Version)
	}
}

func TestCheckRequiresChecksumInSignedManifest(t *testing.T) {
	// GitHub digests and checksums.txt are unsigned; a signed manifest
	// without a checksum must not fall back to them.
	fixture := stableFixture("1.2.0")
	fixture.Checksums = true
	fixture.Assets[0].NoSHA = true
	fixture.Assets[0].Digest = "sha256:" + sha256Hex(fixture.Assets[0].Body)
	server := newFixtureServer(t, []fixtureRelease{fixture})
	if _, err := testUpdater(t, server.URL).Check(context.Background(), ChannelStable); !errors.Is(err, ErrChecksum) {
		t.Fatalf("Check() error = %v, want ErrChecksum", err)
	}
}

func TestCheckWithoutTrustedKeysRefuses(t *testing.T) {
	server := newFixtureServer(t, []fixtureRelease{stableFixture("1.2.0")})
	updater := testUpdater(t, server.URL, func(opts *Options) {
		opts.TrustedKeys = []ed25519.PublicKey{}
	})
	if _, err := updater.Check(context.Background(), ChannelStable); !errors.Is(err, ErrNoTrustedKeys) {
		t.Fatalf("Check() error = %v, want ErrNoTrustedKeys", err)
	}
}

func TestVerifyManifestSignature(t *testing.T) {
	manifest := []byte(`{"version":"1.2.0"}`)
	sig := SignManifest(manifest, testReleaseKey)
	trusted := []ed25519.PublicKey{untrustedReleaseKey.Public().(ed25519.PublicKey), testReleaseKey.Public().(ed25519.PublicKey)}

	if err := VerifyManifestSignature(manifest, sig, trusted); err != nil {
		t.Fatalf("valid signature with rotated key list: %v", err)
	}
	if err := VerifyManifestSignature(append(manifest, ' '), sig, trusted); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered manifest error = %v, want ErrSignature", err)
	}
	if err := VerifyManifestSignature(manifest, []byte("not base64!"), trusted); !errors.Is(err, ErrSignature) {
		t.Fatalf("garbage signature error = %v, want ErrSignature", err)
	}
	// A plain Ed25519 signature without the domain-separation prefix must
	// not verify.
	raw := ed25519.Sign(testReleaseKey, manifest)
	if err := VerifyManifestSignature(manifest, []byte(base64.StdEncoding.EncodeToString(raw)), trusted); !errors.Is(err, ErrSignature) {
		t.Fatalf("undomained signature error = %v, want ErrSignature", err)
	}
}

func TestParseKeys(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	private, err := ParsePrivateKey(base64.StdEncoding.EncodeToString(seed))
	if err != nil || !private.Equal(testReleaseKey) {
		t.Fatalf("ParsePrivateKey = %v", err)
	}
	public, err := ParsePublicKey(base64.StdEncoding.EncodeToString(testReleaseKey.Public().(ed25519.PublicKey)))
	if err != nil || !public.Equal(testReleaseKey.Public()) {
		t.Fatalf("ParsePublicKey = %v", err)
	}
	if _, err := ParsePublicKey("c2hvcnQ="); err == nil {
		t.Fatal("short public key accepted")
	}
	if _, err := ParsePrivateKey("c2hvcnQ="); err == nil {
		t.Fatal("short seed accepted")
	}
	if _, err := DefaultTrustedKeys(); err != nil {
		t.Fatalf("embedded release keys do not parse: %v", err)
	}
}
