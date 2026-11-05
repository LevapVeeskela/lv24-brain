package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestMemoryConnectorRunOwnsMutableFields(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	finished := time.Now().UTC()
	counts := map[string]int{"repos": 1}
	run, _, err := store.CreateConnectorRun(ctx, domain.ConnectorRun{ID: "run-alias", Source: "github", Status: domain.ConnectorSuccess, Counts: counts, FinishedAt: &finished})
	if err != nil {
		t.Fatal(err)
	}
	counts["repos"] = 9
	finished = finished.Add(time.Hour)
	run.Counts["repos"] = 8
	got, err := store.GetConnectorRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Counts["repos"] != 1 || got.FinishedAt.Equal(finished) {
		t.Fatalf("create retained mutable aliases: %+v", got)
	}
	got.Counts["repos"] = 7
	newCounts := map[string]int{"repos": 2}
	updated, _, err := store.FinishConnectorRun(ctx, run.ID, domain.ConnectorSuccess, newCounts, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	newCounts["repos"] = 6
	updated.Counts["repos"] = 5
	listed, err := store.ListConnectorRuns(ctx, ConnectorRunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if listed[0].Counts["repos"] != 2 {
		t.Fatalf("finish retained mutable aliases: %+v", listed[0])
	}
	listed[0].Counts["repos"] = 4
	*listed[0].FinishedAt = time.Time{}
	got, err = store.GetConnectorRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Counts["repos"] != 2 || got.FinishedAt.IsZero() {
		t.Fatalf("read exposed mutable aliases: %+v", got)
	}
}

func TestMemoryConnectorRunTieOrderMatchesPostgres(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	started := time.Now().UTC()
	for _, id := range []string{"run-b", "run-a"} {
		if _, _, err := store.CreateConnectorRun(ctx, domain.ConnectorRun{ID: id, Source: "github", Status: domain.ConnectorRunning, StartedAt: started}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := store.ListConnectorRuns(ctx, ConnectorRunFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "run-a" {
		t.Fatalf("tie order = %+v, want run-a first (PostgreSQL ORDER BY started_at DESC, id)", runs)
	}
}
