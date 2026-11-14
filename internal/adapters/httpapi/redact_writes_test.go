package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// writeSecrets is the 20-pattern corpus WB-0.7 requires every server write
// handler to redact. Each value is a credential shape an API client could
// paste; none may reach the store.
var writeSecrets = []string{
	"ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
	"github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEF",
	"glpat-abcdefghij0123456789",
	"glrt-abcdefghij0123456789",
	"AKIAABCDEFGHIJKLMNOP",
	"ASIAABCDEFGHIJKLMNOP",
	"AIza1234567890abcdefghijklmnopqrstuvwxy",
	"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
	"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----",
	"-----BEGIN RSA PRIVATE KEY-----\nAAAA",
	"sk-proj-abcdefghijklmnopqrstuvwxyz0123",
	"sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345",
	"xoxb-1234567890-abcdefghij",
	"xapp-1-abcdefghijkl",
	"https://hooks.slack.com/services/T00/B00/abcdef",
	"npm_abcdefghijklmnopqrstuvwxyz0123456789",
	"sk_live_abcdefghijklmnop1234",
	"DB_PASSWORD=hunter2hunter2",
	"api_key=abcd1234efgh5678",
	"https://alice:s3cr3tPassw0rd@github.com/acme/x.git",
}

// secretFragments are the substrings that must never appear in a stored or
// returned record. They are checked independently of the corpus so a partially
// redacted match is still caught.
var secretFragments = []string{
	"ghp_", "github_pat_", "glpat-", "glrt-", "AKIA", "ASIA", "AIza", "eyJ",
	"PRIVATE KEY", "sk-proj-", "sk-ant-", "xoxb-", "xapp-",
	"hooks.slack.com/services/T00", "npm_", "sk_live_", "hunter2",
	"abcd1234efgh5678", "s3cr3tPassw0rd",
}

func corpus() string {
	return strings.Join(writeSecrets, " | ")
}

// assertNoSecrets walks every string of a JSON payload and fails when a secret
// fragment survives without a redaction marker.
func assertNoSecrets(t *testing.T, label string, payload []byte) {
	t.Helper()
	var body any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("%s is not JSON: %v (%s)", label, err, payload)
	}
	var walk func(value any) string
	walk = func(value any) string {
		switch typed := value.(type) {
		case string:
			return typed
		case []any:
			parts := make([]string, 0, len(typed))
			for _, item := range typed {
				parts = append(parts, walk(item))
			}
			return strings.Join(parts, "\n")
		case map[string]any:
			parts := make([]string, 0, len(typed))
			for _, item := range typed {
				parts = append(parts, walk(item))
			}
			return strings.Join(parts, "\n")
		default:
			return ""
		}
	}
	text := walk(body)
	for _, fragment := range secretFragments {
		if strings.Contains(text, fragment) {
			t.Fatalf("%s leaked %q: %s", label, fragment, payload)
		}
	}
	if !strings.Contains(text, "[redacted]") {
		t.Fatalf("%s carries no redaction marker: %s", label, payload)
	}
}

