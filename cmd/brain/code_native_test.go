package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// setupNativeCodeBrain creates a local brain whose demo checkout is a real
// multi-package Go module and selects the native graph engine.
func setupNativeCodeBrain(t *testing.T) (cfgPath, root string) {
	t.Helper()
	t.Setenv("BRAIN_CONFIG", "")
	t.Setenv("GOWORK", "off")
	base := t.TempDir()
	dataDir := filepath.Join(base, "brain")
	root = filepath.Join(base, "repo")
	cfgPath = filepath.Join(dataDir, "brain.yaml")

	steps := [][]string{
		{"init", "--mode", "local", "--data-dir", dataDir},
		{"project", "set", "demo", "--name", "Demo", "--add-path", root, "--config", cfgPath},
	}
	for _, args := range steps {
		if code, _, stderr := runForTest(t, args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, stderr)
		}
	}
	writeNativeCheckout(t, root)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Graph = config.GraphConfig{Engine: ports.GraphEngineNative}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	return cfgPath, root
}

// writeNativeCheckout writes a three-file module: a library, its test and a
// caller.
func writeNativeCheckout(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"go.mod": "module example.com/demo\n\ngo 1.21\n",
		"lib/lib.go": `package lib

// Add returns the sum of two values.
func Add(left, right int) int {
	return left + right
}

// Total sums a slice.
func Total(values []int) int {
	total := 0
	for _, value := range values {
		total = Add(total, value)
	}
	return total
}
`,
		"lib/lib_test.go": `package lib

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("bad sum")
	}
}
`,
		"app/main.go": `package main

import (
	"fmt"

	"example.com/demo/lib"
)

func Run() int {
	return lib.Total([]int{1, 2, 3})
}

func main() {
	fmt.Println(Run())
}
`,
	}
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// decodeCodeData reads the shared {schema, data} JSON envelope.
func decodeCodeData[T any](t *testing.T, raw string) T {
	t.Helper()
	var envelope struct {
		Schema int             `json:"schema"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("json envelope: %v\n%s", err, raw)
	}
	var out T
	if err := json.Unmarshal(envelope.Data, &out); err != nil {
		t.Fatalf("json data: %v\n%s", err, raw)
	}
	return out
}

func TestCodeNativeIndexQueryAndSync(t *testing.T) {
	cfgPath, root := setupNativeCodeBrain(t)

	code, stdout, stderr := runForTest(t, "code", "index", "--project", "demo", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("index: exit %d: %s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "indexed") {
		t.Fatalf("index output = %q", stdout)
	}

	code, stdout, stderr = runForTest(t, "code", "status", "--project", "demo", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("status: exit %d: %s%s", code, stdout, stderr)
	}
	var status ports.GraphStatus
	status = decodeCodeData[ports.GraphStatus](t, stdout)
	if status.Engine != ports.GraphEngineNative || status.Files != 3 || status.Nodes == 0 || status.Edges == 0 {
		t.Fatalf("status = %+v", status)
	}
	if !strings.Contains(filepath.ToSlash(status.DBPath), "index/graph") {
		t.Fatalf("db path = %s", status.DBPath)
	}
	if strings.HasPrefix(filepath.Clean(status.DBPath), filepath.Clean(root)) {
		t.Fatalf("db path %s is inside the checkout", status.DBPath)
	}

	code, stdout, stderr = runForTest(t, "code", "search", "Add", "--project", "demo", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("search: exit %d: %s%s", code, stdout, stderr)
	}
	var hits []ports.SymbolHit
	hits = decodeCodeData[[]ports.SymbolHit](t, stdout)
	if len(hits) == 0 || hits[0].Name != "Add" || hits[0].Match != ports.GraphMatchExact {
		t.Fatalf("hits = %+v", hits)
	}

	code, stdout, stderr = runForTest(t, "code", "node", "Add", "--project", "demo", "--config", cfgPath, "--code", "--json")
	if code != 0 {
		t.Fatalf("node: exit %d: %s%s", code, stdout, stderr)
	}
	var node ports.NodeView
	node = decodeCodeData[ports.NodeView](t, stdout)
	if len(node.Source) == 0 || !strings.Contains(node.Source[0].Text, "func Add") {
		t.Fatalf("node source = %+v", node.Source)
	}
	if len(node.Callers) == 0 || len(node.Callees) != 0 {
		t.Fatalf("node neighbours: callers=%+v callees=%+v", node.Callers, node.Callees)
	}

	code, stdout, stderr = runForTest(t, "code", "explore", "Total", "--project", "demo", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("explore: exit %d: %s%s", code, stdout, stderr)
	}
	var explore ports.ExploreView
	explore = decodeCodeData[ports.ExploreView](t, stdout)
	if len(explore.Symbols) == 0 || len(explore.Files) == 0 || explore.Bytes > ports.MaxGraphExploreBytes {
		t.Fatalf("explore = %+v", explore)
	}

	code, stdout, stderr = runForTest(t, "code", "impact", "Add", "--project", "demo", "--config", cfgPath, "--depth", "2", "--json")
	if code != 0 {
		t.Fatalf("impact: exit %d: %s%s", code, stdout, stderr)
	}
	var impact ports.ImpactView
	impact = decodeCodeData[ports.ImpactView](t, stdout)
	foundTest := false
	for _, hit := range impact.Hits {
		if hit.Name == "TestAdd" && hit.Test {
			foundTest = true
		}
	}
	if !foundTest {
		t.Fatalf("TestAdd is not a test hit: %+v", impact.Hits)
	}

	// A body edit is picked up by sync without a full rebuild.
	libPath := filepath.Join(root, "lib", "lib.go")
	content, err := os.ReadFile(libPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(content), "return left + right", "return left + right + 0", 1)
	if err := os.WriteFile(libPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runForTest(t, "code", "sync", "--project", "demo", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("sync: exit %d: %s%s", code, stdout, stderr)
	}
	var syncResult ports.GraphSyncResult
	syncResult = decodeCodeData[ports.GraphSyncResult](t, stdout)
	if syncResult.Changed != 1 || syncResult.Unsupported {
		t.Fatalf("sync = %+v", syncResult)
	}

	// A rebuild is idempotent.
	code, stdout, stderr = runForTest(t, "code", "index", "--project", "demo", "--config", cfgPath, "--rebuild")
	if code != 0 {
		t.Fatalf("rebuild: exit %d: %s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "requested rebuild") {
		t.Fatalf("rebuild output = %q", stdout)
	}
}

func TestCodeNativeMissingIndexAndOffEngine(t *testing.T) {
	cfgPath, _ := setupNativeCodeBrain(t)
	code, stdout, stderr := runForTest(t, "code", "status", "--project", "demo", "--config", cfgPath)
	if code == 0 {
		t.Fatalf("status without an index succeeded: %s", stdout)
	}
	if !strings.Contains(stderr, "brain code index") {
		t.Fatalf("missing-index message = %q", stderr)
	}
	code, _, stderr = runForTest(t, "code", "status", "--project", "demo", "--config", cfgPath, "--engine", "off")
	if code == 0 || !strings.Contains(stderr, "off") {
		t.Fatalf("off engine: exit %d: %s", code, stderr)
	}
}

func TestCodeNativeRejectsCodegraphIndexing(t *testing.T) {
	cfgPath, root := setupNativeCodeBrain(t)
	if err := os.MkdirAll(filepath.Join(root, ".codegraph"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runForTest(t,
		"code", "index", "--project", "demo", "--config", cfgPath, "--engine", "codegraph", "--db", filepath.Join(root, ".codegraph", "codegraph.db"))
	if code == 0 {
		t.Fatalf("codegraph indexing succeeded: %s", stdout)
	}
	if !strings.Contains(stderr, "codegraph") {
		t.Fatalf("message = %q", stderr)
	}
}
