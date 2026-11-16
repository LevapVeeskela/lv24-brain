package graphalgo

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestStronglyConnectedComponentsFindsCycles(t *testing.T) {
	// 0 -> 1 -> 2 -> 0 is a cycle; 3 -> 4 -> 5 is a chain; 6 -> 6 is a
	// self-loop and not a cycle between nodes; 2 -> 3 joins them.
	edges := []Edge{
		{A: 0, B: 1}, {A: 1, B: 2}, {A: 2, B: 0},
		{A: 2, B: 3}, {A: 3, B: 4}, {A: 4, B: 5},
		{A: 5, B: 6}, {A: 6, B: 6},
	}
	components := StronglyConnectedComponents(7, edges)
	if len(components) != 1 {
		t.Fatalf("components = %+v", components)
	}
	if !reflect.DeepEqual(components[0].Members, []int{0, 1, 2}) {
		t.Fatalf("members = %v", components[0].Members)
	}
}

func TestStronglyConnectedComponentsIsDeterministic(t *testing.T) {
	// Two disjoint cycles and a chain between them.
	edges := []Edge{
		{A: 4, B: 5}, {A: 5, B: 6}, {A: 6, B: 4},
		{A: 0, B: 1}, {A: 1, B: 2}, {A: 2, B: 0},
		{A: 2, B: 4},
	}
	first := StronglyConnectedComponents(7, edges)
	if len(first) != 2 {
		t.Fatalf("components = %+v", first)
	}
	// Equal size: the component with the smallest member comes first.
	if !reflect.DeepEqual(first[0].Members, []int{0, 1, 2}) || !reflect.DeepEqual(first[1].Members, []int{4, 5, 6}) {
		t.Fatalf("order = %+v", first)
	}
	reversed := make([]Edge, len(edges))
	for index := range edges {
		reversed[len(edges)-1-index] = edges[index]
	}
	for run := 0; run < 20; run++ {
		if got := StronglyConnectedComponents(7, reversed); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d = %+v, want %+v", run, got, first)
		}
	}
}

func TestStronglyConnectedComponentsIgnoresDagAndInvalidEdges(t *testing.T) {
	edges := []Edge{{A: 0, B: 1}, {A: 1, B: 2}, {A: -1, B: 2}, {A: 0, B: 99}, {A: 3, B: 3}}
	if got := StronglyConnectedComponents(3, edges); len(got) != 0 {
		t.Fatalf("DAG produced cycles: %+v", got)
	}
	if got := StronglyConnectedComponents(0, edges); got != nil {
		t.Fatalf("empty graph = %+v", got)
	}
}

// stringReader yields a fixed key list, one key per Next call.
type stringReader struct {
	keys  []string
	index int
	errAt int
}

func (r *stringReader) Next() (string, error) {
	if r.errAt > 0 && r.index == r.errAt {
		return "", errors.New("fixture read failure")
	}
	if r.index >= len(r.keys) {
		return "", io.EOF
	}
	key := r.keys[r.index]
	r.index++
	return key, nil
}

func TestDiffKeysMergesOrderedStreams(t *testing.T) {
	head := &stringReader{keys: []string{"a", "c", "e"}}
	base := &stringReader{keys: []string{"b", "c", "d"}}
	diff, err := DiffKeys(head, base, 10)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Added != 2 || diff.Removed != 2 || diff.Truncated {
		t.Fatalf("diff = %+v", diff)
	}
	if !reflect.DeepEqual(diff.AddedKeys, []string{"a", "e"}) || !reflect.DeepEqual(diff.RemovedKeys, []string{"b", "d"}) {
		t.Fatalf("samples = %+v", diff)
	}
}

func TestDiffKeysCapsSamplesButCountsEverything(t *testing.T) {
	head := &stringReader{keys: []string{"a", "b", "c", "d"}}
	base := &stringReader{keys: []string{"w", "x", "y"}}
	diff, err := DiffKeys(head, base, 2)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Added != 4 || diff.Removed != 3 || !diff.Truncated {
		t.Fatalf("diff = %+v", diff)
	}
	if len(diff.AddedKeys) != 2 || len(diff.RemovedKeys) != 2 {
		t.Fatalf("samples = %+v", diff)
	}
	if diff.AddedKeys[0] != "a" || diff.RemovedKeys[0] != "w" {
		t.Fatalf("sample order = %+v", diff)
	}
}

func TestDiffKeysPropagatesAReadFailure(t *testing.T) {
	head := &stringReader{keys: []string{"a", "b"}, errAt: 1}
	base := &stringReader{keys: []string{"a"}}
	if _, err := DiffKeys(head, base, 5); err == nil || !strings.Contains(err.Error(), "fixture read failure") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiffKeysEmptySides(t *testing.T) {
	diff, err := DiffKeys(&stringReader{}, &stringReader{keys: []string{"a", "b"}}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Added != 0 || diff.Removed != 2 || len(diff.RemovedKeys) != 2 {
		t.Fatalf("diff = %+v", diff)
	}
	if fmt.Sprint(diff.AddedKeys) != "[]" {
		t.Fatalf("added samples = %v", diff.AddedKeys)
	}
}
