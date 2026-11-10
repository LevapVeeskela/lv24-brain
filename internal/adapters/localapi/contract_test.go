package localapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	contractID         = "localapi-v1"
	contractGoldenFile = "testdata/localapi-v1.operations"
	// updateContractEnv deliberately rewrites the golden operation set. Set it
	// only when the OpenAPI change is intended, and review the resulting diff.
	updateContractEnv = "UPDATE_LOCALAPI_CONTRACT"
)

type openAPIDocument struct {
	OpenAPI   string                                `json:"openapi"`
	XContract string                                `json:"x-contract"`
	Paths     map[string]map[string]json.RawMessage `json:"paths"`
}

func loadOpenAPI(t *testing.T) openAPIDocument {
	t.Helper()
	data, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatalf("read openapi.json: %v", err)
	}
	var document openAPIDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse openapi.json: %v", err)
	}
	return document
}

// documentedOperations returns the normalized, sorted METHOD /path set. Path
// items may also contain "parameters"; only HTTP operations are collected.
func documentedOperations(t *testing.T, paths map[string]map[string]json.RawMessage) []string {
	t.Helper()
	methods := map[string]bool{
		http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
		http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
		http.MethodOptions: true, http.MethodTrace: true, http.MethodConnect: true,
	}
	operations := make([]string, 0, len(paths))
	for path, item := range paths {
		if !strings.HasPrefix(path, "/") {
			t.Errorf("documented path %q is not absolute", path)
			continue
		}
		for key := range item {
			method := strings.ToUpper(key)
			if !methods[method] {
				continue
			}
			operations = append(operations, method+" "+path)
		}
	}
	sort.Strings(operations)
	return operations
}

func readGoldenOperations(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(contractGoldenFile)
	if err != nil {
		t.Fatalf("read %s (set %s=1 to create it): %v", contractGoldenFile, updateContractEnv, err)
	}
	lines := make([]string, 0, len(data))
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	sort.Strings(lines)
	return lines
}

func TestOpenAPIDeclaresLocalAPIV1Contract(t *testing.T) {
	document := loadOpenAPI(t)
	if document.OpenAPI != "3.0.3" {
		t.Fatalf("openapi=%q, want 3.0.3", document.OpenAPI)
	}
	if document.XContract != contractID {
		t.Fatalf("x-contract=%q, want %q", document.XContract, contractID)
	}
	if len(document.Paths) == 0 {
		t.Fatal("openapi.json documents no paths")
	}
}

func TestLocalAPIV1OperationSetIsFrozen(t *testing.T) {
	documented := documentedOperations(t, loadOpenAPI(t).Paths)
	if os.Getenv(updateContractEnv) == "1" {
		if err := os.MkdirAll(filepath.Dir(contractGoldenFile), 0o755); err != nil {
			t.Fatal(err)
		}
		content := strings.Join(documented, "\n") + "\n"
		if err := os.WriteFile(contractGoldenFile, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s with %d operations", contractGoldenFile, len(documented))
		return
	}

	golden := readGoldenOperations(t)
	inGolden := make(map[string]bool, len(golden))
	for _, operation := range golden {
		inGolden[operation] = true
	}
	documentedSet := make(map[string]bool, len(documented))
	for _, operation := range documented {
		documentedSet[operation] = true
	}
	var added, removed []string
	for _, operation := range documented {
		if !inGolden[operation] {
			added = append(added, operation)
		}
	}
	for _, operation := range golden {
		if !documentedSet[operation] {
			removed = append(removed, operation)
		}
	}
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("localapi-v1 operation set changed (added=%v removed=%v); "+
			"additive operations are allowed but must be recorded deliberately with %s=1, "+
			"while removals or method changes require a new major contract", added, removed, updateContractEnv)
	}
}

// TestLocalAPIV1OperationsDispatch proves each documented operation reaches a
// handler instead of the router fallback. Every subtest builds a fresh fixture,
// so the result is independent of test order and shared state.
func TestLocalAPIV1OperationsDispatch(t *testing.T) {
	for _, operation := range documentedOperations(t, loadOpenAPI(t).Paths) {
		method, path, ok := strings.Cut(operation, " ")
		if !ok {
			t.Fatalf("malformed operation %q", operation)
		}
		name := strings.NewReplacer("/", "_", " ", "_", "{", "", "}", "").Replace(operation)
		t.Run(name, func(t *testing.T) {
			fixture := newContractFixture(t)
			if method == http.MethodGet && path == "/v1/runs/{id}/log" {
				// The fixture producer is a live channel; close it so the SSE
				// snapshot terminates instead of waiting for a heartbeat.
				close(fixture.runs.log.lines)
			}
			response := request(fixture, method, contractPath(path), contractBody(method, path), nil)
			switch {
			case response.Code == http.StatusNotFound || response.Code == http.StatusMethodNotAllowed:
				t.Fatalf("documented operation %s dispatched as %d: %s", operation, response.Code, response.Body.String())
			case response.Code >= http.StatusInternalServerError:
				t.Fatalf("documented operation %s is unwired: %d %s", operation, response.Code, response.Body.String())
			}
		})
	}
}

func newContractFixture(t *testing.T) *testFixture {
	t.Helper()
	fixture := newFixture(t)
	if err := fixture.connections.Create(context.Background(), domain.Connection{ID: "demo-1", Provider: "demo"}); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	fixture.runs.runs = []domain.ConnectorRun{{
		ID: "run-1", Source: "demo", ConnectionID: "demo-1", Status: domain.ConnectorSuccess,
	}}
	return fixture
}

// contractPath replaces documented path parameters with values the fixture
// can resolve. Parameters are opaque to clients and never parsed. The mapping
// is explicit so a newly documented parameterized route fails loudly instead
// of silently dispatching to the router fallback.
func contractPath(path string) string {
	switch path {
	case "/v1/connections/{id}":
		return "/v1/connections/demo-1"
	case "/v1/connections/{id}/credentials/{field}":
		return "/v1/connections/demo-1/credentials/token"
	case "/v1/connections/{id}/test":
		return "/v1/connections/demo-1/test"
	case "/v1/connections/{id}/run":
		return "/v1/connections/demo-1/run"
	case "/v1/runs/{id}":
		return "/v1/runs/run-1"
	case "/v1/runs/{id}/cancel":
		return "/v1/runs/run-1/cancel"
	case "/v1/runs/{id}/log":
		return "/v1/runs/run-1/log"
	case "/v1/schedules/{connectionId}":
		return "/v1/schedules/demo-1"
	default:
		return path
	}
}

// contractBody supplies the smallest valid body for operations that require
// one. GET and DELETE operations use no body.
func contractBody(method, path string) string {
	switch {
	case method == http.MethodPost && path == "/v1/connections":
		return `{"id":"demo-2","provider":"demo"}`
	case method == http.MethodPatch && path == "/v1/connections/{id}":
		return `{"account":"smoke"}`
	case method == http.MethodPost && strings.HasSuffix(path, "/credentials/{field}"):
		return `{"value":"smoke-secret"}`
	case method == http.MethodPost && strings.HasSuffix(path, "/run"):
		return `{}`
	case method == http.MethodPut && strings.HasPrefix(path, "/v1/schedules/"):
		return `{"schedule":"@daily"}`
	case method == http.MethodPut && path == "/v1/settings":
		return `{"theme":"light"}`
	case method == http.MethodPost && path == "/v1/engine/update":
		return `{"confirm":true}`
	}
	return ""
}
