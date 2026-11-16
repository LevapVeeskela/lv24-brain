package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestGraphDBPathNative(t *testing.T) {
	dataDir, root := t.TempDir(), t.TempDir()
	cfg := config.BrainConfig{DataDir: dataDir, Graph: config.GraphConfig{Engine: ports.GraphEngineNative}}
	path, err := GraphDBPath(cfg, ports.GraphEngineNative, root, "demo", "")
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := filepath.Join(dataDir, "index", "graph")
	if !strings.HasPrefix(path, wantPrefix) {
		t.Fatalf("path = %s, want under %s", path, wantPrefix)
	}
	if !strings.HasSuffix(path, ".db") || !strings.Contains(filepath.Base(path), "demo-") {
		t.Fatalf("path = %s, want demo-<hash>.db", path)
	}

	// An explicit directory receives the default file name.
	dir := t.TempDir()
	fromDir, err := GraphDBPath(cfg, ports.GraphEngineNative, root, "demo", dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(fromDir) != dir || filepath.Base(fromDir) != filepath.Base(path) {
		t.Fatalf("directory override = %s, want %s", fromDir, filepath.Join(dir, filepath.Base(path)))
	}

	// An explicit .db file wins.
	file := filepath.Join(t.TempDir(), "custom.db")
	fromFile, err := GraphDBPath(cfg, ports.GraphEngineNative, root, "demo", file)
	if err != nil {
		t.Fatal(err)
	}
	if fromFile != file {
		t.Fatalf("file override = %s, want %s", fromFile, file)
	}
}

func TestGraphDBPathCodegraphDefault(t *testing.T) {
	root := t.TempDir()
	cfg := config.BrainConfig{DataDir: t.TempDir(), Graph: config.GraphConfig{Engine: ports.GraphEngineCodeGraph}}
	path, err := GraphDBPath(cfg, ports.GraphEngineCodeGraph, root, "demo", "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, ".codegraph", "codegraph.db")
	if path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
}

func TestOpenGraphNativeCreatesStore(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dataDir, root := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/demo\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "demo.go"), []byte("package demo\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.BrainConfig{DataDir: dataDir, Graph: config.GraphConfig{Engine: ports.GraphEngineNative}}
	path, err := GraphDBPath(cfg, ports.GraphEngineNative, root, "demo", "")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := OpenGraph(context.Background(), GraphOptions{
		Engine: ports.GraphEngineNative, Root: root, Project: "demo", DBPath: path, Create: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()
	if runtime.Native == nil || runtime.Index == nil {
		t.Fatal("native runtime has no writable index")
	}
	if _, err := runtime.Native.Index(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	status, err := runtime.Index.Status(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if status.Files != 1 || status.Nodes == 0 {
		t.Fatalf("status = %+v", status)
	}
	if _, err := OpenGraph(context.Background(), GraphOptions{Engine: "wat"}); err == nil {
		t.Fatal("unknown engine was accepted")
	}
}
