package graphalgo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// twoCliques builds two five-node cliques joined by one bridge edge:
// 0..4 and 5..9, plus bridge 4-5.
func twoCliques() (int, []Edge) {
	const size = 5
	edges := make([]Edge, 0, 2*size*(size-1)/2+1)
	for left := 0; left < size; left++ {
		for right := left + 1; right < size; right++ {
			edges = append(edges, Edge{A: left, B: right})
		}
	}
	for left := size; left < 2*size; left++ {
		for right := left + 1; right < 2*size; right++ {
			edges = append(edges, Edge{A: left, B: right})
		}
	}
	edges = append(edges, Edge{A: size - 1, B: size})
	return 2 * size, edges
}

func TestDetectCommunitiesFindsTwoDenseGroups(t *testing.T) {
	nodes, edges := twoCliques()
	partition := DetectCommunities(nodes, edges, CommunityOptions{})
	if len(partition.Communities) != 2 {
		t.Fatalf("communities = %d: %+v", len(partition.Communities), partition.Communities)
	}
	first, second := partition.Communities[0], partition.Communities[1]
	if len(first.Members) != 5 || len(second.Members) != 5 {
		t.Fatalf("sizes = %v / %v", first.Members, second.Members)
	}
	if !sameSet(first.Members, []int{0, 1, 2, 3, 4}) {
		t.Fatalf("first = %v", first.Members)
	}
	if !sameSet(second.Members, []int{5, 6, 7, 8, 9}) {
		t.Fatalf("second = %v", second.Members)
	}
	if first.Cohesion != 1 || second.Cohesion != 1 {
		t.Fatalf("cohesion = %v / %v", first.Cohesion, second.Cohesion)
	}
	if partition.Assignment[0] != partition.Assignment[1] || partition.Assignment[0] == partition.Assignment[5] {
		t.Fatalf("assignment = %v", partition.Assignment)
	}
}

func TestDetectCommunitiesIsDeterministic(t *testing.T) {
	nodes, edges := twoCliques()
	first := DetectCommunities(nodes, edges, CommunityOptions{})
	for run := 0; run < 20; run++ {
		next := DetectCommunities(nodes, edges, CommunityOptions{})
		if fmt.Sprint(first.Assignment) != fmt.Sprint(next.Assignment) {
			t.Fatalf("run %d assignment = %v, want %v", run, next.Assignment, first.Assignment)
		}
	}
	// The result must not depend on edge order.
	reversed := make([]Edge, len(edges))
	for index := range edges {
		reversed[len(edges)-1-index] = edges[index]
	}
	other := DetectCommunities(nodes, reversed, CommunityOptions{})
	if fmt.Sprint(first.Assignment) != fmt.Sprint(other.Assignment) {
		t.Fatalf("reversed edges = %v, want %v", other.Assignment, first.Assignment)
	}
}

func TestDetectCommunitiesSingletonAndEmptyGraph(t *testing.T) {
	partition := DetectCommunities(3, nil, CommunityOptions{})
	if len(partition.Communities) != 3 {
		t.Fatalf("empty graph = %+v", partition.Communities)
	}
	for _, community := range partition.Communities {
		if len(community.Members) != 1 || community.Cohesion != 0 {
			t.Fatalf("singleton = %+v", community)
		}
	}
	if len(DetectCommunities(0, nil, CommunityOptions{}).Communities) != 0 {
		t.Fatal("zero nodes produced a community")
	}
}

func sameSet(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	seen := map[int]bool{}
	for _, value := range left {
		seen[value] = true
	}
	for _, value := range right {
		if !seen[value] {
			return false
		}
	}
	return true
}

func TestShortestPathPrefersTheShortestChain(t *testing.T) {
	graph := map[string][]Neighbor{
		"a": {{ID: "b", Edge: "calls", Confidence: 1}, {ID: "d", Edge: "calls", Confidence: 0.5}},
		"b": {{ID: "c", Edge: "calls", Confidence: 0.9}},
		"d": {{ID: "c", Edge: "references", Confidence: 0.4}},
		"c": nil,
	}
	expand := func(frontier []string) (map[string][]Neighbor, error) {
		out := map[string][]Neighbor{}
		for _, id := range frontier {
			out[id] = graph[id]
		}
		return out, nil
	}
	steps, found, err := ShortestPath("a", "c", 4, expand)
	if err != nil || !found {
		t.Fatalf("path = %v, %v", found, err)
	}
	if len(steps) != 3 || steps[0].ID != "a" || steps[1].ID != "b" || steps[2].ID != "c" {
		t.Fatalf("steps = %+v", steps)
	}
	if steps[2].Edge != "calls" || steps[2].Confidence != 0.9 {
		t.Fatalf("last hop = %+v", steps[2])
	}
}

