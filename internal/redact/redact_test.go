package redact

import (
	"strings"
	"testing"
)

// secretCases is the shared 20-pattern corpus. Each entry is one credential a
// client could paste into a note, decision, attempt, handoff or state; none may
// survive storage. email appears only in credential form (URL userinfo or an
// assignment), matching the CLI redactor: a bare address is data, not a secret.
var secretCases = []struct {
	name string
	text string
	keep string
}{
	{"github classic", "used ghp_0123456789abcdefghijklmnopqrstuvwxyzAB today", "used"},
	{"github fine grained", "pat github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEF end", "end"},
	{"gitlab pat", "glpat-abcdefghij0123456789 later", "later"},
	{"gitlab deploy token", "glft-abcdefghij0123456789 later", "later"},
	{"aws access key", "AKIAABCDEFGHIJKLMNOP kept", "kept"},
	{"aws session key", "ASIAABCDEFGHIJKLMNOP kept", "kept"},
	{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U end", "end"},
	{"openai project key", "sk-proj-abcdefghijklmnopqrstuvwxyz0123 done", "done"},
	{"anthropic key", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345 done", "done"},
	{"pem private key", "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY----- after", "after"},
	{"cut pem private key", "-----BEGIN RSA PRIVATE KEY-----\nAAAA", ""},
	{"slack bot token", "xoxb-1234567890-abcdefghij end", "end"},
	{"slack app token", "xapp-1-abcdefghijkl end", "end"},
	{"slack webhook", "https://hooks.slack.com/services/T00/B00/abcdef done", "done"},
	{"google api key", "AIza1234567890abcdefghijklmnopqrstuvwxy end", "end"},
	{"npm token", "npm_abcdefghijklmnopqrstuvwxyz0123456789 end", "end"},
	{"stripe live key", "sk_live_abcdefghijklmnop1234 end", "end"},
	{"password assignment", "DB_PASSWORD=hunter2hunter2 next", "next"},
	{"api key json", `"api_key": "abcd1234efgh5678"`, ""},
	{"url userinfo email", "clone https://alice:s3cr3tPassw0rd@github.com/acme/x.git end", "https://alice:[redacted]@github.com/acme/x.git"},
	{"authorization bearer", "Authorization: Bearer abcdefghijklmnopqrst done", "done"},
	{"url query token", "https://x.test/cb?access_token=abcdefghijklmnop end", "end"},
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
				"hunter2", "abcd1234efgh5678", "s3cr3tPassw0rd", "abcdefghijklmnopqrst",
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
	once, first := ApplyIngest("token: ghp_0123456789abcdefghijklmnopqrstuvwxyzAB end")
	twice, _ := ApplyIngest(once)
	if once != twice || first == 0 {
		t.Fatalf("second pass changed %q -> %q (%d)", once, twice, first)
	}
	if got, count := ApplyIngest(""); got != "" || count != 0 {
		t.Fatalf("ApplyIngest(\"\") = %q (%d)", got, count)
	}
}
