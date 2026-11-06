package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// SignatureAssetName is the release asset carrying the detached Ed25519
// signature of the exact release.json bytes.
const SignatureAssetName = ManifestAssetName + ".sig"

// maxSignatureBytes bounds a signature asset read.
const maxSignatureBytes = 4 << 10

// signatureContext domain-separates release-manifest signatures so the key
// cannot be abused to sign anything else that verifies as a manifest.
const signatureContext = "lv24-brain release manifest v1\n"

// trustedReleaseKeys are the base64 Ed25519 public keys whose signatures the
// updater accepts. Several keys may be listed to rotate without breaking
// installed binaries. Generate a pair with
// `go run ./internal/tools/releasesign keygen` and paste the public key here;
// the private key lives only in the BRAIN_RELEASE_SIGNING_KEY Actions secret.
var trustedReleaseKeys = []string{
	// 2026-11-06 primary release key.
	"8ZkT8c37JX6zeXnMEMHEnkcG9K8pGedO19gCAowW3iY=",
}

var (
	// ErrUnsigned means a release has no release.json.sig asset.
	ErrUnsigned = errors.New("update: release manifest is not signed (" + SignatureAssetName + " missing)")
	// ErrSignature means the manifest signature does not verify against any
	// trusted key.
	ErrSignature = errors.New("update: release manifest signature is invalid")
	// ErrNoTrustedKeys means this build embeds no release public key, so it
	// cannot verify any release and refuses to update.
	ErrNoTrustedKeys = errors.New("update: this build has no trusted release keys; reinstall from a signed release")
)

// ParsePublicKey decodes a base64 Ed25519 public key.
func ParsePublicKey(value string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("update: release public key must be base64 of 32 bytes")
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateKey decodes a base64 Ed25519 seed (32 bytes) into a private key.
func ParsePrivateKey(value string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("update: release signing key must be base64 of a 32-byte seed")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// DefaultTrustedKeys returns the release keys embedded in this build.
func DefaultTrustedKeys() ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(trustedReleaseKeys))
	for i, value := range trustedReleaseKeys {
		key, err := ParsePublicKey(value)
		if err != nil {
			return nil, fmt.Errorf("update: embedded release key %d: %w", i, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// SignManifest returns the signature-asset contents for manifest.
func SignManifest(manifest []byte, key ed25519.PrivateKey) []byte {
	sig := ed25519.Sign(key, signedMessage(manifest))
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// VerifyManifestSignature checks a signature asset against the exact manifest
// bytes and accepts it if any trusted key verifies.
func VerifyManifestSignature(manifest, signature []byte, keys []ed25519.PublicKey) error {
	if len(keys) == 0 {
		return ErrNoTrustedKeys
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signature)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return ErrSignature
	}
	message := signedMessage(manifest)
	for _, key := range keys {
		if len(key) == ed25519.PublicKeySize && ed25519.Verify(key, message, sig) {
			return nil
		}
	}
	return ErrSignature
}

func signedMessage(manifest []byte) []byte {
	out := make([]byte, 0, len(signatureContext)+len(manifest))
	out = append(out, signatureContext...)
	return append(out, manifest...)
}
