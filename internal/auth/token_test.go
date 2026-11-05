package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func testSigner(t *testing.T, now func() time.Time) *Signer {
	t.Helper()
	return testSignerWithSecret(t, now, strings.Repeat("k", MinJWTSecretLength))
}

func testSignerWithSecret(t *testing.T, now func() time.Time, secret string) *Signer {
	t.Helper()
	signer, err := NewSigner(SignerConfig{
		Secret:   []byte(secret),
		Issuer:   "test-issuer",
		Audience: "test-audience",
		TTL:      time.Minute,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}
	return signer
}

func TestSignerIssueAndParse(t *testing.T) {
	signer := testSigner(t, time.Now)
	user := domain.User{ID: "user_1", Login: "user@example.test", DisplayName: "User", Role: domain.RoleMember}
	token, expires, err := signer.Issue(user, "sess_1", "fam_1")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if time.Until(expires) <= 0 {
		t.Fatal("access token is already expired")
	}
	claims, err := signer.Parse(token)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if claims.Subject != user.ID || claims.Login != user.Login || claims.Role != string(user.Role) {
		t.Fatalf("claims = %+v", claims)
	}
	if claims.SessionID != "sess_1" || claims.FamilyID != "fam_1" {
		t.Fatalf("session claims = %+v", claims)
	}
}

func TestSignerRejectsTampering(t *testing.T) {
	signer := testSigner(t, time.Now)
	user := domain.User{ID: "user_1", Login: "user@example.test", Role: domain.RoleViewer}
	token, _, err := signer.Issue(user, "sess_1", "fam_1")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	tampered := token[:len(token)-2] + "aa"
	if _, err := signer.Parse(tampered); err == nil {
		t.Fatal("Parse accepted a tampered token")
	}
	other := testSignerWithSecret(t, time.Now, strings.Repeat("x", MinJWTSecretLength))
	if _, err := other.Parse(token); err == nil {
		t.Fatal("Parse accepted a token signed by another key")
	}
}

func TestSignerRejectsExpiredToken(t *testing.T) {
	now := time.Now()
	signer := testSigner(t, func() time.Time { return now })
	user := domain.User{ID: "user_1", Login: "user@example.test", Role: domain.RoleViewer}
	token, _, err := signer.Issue(user, "sess_1", "fam_1")
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	later := now.Add(2 * time.Minute)
	expiredSigner := testSigner(t, func() time.Time { return later })
	if _, err := expiredSigner.Parse(token); err == nil {
		t.Fatal("Parse accepted an expired token")
	}
}

func TestGenerateTokenHashes(t *testing.T) {
	raw, hash, display, err := GenerateToken("brain_", 32)
	if err != nil {
		t.Fatalf("GenerateToken() error = %v", err)
	}
	if !strings.HasPrefix(raw, "brain_") {
		t.Fatalf("raw = %q", raw)
	}
	if hash != HashToken(raw) {
		t.Fatal("hash does not match HashToken")
	}
	if display == raw {
		t.Fatal("display value must not expose the whole token")
	}
}