func TestShortestPathRespectsDepthAndMissingGoal(t *testing.T) {
	graph := map[string][]Neighbor{
		"a": {{ID: "b"}},
		"b": {{ID: "c"}},
		"c": nil,
	}
	expand := func(frontier []string) (map[string][]Neighbor, error) {
		out := map[string][]Neighbor{}
		for _, id := range frontier {
			out[id] = graph[id]
		}
		return out, nil
	}
	if _, found, _ := ShortestPath("a", "c", 1, expand); found {
		t.Fatal("a path longer than maxDepth was found")
	}
	if _, found, _ := ShortestPath("a", "z", 8, expand); found {
		t.Fatal("a missing goal was found")
	}
	steps, found, _ := ShortestPath("a", "a", 0, expand)
	if !found || len(steps) != 1 {
		t.Fatalf("same node = %+v", steps)
	}
}

func TestShortestPathIsDeterministicAcrossNeighbourOrder(t *testing.T) {
	graph := map[string][]Neighbor{
		"a": {{ID: "c"}, {ID: "b"}},
		"b": {{ID: "d"}},
		"c": {{ID: "d"}},
		"d": nil,
	}
	expand := func(frontier []string) (map[string][]Neighbor, error) {
		out := map[string][]Neighbor{}
		for _, id := range frontier {
			hops := append([]Neighbor(nil), graph[id]...)
			if len(hops) == 2 {
				hops[0], hops[1] = hops[1], hops[0]
			}
			out[id] = hops
		}
		return out, nil
	}
	for run := 0; run < 10; run++ {
		steps, found, err := ShortestPath("a", "d", 4, expand)
		if err != nil || !found {
			t.Fatalf("path = %v, %v", found, err)
		}
		if steps[1].ID != "b" {
			t.Fatalf("run %d picked %q, want the lexicographically first hop", run, steps[1].ID)
		}
	}
}

func TestValidateRefRefusesOptionsAndRanges(t *testing.T) {
	for _, ref := range []string{"-x", "HEAD~1..HEAD", "HEAD:path", "a b", "", strings.Repeat("a", 300)} {
		if err := ValidateRef("base", ref); err == nil {
			t.Fatalf("%q was accepted", ref)
		}
	}
	for _, ref := range []string{"HEAD", "HEAD~3", "origin/main", "v0.18.0", "0123456789abcdef"} {
		if err := ValidateRef("base", ref); err != nil {
			t.Fatalf("%q refused: %v", ref, err)
		}
	}
}

func TestChangedFilesReadsATempRepository(t *testing.T) {
	root := initRepository(t)
	writeFile(t, root, "a.txt", "one\n")
	writeFile(t, root, "b.txt", "keep\n")
	commit(t, root, "base")
	base := gitOutput(t, root, "rev-parse", "HEAD")

	writeFile(t, root, "a.txt", "two\n")
	writeFile(t, root, "c.txt", "new\n")
	commit(t, root, "head")
	head := gitOutput(t, root, "rev-parse", "HEAD")

	files, total, truncated, err := ChangedFiles(context.Background(), root, base, head, 0)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || total != 2 || len(files) != 2 {
		t.Fatalf("files = %+v total=%d truncated=%v", files, total, truncated)
	}
	if files[0] != (ChangedFile{Path: "a.txt", Status: "M"}) || files[1] != (ChangedFile{Path: "c.txt", Status: "A"}) {
		t.Fatalf("files = %+v", files)
	}

	limited, limitedTotal, truncated, err := ChangedFiles(context.Background(), root, base, head, 1)
	if err != nil || !truncated || len(limited) != 1 || limitedTotal != 2 {
		t.Fatalf("limited = %+v total=%d truncated=%v err=%v", limited, limitedTotal, truncated, err)
	}
	if _, _, _, err := ChangedFiles(context.Background(), root, "does-not-exist", head, 0); err == nil {
		t.Fatal("an unknown ref was accepted")
	}
}

func TestChangedFilesReportsDeletes(t *testing.T) {
	root := initRepository(t)
	writeFile(t, root, "gone.txt", "x\n")
	commit(t, root, "base")
	base := gitOutput(t, root, "rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	commit(t, root, "head")
	head := gitOutput(t, root, "rev-parse", "HEAD")
	files, _, _, err := ChangedFiles(context.Background(), root, base, head, 0)
	if err != nil || len(files) != 1 || files[0].Status != "D" {
		t.Fatalf("files = %+v err=%v", files, err)
	}
}

func initRepository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	return root
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, root, message string) {
	t.Helper()
	git(t, root, "add", "-A")
	git(t, root, "-c", "user.email=fixture@example.com", "-c", "user.name=Fixture",
		"commit", "-q", "-m", message)
}

func git(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}
