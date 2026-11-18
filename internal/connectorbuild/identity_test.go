package connectorbuild

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestProtectAndResolveAccount(t *testing.T) {
	ctx := context.Background()
	secrets := credentialMemory{}
	store := SecretCredentials{Store: secrets}
	raw := "Work.Mail@Example.test"

	marker, err := ProtectAccount(ctx, "mail", raw, store)
	if err != nil {
		t.Fatal(err)
	}
	if !IsAccountMarker(marker) {
		t.Fatalf("not a marker: %q", marker)
	}
	if strings.Contains(marker, "Work.Mail") || strings.Contains(strings.ToLower(marker), "example.test") {
		t.Fatalf("marker leaked the account: %q", marker)
	}
	if got := secrets["conn/mail/account"]; got != raw {
		t.Fatalf("stored %q", got)
	}
	if !AccountMarkerMatches(marker, " work.mail@example.TEST ") {
		t.Fatal("case-insensitive account did not match the marker")
	}
	if AccountMarkerMatches(marker, "other@example.test") {
		t.Fatal("different account matched the marker")
	}

	resolved, err := ResolveAccount(ctx, domain.Connection{ID: "mail", Account: marker}, store)
	if err != nil || resolved.Account != raw {
		t.Fatalf("resolve = %q %v", resolved.Account, err)
	}

	// Dual-read fallback: a legacy raw account is used as-is and never rewritten.
	legacy, err := ResolveAccount(ctx, domain.Connection{ID: "mail", Account: "legacy@example.test"}, nil)
	if err != nil || legacy.Account != "legacy@example.test" {
		t.Fatalf("legacy = %q %v", legacy.Account, err)
	}

	// Re-protecting a marker is a no-op, so retries are safe.
	again, err := ProtectAccount(ctx, "mail", marker, store)
	if err != nil || again != marker {
		t.Fatalf("idempotent protect = %q %v", again, err)
	}
	if empty, err := ProtectAccount(ctx, "mail", "", store); err != nil || empty != "" {
		t.Fatalf("empty protect = %q %v", empty, err)
	}
}

func TestResolveAccountMissingSecretIsClear(t *testing.T) {
	ctx := context.Background()
	secrets := credentialMemory{}
	store := SecretCredentials{Store: secrets}
	marker, err := ProtectAccount(ctx, "mail", "reader@example.test", store)
	if err != nil {
		t.Fatal(err)
	}
	delete(secrets, "conn/mail/account")

	_, err = ResolveAccount(ctx, domain.Connection{ID: "mail", Account: marker}, store)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing secret = %v", err)
	}
	for _, want := range []string{"conn/mail/account", "secret set mail account"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "reader@example.test") {
		t.Fatalf("error leaked the account: %v", err)
	}
	if issues := IdentityIssues(ctx, []domain.Connection{{ID: "mail", Account: marker}}, store); len(issues) != 1 || issues[0].Problem != IdentityProblemMissing {
		t.Fatalf("issues = %+v", issues)
	}
	if !strings.Contains(DescribeAccount(marker), "keychain") {
		t.Fatalf("display = %q", DescribeAccount(marker))
	}
}

func TestResolveAccountRejectsMismatchedSecret(t *testing.T) {
	ctx := context.Background()
	secrets := credentialMemory{}
	store := SecretCredentials{Store: secrets}
	marker, err := ProtectAccount(ctx, "mail", "reader@example.test", store)
	if err != nil {
		t.Fatal(err)
	}
	secrets["conn/mail/account"] = "other@example.test"
	if _, err := ResolveAccount(ctx, domain.Connection{ID: "mail", Account: marker}, store); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("mismatch = %v", err)
	}
	issues := IdentityIssues(ctx, []domain.Connection{{ID: "mail", Account: marker}}, store)
	if len(issues) != 1 || issues[0].Problem != IdentityProblemMismatch {
		t.Fatalf("issues = %+v", issues)
	}
}

type mismatchedCredentialStore struct{ SecretCredentials }

func (s mismatchedCredentialStore) Get(context.Context, string, string) (string, error) {
	return "another-value", nil
}

func TestProtectAccountVerifiesReadBack(t *testing.T) {
	ctx := context.Background()
	store := mismatchedCredentialStore{SecretCredentials{Store: credentialMemory{}}}
	if _, err := ProtectAccount(ctx, "mail", "reader@example.test", store); err == nil || !strings.Contains(err.Error(), "could not be verified") {
		t.Fatalf("verify = %v", err)
	}
	if _, err := ProtectAccount(ctx, "mail", "reader@example.test", nil); err == nil {
		t.Fatal("accepted a nil credential store")
	}
}

