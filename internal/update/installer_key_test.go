package update

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ed25519SPKIPrefix is the DER header of an Ed25519 SubjectPublicKeyInfo.
var ed25519SPKIPrefix = []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}

// The Unix installer verifies release.json with an embedded copy of the
// release key and signature context; they must match the updater's.
func TestUnixInstallerTrustsTheReleaseKey(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "installers", "unix", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	match := regexp.MustCompile(`(?s)BEGIN PUBLIC KEY-----\s*(\S+)\s*-----END PUBLIC KEY`).FindStringSubmatch(script)
	if match == nil {
		t.Fatal("installer has no release public key")
	}
	der, err := base64.StdEncoding.DecodeString(match[1])
	if err != nil || !bytes.HasPrefix(der, ed25519SPKIPrefix) {
		t.Fatalf("installer key is not an Ed25519 SPKI: %v", err)
	}
	installerKey := base64.StdEncoding.EncodeToString(der[len(ed25519SPKIPrefix):])
	trusted := false
	for _, key := range trustedReleaseKeys {
		trusted = trusted || key == installerKey
	}
	if !trusted {
		t.Fatalf("installer key %s is not in trustedReleaseKeys", installerKey)
	}
	context := strings.TrimSuffix(signatureContext, "\n")
	if !strings.Contains(script, "SIGNATURE_CONTEXT='"+context+"'") {
		t.Fatalf("installer signature context differs from %q", context)
	}
}
