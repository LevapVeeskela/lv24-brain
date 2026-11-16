package codeindex

import (
	"context"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// insertModule adds one synthetic module symbol the import cycle walk can use.
func insertModule(t *testing.T, fixture *fixtureIndex, id, name string) {
	t.Helper()
	if _, err := fixture.index.db.ExecContext(context.Background(),
		`INSERT INTO symbols(id, path, package, kind, name, qualname, start_line, end_line, signature, doc, exported)
		 VALUES(?, ?, 'synthetic', 'module', ?, ?, 1, 1, '', '', 0)`,
		id, name+".ts", name, name); err != nil {
		t.Fatal(err)
	}
}

// insertImport adds one imports edge.
func insertImport(t *testing.T, fixture *fixtureIndex, from, to string) {
	t.Helper()
	if _, err := fixture.index.db.ExecContext(context.Background(),
		`INSERT INTO edges(source, target, kind, line, confidence, provenance)
		 VALUES(?, ?, 'imports', 1, 1.0, 'types')`, from, to); err != nil {
		t.Fatal(err)
	}
}

func TestCyclesFindsImportCycles(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()
	// m0 -> m1 -> m2 -> m0 is one cycle; m3 -> m4 is a chain; m4 -> m4 is a
	// self-import and is reported separately.
	modules := []struct{ id, name string }{
		{"mod_cycle_0", "cycle0"}, {"mod_cycle_1", "cycle1"}, {"mod_cycle_2", "cycle2"},
		{"mod_chain_3", "chain3"}, {"mod_chain_4", "chain4"},
	}
	for _, module := range modules {
		insertModule(t, fixture, module.id, module.name)
	}
	insertImport(t, fixture, "mod_cycle_0", "mod_cycle_1")
	insertImport(t, fixture, "mod_cycle_1", "mod_cycle_2")
	insertImport(t, fixture, "mod_cycle_2", "mod_cycle_0")
	insertImport(t, fixture, "mod_cycle_2", "mod_chain_3")
	insertImport(t, fixture, "mod_chain_3", "mod_chain_4")
	insertImport(t, fixture, "mod_chain_4", "mod_chain_4")

	view, err := fixture.index.Cycles(ctx, "fixture", ports.CycleQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if view.Total != 1 || view.Nodes != 3 || view.SelfLoops != 1 {
		t.Fatalf("cycles = %+v", view)
	}
	if len(view.Cycles) != 1 || view.Cycles[0].Size != 3 || len(view.Cycles[0].Members) != 3 {
		t.Fatalf("cycle = %+v", view.Cycles)
	}
	names := make([]string, 0, 3)
	for _, member := range view.Cycles[0].Members {
		names = append(names, member.Name)
	}
	if strings.Join(names, ",") != "cycle0,cycle1,cycle2" {
		t.Fatalf("members = %v", names)
	}
	if !notesContain(view.Notes, "self-import") {
		t.Fatalf("notes = %v", view.Notes)
	}

	again, err := fixture.index.Cycles(ctx, "fixture", ports.CycleQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Cycles) != len(view.Cycles) || again.Cycles[0].Members[0].ID != view.Cycles[0].Members[0].ID {
		t.Fatalf("cycles are not deterministic: %+v / %+v", view.Cycles, again.Cycles)
	}
}

func TestCyclesCapsComponentsAndMembers(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()
	for cycle := 0; cycle < 3; cycle++ {
		first := "mod_cap_" + string(rune('a'+cycle)) + "0"
		second := "mod_cap_" + string(rune('a'+cycle)) + "1"
		insertModule(t, fixture, first, first)
		insertModule(t, fixture, second, second)
		insertImport(t, fixture, first, second)
		insertImport(t, fixture, second, first)
	}
	view, err := fixture.index.Cycles(ctx, "fixture", ports.CycleQuery{Limit: 1, MemberLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if view.Total != 3 || len(view.Cycles) != 1 || !view.Truncated {
		t.Fatalf("cycles = %+v", view)
	}
	if view.Cycles[0].Size != 2 || len(view.Cycles[0].Members) != 1 || !view.Cycles[0].MembersTruncated {
		t.Fatalf("cycle cap = %+v", view.Cycles[0])
	}
	if !notesContain(view.Notes, "showing the 1 largest") {
		t.Fatalf("notes = %v", view.Notes)
	}
}

func TestCyclesOnAnAcyclicGraph(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()
	insertModule(t, fixture, "mod_dag_a", "dagA")
	insertModule(t, fixture, "mod_dag_b", "dagB")
	insertImport(t, fixture, "mod_dag_a", "mod_dag_b")
	view, err := fixture.index.Cycles(ctx, "fixture", ports.CycleQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if view.Total != 0 || len(view.Cycles) != 0 || !notesContain(view.Notes, "acyclic") {
		t.Fatalf("cycles = %+v", view)
	}
}