func TestServerWritesRedactEverySecretPattern(t *testing.T) {
	ts, _, store := newTestServer(t, func(cfg *Config) { cfg.MaxBodyBytes = 1 << 20 })
	admin := loginAs(t, ts.URL, adminLogin, adminPassword).AccessToken
	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects", map[string]any{
		"id": "sec", "name": "Security",
	}, admin)
	requireStatus(t, resp, payload, http.StatusCreated)
	ctx := context.Background()

	t.Run("note", func(t *testing.T) {
		resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects/sec/notes", map[string]any{
			"title": corpus(), "body": corpus(), "tags": []string{corpus()},
		}, admin)
		requireStatus(t, resp, payload, http.StatusCreated)
		assertNoSecrets(t, "note response", payload)
		id := createdID(t, payload)
		stored, err := store.GetNote(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(stored)
		assertNoSecrets(t, "stored note", raw)

		// Update keeps the same guarantee.
		resp, payload = jsonRequest(t, http.MethodPatch, ts.URL+"/v1/notes/"+id, map[string]any{
			"title": "updated", "body": corpus(), "tags": []string{corpus()},
		}, admin)
		requireStatus(t, resp, payload, http.StatusOK)
		assertNoSecrets(t, "updated note", payload)
	})

	t.Run("decision", func(t *testing.T) {
		resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects/sec/decisions", map[string]any{
			"title": corpus(), "status": "accepted", "context": corpus(),
			"alternatives": []string{corpus()}, "consequences": []string{corpus()}, "evidence": []string{corpus()},
		}, admin)
		requireStatus(t, resp, payload, http.StatusCreated)
		assertNoSecrets(t, "decision response", payload)
		id := createdID(t, payload)
		stored, err := store.GetDecision(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(stored)
		assertNoSecrets(t, "stored decision", raw)
	})

	t.Run("attempt", func(t *testing.T) {
		resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects/sec/attempts", map[string]any{
			"title": corpus(), "hypothesis": corpus(), "result": corpus(), "reason": corpus(),
		}, admin)
		requireStatus(t, resp, payload, http.StatusCreated)
		assertNoSecrets(t, "attempt response", payload)
		id := createdID(t, payload)
		stored, err := store.GetAttempt(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(stored)
		assertNoSecrets(t, "stored attempt", raw)
	})

	t.Run("handoff", func(t *testing.T) {
		resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects/sec/handoffs", map[string]any{
			"summary": corpus(), "gitContext": corpus(), "nextAction": corpus(),
		}, admin)
		requireStatus(t, resp, payload, http.StatusCreated)
		assertNoSecrets(t, "handoff response", payload)
		id := createdID(t, payload)
		stored, err := store.GetHandoff(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(stored)
		assertNoSecrets(t, "stored handoff", raw)
	})

	t.Run("state", func(t *testing.T) {
		resp, payload := jsonRequest(t, http.MethodPut, ts.URL+"/v1/projects/sec/state", map[string]any{
			"goal": corpus(), "nextAction": corpus(),
			"active": []string{corpus()}, "blocked": []string{corpus()}, "openQuestions": []string{corpus()},
		}, admin)
		requireStatus(t, resp, payload, http.StatusOK)
		assertNoSecrets(t, "state response", payload)
		stored, err := store.GetState(ctx, "sec")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(stored)
		assertNoSecrets(t, "stored state", raw)
	})
}

// TestServerWritesPreserveCleanText is the false-positive guard: ordinary
// prose, including a bare e-mail address, is stored byte for byte.
func TestServerWritesPreserveCleanText(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword).AccessToken
	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects", map[string]any{
		"id": "clean", "name": "Clean",
	}, admin)
	requireStatus(t, resp, payload, http.StatusCreated)

	const body = "Rotate the GitHub token monthly; ask alice@example.test about the rollout. password: yes."
	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects/clean/notes", map[string]any{
		"title": "Ops", "body": body, "tags": []string{"ops"},
	}, admin)
	requireStatus(t, resp, payload, http.StatusCreated)
	id := createdID(t, payload)
	stored, err := store.GetNote(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Body != body || stored.Title != "Ops" || len(stored.Tags) != 1 || stored.Tags[0] != "ops" {
		t.Fatalf("clean text changed: %+v", stored.Note)
	}
}

// TestWriteRedactionAuditEvent proves the redaction is visible to operators
// (the AC asks for a false-positive report) without leaking the text.
func TestWriteRedactionAuditEvent(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword).AccessToken
	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects", map[string]any{
		"id": "audit", "name": "Audit",
	}, admin)
	requireStatus(t, resp, payload, http.StatusCreated)
	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects/audit/notes", map[string]any{
		"body": "token ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
	}, admin)
	requireStatus(t, resp, payload, http.StatusCreated)

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/audit?action="+AuditKnowledgeRedacted, nil, admin)
	requireStatus(t, resp, payload, http.StatusOK)
	var body struct {
		Items []AuditEvent `json:"items"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].Action != AuditKnowledgeRedacted {
		t.Fatalf("audit items = %+v", body.Items)
	}
	if count, _ := body.Items[0].Detail["redactions"].(float64); count != 1 {
		t.Fatalf("audit detail = %+v", body.Items[0].Detail)
	}
	if strings.Contains(string(payload), "ghp_") || strings.Contains(string(payload), "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("audit leaked the secret: %s", payload)
	}
}