func TestIdentityKeyMatchesRawAndMarker(t *testing.T) {
	raw := domain.Connection{ID: "mail", Provider: "imap", BaseURL: "imaps://imap.example.test:993", Account: "Reader@Example.test"}
	migrated := raw
	migrated.Account = encodeIdentityMarker(AccountField, raw.Account)
	if IdentityKey(raw) != IdentityKey(migrated) {
		t.Fatalf("raw and migrated identities differ: %q != %q", IdentityKey(raw), IdentityKey(migrated))
	}
	other := raw
	other.Account = "other@example.test"
	if IdentityKey(raw) == IdentityKey(other) {
		t.Fatal("different accounts share an identity")
	}
	// A malformed or unrelated JSON value is a legacy raw account, not a marker.
	if IsAccountMarker(`{"$keychain":"other"}`) || IsAccountMarker(`{"$keychain":"account"}`) || IsAccountMarker("reader@example.test") {
		t.Fatal("accepted a non-marker")
	}
}

func TestProtectAndResolveIdentitySetting(t *testing.T) {
	ctx := context.Background()
	secrets := credentialMemory{}
	store := SecretCredentials{Store: secrets}
	raw := "reader@example.test"
	connection := domain.Connection{
		ID: "mail", Provider: "imap", BaseURL: "imaps://imap.example.test:993",
		Settings: map[string]string{"username": raw, "folders": "INBOX"},
	}

	protected, err := ProtectIdentity(ctx, connection, store)
	if err != nil {
		t.Fatal(err)
	}
	if connection.Settings["username"] != raw {
		t.Fatal("ProtectIdentity mutated the input")
	}
	if !IsSettingMarker("imap", "username", protected.Settings["username"]) {
		t.Fatalf("setting = %q", protected.Settings["username"])
	}
	if strings.Contains(protected.Settings["username"], "example.test") || protected.Settings["folders"] != "INBOX" {
		t.Fatalf("protected = %+v", protected.Settings)
	}
	if got := secrets["conn/mail/username"]; got != raw {
		t.Fatalf("secret = %q", got)
	}
	if !IdentitySettingMarkerMatches("imap", "username", protected.Settings["username"], raw) {
		t.Fatal("marker does not match the value")
	}

	resolved, err := ResolveIdentity(ctx, protected, store)
	if err != nil || resolved.Settings["username"] != raw || resolved.Settings["folders"] != "INBOX" {
		t.Fatalf("resolve = %+v %v", resolved.Settings, err)
	}
	settingsOnly, err := ResolveIdentitySettings(ctx, protected, store)
	if err != nil || settingsOnly.Settings["username"] != raw {
		t.Fatalf("settings resolve = %+v %v", settingsOnly.Settings, err)
	}

	delete(secrets, "conn/mail/username")
	_, err = ResolveIdentity(ctx, protected, store)
	if !errors.Is(err, domain.ErrNotFound) || !strings.Contains(err.Error(), "secret set mail username") {
		t.Fatalf("missing = %v", err)
	}
	issues := IdentityIssues(ctx, []domain.Connection{protected}, store)
	if len(issues) != 1 || issues[0].Field != "username" || issues[0].Problem != IdentityProblemMissing {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestRegistryBuildResolvesIdentitySetting(t *testing.T) {
	ctx := context.Background()
	secrets := credentialMemory{}
	store := SecretCredentials{Store: secrets}
	protected, err := ProtectIdentity(ctx, domain.Connection{
		ID: "mail", Provider: "imap", BaseURL: "imaps://imap.example.test:993",
		Settings: map[string]string{"username": "reader@example.test"},
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := domain.SourceDescriptor{
		Provider: "imap", DisplayName: "IMAP",
		Capabilities:   domain.Capabilities{AuthModes: []domain.AuthMode{domain.AuthToken}},
		DefaultPublish: domain.PublishLocalOnly,
		ConfigFields:   []domain.ConfigField{{Key: "username", Label: "Login", Required: true}},
	}
	var captured domain.Connection
	registry := NewRegistry()
	if err := registry.Register(descriptor, func(connection domain.Connection, _ Deps) (ports.Source, error) {
		captured = connection
		return stubSource{descriptor: descriptor}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Build(protected, Deps{Credentials: store}); err != nil || captured.Settings["username"] != "reader@example.test" {
		t.Fatalf("build = %q %v", captured.Settings["username"], err)
	}

	delete(secrets, "conn/mail/username")
	if _, err := registry.Build(protected, Deps{Credentials: store}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing at build = %v", err)
	}
}
