package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestKeygenSignVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "release.key")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"keygen", "-out", keyFile}, &stdout, &stderr, os.Getenv); code != 0 {
		t.Fatalf("keygen exit %d: %s", code, stderr.String())
	}
	seed, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if strings.Contains(stdout.String(), strings.TrimSpace(string(seed))) {
		t.Fatal("keygen printed the private seed")
	}
	public := regexp.MustCompile(`(?m)^[A-Za-z0-9+/]{43}=$`).FindString(stdout.String())
	if public == "" {
		t.Fatalf("keygen output has no public key: %s", stdout.String())
	}
	if code := run([]string{"keygen", "-out", keyFile}, &stdout, &stderr, os.Getenv); code == 0 {
		t.Fatal("keygen overwrote an existing key file")
	}

	manifest := filepath.Join(dir, "release.json")
	if err := os.WriteFile(manifest, []byte(`{"version":"1.0.0"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sigFile := manifest + ".sig"
	getenv := func(name string) string {
		if name == envSigningKey {
			return string(seed)
		}
		return ""
	}

	// The fresh key is not embedded in this build, so a normal sign refuses.
	stderr.Reset()
	if code := run([]string{"sign", "-in", manifest, "-out", sigFile}, &stdout, &stderr, getenv); code == 0 {
		t.Fatal("sign accepted a key that is not embedded")
	}
	if _, err := os.Stat(sigFile); err == nil {
		t.Fatal("refused sign still wrote a signature")
	}

	if code := run([]string{"sign", "-allow-unembedded", "-in", manifest, "-out", sigFile}, &stdout, &stderr, getenv); code != 0 {
		t.Fatalf("sign exit %d: %s", code, stderr.String())
	}
	if code := run([]string{"verify", "-in", manifest, "-sig", sigFile, "-pub", public}, &stdout, &stderr, os.Getenv); code != 0 {
		t.Fatalf("verify exit %d: %s", code, stderr.String())
	}
	if err := os.WriteFile(manifest, []byte(`{"version":"6.6.6"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"verify", "-in", manifest, "-sig", sigFile, "-pub", public}, &stdout, &stderr, os.Getenv); code == 0 {
		t.Fatal("verify accepted a tampered manifest")
	}
}

func TestSignRequiresKey(t *testing.T) {
	var stdout, stderr bytes.Buffer
	empty := func(string) string { return "" }
	if code := run([]string{"sign", "-in", "x", "-out", "y"}, &stdout, &stderr, empty); code != 1 || !strings.Contains(stderr.String(), envSigningKey) {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}
