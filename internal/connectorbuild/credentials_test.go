package connectorbuild

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type credentialMemory map[string]string

func (s credentialMemory) Get(_ context.Context, key string) (string, error) {
	v, ok := s[key]
	if !ok {
		return "", domain.ErrNotFound
	}
	return v, nil
}
func (s credentialMemory) Set(_ context.Context, key, value string) error { s[key] = value; return nil }
func (s credentialMemory) Delete(_ context.Context, key string) error     { delete(s, key); return nil }
func (s credentialMemory) Names(context.Context) ([]string, error)        { return nil, nil }

func TestSecretCredentialsCRUD(t *testing.T) {
	ctx := context.Background()
	secrets := credentialMemory{}
	var store ports.CredentialStore = SecretCredentials{Store: secrets}
	if _, err := store.Get(ctx, "github", "token"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if err := store.Set(ctx, "github", "token", "private-value"); err != nil {
		t.Fatal(err)
	}
	if secrets["conn/github/token"] != "private-value" {
		t.Fatal("wrong namespace")
	}
	got, err := store.Get(ctx, "github", "token")
	if err != nil || got != "private-value" {
		t.Fatal("round trip failed")
	}
	if err := store.Delete(ctx, "github", "token"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "github", "token"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted = %v", err)
	}
}

func TestSecretCredentialsFoldedNamesDoNotCollide(t *testing.T) {
	fold := strings.NewReplacer("/", "_", "-", "_", ".", "_")
	seen := map[string]string{}
	for _, id := range []string{"a", "a-b", "a--b", "a-b-c", "ab", "abc", "0", "0-a"} {
		for _, field := range []string{"token", "btoken", "password", "c", "bc", "c1"} {
			key, err := domain.CredentialKey(id, field)
			if err != nil {
				t.Fatal(err)
			}
			folded := strings.ToLower(fold.Replace(key))
			if prior, ok := seen[folded]; ok {
				t.Fatalf("collision %s and %s", prior, key)
			}
			seen[folded] = key
		}
	}
}

func TestSecretCredentialsValidateBeforeCallingStore(t *testing.T) {
	ctx := context.Background()
	for _, store := range []SecretCredentials{{}, {Store: credentialMemory{}}} {
		if err := store.Set(ctx, "bad_id", "token", "private-value"); err == nil {
			t.Fatal("accepted invalid ID")
		}
		if err := store.Delete(ctx, "valid", "pass-word"); err == nil {
			t.Fatal("accepted invalid field")
		}
		if _, err := store.Get(ctx, "valid", "pass_word"); err == nil {
			t.Fatal("accepted invalid field")
		}
	}
	if err := (SecretCredentials{}).Set(ctx, "valid", "token", "private-value"); err == nil {
		t.Fatal("accepted nil store")
	}
	if err := (SecretCredentials{}).Delete(ctx, "valid", "token"); err == nil {
		t.Fatal("accepted nil store")
	}
}
