package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// setupScriptCodeBrain creates a local brain whose checkout has no Go module
// and selects the native graph engine.
func setupScriptCodeBrain(t *testing.T) (cfgPath, root string) {
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
	files := map[string]string{
		"src/models.ts": `export interface Entity {
  id: string;
}

export class User implements Entity {
  constructor(public id: string) {}

  label(): string {
    return this.id;
  }
}

export function createUser(id: string): User {
  return new User(id);
}
`,
		"src/app.ts": `import { createUser, type User } from "./models";

export class App {
  run(user: User): string {
    return createUser(user.id).label();
  }
}
`,
		"src/app.test.ts": `import { App } from "./app";

export function testApp(): string {
  return new App().run({ id: "1" });
}
`,
	}
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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

// TestCodeNativeScriptIndexAndQueries exercises the CLI surface on a
// TypeScript-only checkout: index, status, search, node, explore, impact and
// sync must all answer with script symbols.
func TestCodeNativeScriptIndexAndQueries(t *testing.T) {
	cfgPath, root := setupScriptCodeBrain(t)
	code, stdout, stderr := runForTest(t, "code", "index", "--project", "demo", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("index: exit %d: %s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "TypeScript/JavaScript") {
		t.Fatalf("index output = %q", stdout)
	}

	code, stdout, stderr = runForTest(t, "code", "status", "--project", "demo", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("status: exit %d: %s%s", code, stdout, stderr)
	}
	status := decodeCodeData[ports.GraphStatus](t, stdout)
	if status.Files != 3 || status.FilesByLanguage["typescript"] != 3 || status.Nodes == 0 || status.Edges == 0 {
		t.Fatalf("status = %+v", status)
	}

	code, stdout, stderr = runForTest(t, "code", "search", "User", "--project", "demo", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("search: exit %d: %s%s", code, stdout, stderr)
	}
	hits := decodeCodeData[[]ports.SymbolHit](t, stdout)
	if len(hits) == 0 || hits[0].Name != "User" || hits[0].Language != "typescript" || hits[0].Kind != "type" {
		t.Fatalf("hits = %+v", hits)
	}

	code, stdout, stderr = runForTest(t, "code", "node", "App.run", "--project", "demo", "--config", cfgPath, "--code", "--json")
	if code != 0 {
		t.Fatalf("node: exit %d: %s%s", code, stdout, stderr)
	}
	node := decodeCodeData[ports.NodeView](t, stdout)
	if len(node.Source) == 0 || !strings.Contains(node.Source[0].Text, "run(user: User)") {
		t.Fatalf("node source = %+v", node.Source)
	}
	foundCall := false
	for _, callee := range node.Callees {
		if callee.Name == "createUser" && callee.Confidence == 0.5 && callee.Provenance == "tree-sitter" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Fatalf("App.run callees = %+v", node.Callees)
	}

	code, stdout, stderr = runForTest(t, "code", "explore", "User", "--project", "demo", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("explore: exit %d: %s%s", code, stdout, stderr)
	}
	explore := decodeCodeData[ports.ExploreView](t, stdout)
	if len(explore.Symbols) == 0 || len(explore.Files) == 0 || explore.Bytes > ports.MaxGraphExploreBytes {
		t.Fatalf("explore = %+v", explore)
	}
	if explore.Files[0].Language != "typescript" {
		t.Fatalf("explore language = %+v", explore.Files[0])
	}

	code, stdout, stderr = runForTest(t, "code", "impact", "User", "--project", "demo", "--config", cfgPath, "--depth", "3", "--json")
	if code != 0 {
		t.Fatalf("impact: exit %d: %s%s", code, stdout, stderr)
	}
	impact := decodeCodeData[ports.ImpactView](t, stdout)
	foundCreate := false
	for _, hit := range impact.Hits {
		if hit.Name == "createUser" {
			foundCreate = true
		}
	}
	if !foundCreate {
		t.Fatalf("impact hits = %+v", impact.Hits)
	}

	// A body edit is file-scoped, not a full rebuild.
	appPath := filepath.Join(root, "src", "app.ts")
	appSource, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(appSource), "return createUser(user.id).label();", "return createUser(user.id).label() + \"\";", 1)
	if err := os.WriteFile(appPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runForTest(t, "code", "sync", "--project", "demo", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("sync: exit %d: %s%s", code, stdout, stderr)
	}
	syncResult := decodeCodeData[ports.GraphSyncResult](t, stdout)
	if syncResult.Changed != 1 || syncResult.Unsupported {
		t.Fatalf("sync = %+v", syncResult)
	}
	if strings.Contains(strings.Join(syncResult.Notes, " "), "full rebuild") {
		t.Fatalf("script body edit escalated: %+v", syncResult.Notes)
	}
}
