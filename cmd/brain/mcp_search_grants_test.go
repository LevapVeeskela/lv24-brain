package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/mcpserver"
)

// TestMCPItemSearchHonorsGrants is WB-0.9 for the MCP tool backend: a
// brain_search call with kinds ["item"] returns raw text only when a
// context-allow.json grant authorizes the connection.
func TestMCPItemSearchHonorsGrants(t *testing.T) {
	cfgPath, dataDir := connectionConfig(t, config.ModeLocal)
	cfg, err := loadRequiredConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.PutProject(ctx, domain.Project{ID: "demo", Name: "Demo"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	item := domain.Item{
		Ref: domain.ItemRef{ConnectionID: "work", ExternalID: "1"}, Kind: domain.ItemIssue,
		Title: "zebra migration", Body: "zebra body", ProjectID: "demo",
		UpdatedAt: now, ObservedAt: now,
	}.WithHash()
	if _, err := store.Items().Upsert(ctx, item); err != nil {
		t.Fatal(err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()
	if _, err := runtime.App.IndexItems(ctx, "work", []string{item.Key()}); err != nil {
		t.Fatal(err)
	}
	backend := mcpBackend{app: runtime.App}
	search := func() []mcpserver.Hit {
		t.Helper()
		hits, err := backend.Search(ctx, mcpserver.SearchInput{
			Query: "zebra", ProjectID: "demo", Kinds: []string{"item"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return hits
	}

	if hits := search(); len(hits) != 0 {
		t.Fatalf("ungranted MCP item search returned %+v", hits)
	}
	grant := `{"schemaVersion":1,"grants":[{"project":"demo","connection":"work"}]}`
	if err := os.WriteFile(filepath.Join(dataDir, fsstore.ContextAllowPath), []byte(grant), 0o600); err != nil {
		t.Fatal(err)
	}
	hits := search()
	if len(hits) != 1 || hits[0].Kind != "item" || hits[0].ProjectID != "demo" {
		t.Fatalf("granted MCP item search = %+v", hits)
	}
}
