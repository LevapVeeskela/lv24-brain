package auth

import (
	"strings"
	"testing"
)

var testParams = Argon2Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery", testParams)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("hash = %q, want argon2id encoding", hash)
	}
	ok, err := VerifyPassword(hash, "correct horse battery")
	if err != nil || !ok {
		t.Fatalf("VerifyPassword() = %v, %v; want true, nil", ok, err)
	}
	ok, err = VerifyPassword(hash, "wrong password")
	if err != nil {
		t.Fatalf("VerifyPassword(wrong) error = %v", err)
	}
	if ok {
		t.Fatal("VerifyPassword accepted a wrong password")
	}
}

func TestHashPasswordUsesFreshSalt(t *testing.T) {
	first, err := HashPassword("same password", testParams)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	second, err := HashPassword("same password", testParams)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if first == second {
		t.Fatal("password hashes reused a salt")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, encoded := range []string{"", "plaintext", "$argon2id$v=19$m=x,t=1,p=1$a$b", "$bcrypt$..."} {
		if _, err := VerifyPassword(encoded, "password"); err == nil {
			t.Fatalf("VerifyPassword(%q) accepted a malformed hash", encoded)
		}
	}
}

func TestValidatePasswordPolicy(t *testing.T) {
	if err := ValidatePassword("short"); err == nil {
		t.Fatal("ValidatePassword accepted a short password")
	}
	if err := ValidatePassword(strings.Repeat("x", MaxPasswordLength+1)); err == nil {
		t.Fatal("ValidatePassword accepted an oversized password")
	}
	if err := ValidatePassword("long-enough-password"); err != nil {
		t.Fatalf("ValidatePassword() error = %v", err)
	}
}

func TestHashNeedsRehash(t *testing.T) {
	hash, err := HashPassword("password-value", Argon2Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1})
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if HashNeedsRehash(hash, testParams) {
		t.Fatal("HashNeedsRehash should be false for equal parameters")
	}
	if !HashNeedsRehash(hash, Argon2Params{Memory: 16 * 1024, Iterations: 1, Parallelism: 1}) {
		t.Fatal("HashNeedsRehash should be true for stronger parameters")
	}
}
