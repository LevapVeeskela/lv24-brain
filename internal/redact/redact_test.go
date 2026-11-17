package redact

import (
	"strings"
	"testing"
)

// fake assembles a synthetic credential at runtime from fragments so that no
// complete credential-shaped literal appears in source (avoids secret scanners).
// Runtime value is byte-identical to the original fixture.
func fake(parts ...string) string { return strings.Join(parts, "") }

// secretCases is the shared 20-pattern corpus. Each entry is one credential a
// client could paste into a note, decision, attempt, handoff or state; none may
// survive storage. email appears only in credential form (URL userinfo or an
// assignment), matching the CLI redactor: a bare address is data, not a secret.
// All credential values below are synthetic fixtures assembled at runtime.
var secretCases = []struct {
	name string
	text string
	keep string
}{
	{"github classic", fake("used ", "ghp_", "0123456789abcdefghijklmnopqrstuvwxyzAB", " today"), "used"},
	{"github fine grained", fake("pat ", "github_pat_", "11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEF", " end"), "end"},
	{"gitlab pat", fake("glpat-", "abcdefghij0123456789", " later"), "later"},
	{"gitlab deploy token", fake("glft-", "abcdefghij0123456789", " later"), "later"},
	{"aws access key", fake("AKIA", "ABCDEFGHIJKLMNOP", " kept"), "kept"},
	{"aws session key", fake("ASIA", "ABCDEFGHIJKLMNOP", " kept"), "kept"},
	{"jwt", fake("eyJ", "hbGciOiJIUzI1NiJ9", ".", "eyJzdWIiOiIxMjM0NTY3ODkwIn0", ".", "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", " end"), "end"},
	{"openai project key", fake("sk-", "proj-abcdefghijklmnopqrstuvwxyz0123", " done"), "done"},
	{"anthropic key", fake("sk-", "ant-api03-abcdefghijklmnopqrstuvwxyz012345", " done"), "done"},
	{"pem private key", fake("-----BEGIN OPENSSH PRIVATE", " KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----", " after"), "after"},
	{"cut pem private key", fake("-----BEGIN RSA PRIVATE", " KEY-----\nAAAA"), ""},
	{"slack bot token", fake("xoxb-", "1234567890-abcdefghij", " end"), "end"},
	{"slack app token", fake("xapp-", "1-abcdefghijkl", " end"), "end"},
	{"slack webhook", fake("https://hooks.slack.com/services/", "T00/B00/abcdef", " done"), "done"},
	{"google api key", fake("AIza", "1234567890abcdefghijklmnopqrstuvwxy", " end"), "end"},
	{"npm token", fake("npm_", "abcdefghijklmnopqrstuvwxyz0123456789", " end"), "end"},
	{"stripe live key", fake("sk_", "live_abcdefghijklmnop1234", " end"), "end"},
	{"password assignment", fake("DB_PASSWORD=", "hunter2hunter2", " next"), "next"},
	{"api key json", fake(`"api_key": "abcd`, `1234efgh5678"`), ""},
	{"url userinfo email", fake("clone https://alice:", "s3cr3t", "Passw0rd", "@github.com/acme/x.git end"), "https://alice:[redacted]@github.com/acme/x.git"},
	{"authorization bearer", fake("Authorization: Bearer ", "abcdefghijklmnopqrst", " done"), "done"},
	{"url query token", fake("https://x.test/cb?access_token=", "abcdefghijklmnop", " end"), "end"},
}

// TestApplyIngestRemovesEverySecretPattern asserts that the shared ingestion
// redactor removes all 22 credential shapes and reports at least one hit.
func TestApplyIngestRemovesEverySecretPattern(t *testing.T) {
	for _, tc := range secretCases {
		t.Run(tc.name, func(t *testing.T) {
			got, count := ApplyIngest(tc.text)
			if count == 0 || !strings.Contains(got, Marker) {
				t.Fatalf("ApplyIngest(%q) = %q (%d), want a redaction", tc.text, got, count)
			}
			if tc.keep != "" && !strings.Contains(got, tc.keep) {
				t.Fatalf("ApplyIngest(%q) = %q, lost %q", tc.text, got, tc.keep)
			}
			for _, secret := range []string{
				"ghp_", "github_pat_", "glpat-", "glft-", "AKIA", "ASIA", "eyJ",
				"sk-proj-", "sk-ant-", "PRIVATE KEY", "xoxb-", "xapp-",
				"hooks.slack.com/services/T00", "AIza", "npm_", "sk_live_",
				fake("hunter", "2"), fake("abcd", "1234efgh5678"), fake("s3cr3t", "Passw0rd"), fake("abcdefghijklmnop", "qrst"),
			} {
				if strings.Contains(got, secret) {
					t.Fatalf("ApplyIngest(%q) = %q, still contains %q", tc.text, got, secret)
				}
			}
		})
	}
}

// TestApplyLeavesLegitimateTextAlone guards against false positives: ordinary
// engineering prose and bare e-mail addresses are data, not credentials.
func TestApplyLeavesLegitimateTextAlone(t *testing.T) {
	clean := []string{
		"Rotate the GitHub token monthly; see docs.",
		"password: yes",
		"Decision: keep sk-learn models in Python",
		"https://github.com/acme/x.git and git@github.com:acme/x.git",
		"Ask alice@example.test about the rollout; e-mail her the summary.",
		"Bearerer of bad news: the token expired.",
	}
	for _, text := range clean {
		if got, count := ApplyIngest(text); count != 0 || got != text {
			t.Errorf("ApplyIngest(%q) = %q (%d), want unchanged", text, got, count)
		}
	}
}

// TestApplyIngestIsStable guarantees that redacted text stays redacted (the
// marker is never mangled) and empty input is free. A second pass may match a
// marker inside an assignment again, but the rendered text must not change.
func TestApplyIngestIsStable(t *testing.T) {
	once, first := ApplyIngest(fake("token: ", "ghp_", "0123456789abcdefghijklmnopqrstuvwxyzAB", " end"))
	twice, _ := ApplyIngest(once)
	if once != twice || first == 0 {
		t.Fatalf("second pass changed %q -> %q (%d)", once, twice, first)
	}
	if got, count := ApplyIngest(""); got != "" || count != 0 {
		t.Fatalf("ApplyIngest(\"\") = %q (%d)", got, count)
	}
}
