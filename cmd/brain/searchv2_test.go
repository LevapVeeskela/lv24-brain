package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// createNoteForTest appends one note to the demo project.
func createNoteForTest(t *testing.T, cfgPath, title, body string) {
	t.Helper()
	if code, _, stderr := runForTest(t, "note", "--config", cfgPath, "--project", "demo", "--title", title, "--body", body); code != 0 {
		t.Fatalf("note %q: %s", title, stderr)
	}
}

// --project uses the shared selector: an id, an alias, an existing checkout
// path or a directory under one. An unknown value is exit 2 with a clear
// error, never a successful "0 hits" that looks like an empty index.
func TestSearchProjectSelectorResolvesAliasAndPath(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runForTest(t, "project", "set", "demo",
		"--path", checkout, "--alias", "d", "--config", cfgPath); code != 0 {
		t.Fatalf("project set: %s", stderr)
	}
	createNoteForTest(t, cfgPath, "Selector note", "selector needle")
	if code, _, stderr := runForTest(t, "index", "--config", cfgPath); code != 0 {
		t.Fatalf("index: %s", stderr)
	}

	for _, selector := range []string{"DEMO", "d", checkout, filepath.Join(checkout, "sub")} {
		code, stdout, stderr := runForTest(t, "search", "--config", cfgPath, "--project", selector, "needle")
		if code != 0 || !strings.Contains(stdout, "note/demo/") {
			t.Fatalf("search --project %q: exit %d, stdout %q, stderr %q", selector, code, stdout, stderr)
		}
	}

	code, stdout, stderr := runForTest(t, "search", "--config", cfgPath, "--project", "lv24-brian", "needle")
	if code != 2 || !strings.Contains(stderr, "not a registered project") {
		t.Fatalf("unknown project: exit %d, stdout %q, stderr %q; want exit 2 and a clear error", code, stdout, stderr)
	}
}

// searchPageJSON is the decoded `brain search --json` payload.
type searchPageJSON struct {
	Relaxed    bool   `json:"relaxed"`
	Page       int    `json:"page"`
	PageSize   int    `json:"page_size"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
	Hits       []struct {
		ID           string         `json:"id"`
		Snippet      string         `json:"snippet"`
		Score        float64        `json:"score"`
		MatchReasons []string       `json:"match_reasons"`
		Explain      map[string]any `json:"explain"`
		Body         string         `json:"body"`
	} `json:"hits"`
}

// searchJSON runs a search and decodes its data object.
func searchJSON(t *testing.T, args ...string) searchPageJSON {
	t.Helper()
	_, data := runJSONForTest(t, append([]string{"search", "--json"}, args...)...)
	page := searchPageJSON{
		Relaxed:    data["relaxed"] == true,
		Page:       int(data["page"].(float64)),
		PageSize:   int(data["page_size"].(float64)),
		HasMore:    data["has_more"] == true,
		NextCursor: stringValue(data["next_cursor"]),
	}
	for _, raw := range asArray(t, "hits", data["hits"]) {
		hit := asObject(t, "hit", raw)
		entry := struct {
			ID           string         `json:"id"`
			Snippet      string         `json:"snippet"`
			Score        float64        `json:"score"`
			MatchReasons []string       `json:"match_reasons"`
			Explain      map[string]any `json:"explain"`
			Body         string         `json:"body"`
		}{
			ID:      stringValue(hit["id"]),
			Snippet: stringValue(hit["snippet"]),
			Body:    stringValue(hit["body"]),
		}
		if score, ok := hit["score"].(float64); ok {
			entry.Score = score
		}
		for _, reason := range asArray(t, "match_reasons", hit["match_reasons"]) {
			entry.MatchReasons = append(entry.MatchReasons, stringValue(reason))
		}
		if explain, ok := hit["explain"].(map[string]any); ok {
			entry.Explain = explain
		}
		page.Hits = append(page.Hits, entry)
	}
	return page
}

// stringValue reads an optional string field.
func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// Three pages walk the whole result set without duplicates or omissions.
func TestSearchCLIPaginationThreePages(t *testing.T) {
	cfgPath, _, _ := codexFixture(t)
	for index := 0; index < 7; index++ {
		createNoteForTest(t, cfgPath, "Paging note "+string(rune('A'+index)), "paginationmarker body")
	}

	var seen []string
	cursor := ""
	for page := 1; page <= 3; page++ {
		args := []string{"paginationmarker", "--config", cfgPath, "--page-size", "3"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		result := searchJSON(t, args...)
		if result.Page != page || result.PageSize != 3 {
			t.Fatalf("page %d: page=%d size=%d", page, result.Page, result.PageSize)
		}
		for _, hit := range result.Hits {
			seen = append(seen, hit.ID)
		}
		switch page {
		case 1, 2:
			if !result.HasMore || result.NextCursor == "" {
				t.Fatalf("page %d: has_more=%v cursor=%q", page, result.HasMore, result.NextCursor)
			}
			cursor = result.NextCursor
		case 3:
			if result.HasMore || result.NextCursor != "" {
				t.Fatalf("page 3: has_more=%v cursor=%q", result.HasMore, result.NextCursor)
			}
		}
	}
	if len(seen) != 7 {
		t.Fatalf("paged ids = %v, want 7", seen)
	}
	unique := map[string]bool{}
	for _, id := range seen {
		if unique[id] {
			t.Fatalf("duplicate id %s across pages: %v", id, seen)
		}
		unique[id] = true
	}

	// A cursor is bound to the query it was issued for.
	first := searchJSON(t, "paginationmarker", "--config", cfgPath, "--page-size", "2")
	code, _, stderr := runForTest(t, "search", "anothermarker", "--config", cfgPath, "--cursor", first.NextCursor)
	if code != 2 || !strings.Contains(stderr, "another query") {
		t.Fatalf("replayed cursor: exit %d, stderr %q", code, stderr)
	}
}

// The search v2 flags change the query, the ranking output and nothing else.
func TestSearchCLIRankingFlags(t *testing.T) {
	cfgPath, _, _ := codexFixture(t)
	createNoteForTest(t, cfgPath, "Alpha beta", "alpha beta marker")
	createNoteForTest(t, cfgPath, "Alpha only", "alpha marker")

	relaxed := searchJSON(t, "alpha gammamarker", "--config", cfgPath, "--match-mode", "all", "--relaxed")
	if !relaxed.Relaxed || len(relaxed.Hits) != 2 {
		t.Fatalf("--relaxed: relaxed=%v hits=%d, want true/2", relaxed.Relaxed, len(relaxed.Hits))
	}
	strict := searchJSON(t, "alpha gammamarker", "--config", cfgPath, "--match-mode", "all")
	if strict.Relaxed || len(strict.Hits) != 0 {
		t.Fatalf("--match-mode all: relaxed=%v hits=%d, want false/0", strict.Relaxed, len(strict.Hits))
	}
	anyMode := searchJSON(t, "alpha gammamarker", "--config", cfgPath, "--match-mode", "any")
	if anyMode.Relaxed || len(anyMode.Hits) != 2 {
		t.Fatalf("--match-mode any: relaxed=%v hits=%d, want false/2", anyMode.Relaxed, len(anyMode.Hits))
	}

	explained := searchJSON(t, "alpha", "--config", cfgPath, "--explain")
	if len(explained.Hits) == 0 {
		t.Fatal("--explain returned no hits")
	}
	hit := explained.Hits[0]
	if hit.Score <= 0 || hit.Score > 1 {
		t.Fatalf("score = %v, want (0, 1]", hit.Score)
	}
	if len(hit.MatchReasons) == 0 {
		t.Fatal("--explain hit has no match_reasons")
	}
	for _, signal := range []string{"bm25", "recency", "importance", "pin", "link"} {
		if _, ok := hit.Explain[signal]; !ok {
			t.Fatalf("--explain payload has no %s contribution: %v", signal, hit.Explain)
		}
	}
	// Every contribution keeps its key and gains a share of the score; the
	// shares add up to 1.
	shareSum := 0.0
	for _, signal := range []string{"bm25", "recency", "importance", "pin", "link"} {
		share, ok := hit.Explain[signal+"_share"].(float64)
		if !ok {
			t.Fatalf("--explain payload has no %s_share: %v", signal, hit.Explain)
		}
		shareSum += share
	}
	if math.Abs(shareSum-1) > 1e-9 {
		t.Fatalf("--explain shares = %.9f, want 1: %v", shareSum, hit.Explain)
	}

	// The default mode relaxes to OR when strict AND returns fewer than K hits,
	// so a partial match is returned below the both-term record.
	full := searchJSON(t, "alpha beta", "--config", cfgPath, "--full")
	if len(full.Hits) != 2 || !strings.Contains(full.Hits[0].Body, "alpha beta marker") {
		t.Fatalf("--full hits = %+v", full.Hits)
	}

	dropped := searchJSON(t, "alpha", "--config", cfgPath, "--min-score", "0.99")
	if len(dropped.Hits) != 0 {
		t.Fatalf("--min-score 0.99 hits = %d, want 0", len(dropped.Hits))
	}

	for _, bad := range [][]string{
		{"alpha", "--match-mode", "sometimes"},
		{"alpha", "--min-score", "2"},
		{"alpha", "--page-size", "201"},
	} {
		args := append([]string{"search", "--config", cfgPath}, bad...)
		if code, _, stderr := runForTest(t, args...); code != 2 {
			t.Fatalf("search %v: exit %d (stderr %q), want 2", bad, code, stderr)
		}
	}
}

// The pre-0.16 flags and the text layout keep working.
func TestSearchCLIOldFlagsStillWork(t *testing.T) {
	cfgPath, _, _ := codexFixture(t)
	createNoteForTest(t, cfgPath, "Legacy note", "legacymarker body")

	code, stdout, stderr := runForTest(t, "search", "--config", cfgPath, "--project", "demo", "--limit", "1", "legacymarker")
	if code != 0 {
		t.Fatalf("search exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.HasPrefix(stdout, "note/demo/") || !strings.Contains(stdout, "legacymarker") {
		t.Fatalf("search stdout = %q", stdout)
	}
	code, stdout, stderr = runForTest(t, "search", "--config", cfgPath, "--project", "demo", "--explain", "legacymarker")
	if code != 0 || !strings.Contains(stdout, "contribution") || !strings.Contains(stdout, "share") {
		t.Fatalf("search --explain text: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	limited := searchJSON(t, "legacymarker", "--config", cfgPath, "--limit", "1", "--kind", "note", "--since", "7d")
	if len(limited.Hits) != 1 {
		t.Fatalf("old flags hits = %d, want 1", len(limited.Hits))
	}
	if !strings.Contains(strings.Join(limited.Hits[0].MatchReasons, ","), "bm25") {
		t.Fatalf("match_reasons = %v, want bm25", limited.Hits[0].MatchReasons)
	}
}

// get prints the record body under its token budget, and the JSON reports the
// truncation.
func TestGetCLIMaxTokens(t *testing.T) {
	cfgPath, _, _ := codexFixture(t)
	context := strings.TrimSpace(strings.Repeat("the cache keeps entries ", 60))
	if code, _, stderr := runForTest(t, "decision", "--config", cfgPath, "--project", "demo",
		"--title", "Long decision", "--context", context); code != 0 {
		t.Fatalf("decision: %s", stderr)
	}
	id := listRecordIDs(t, cfgPath, "decision")[0]

	_, data := runJSONForTest(t, "get", id, "--config", cfgPath, "--json", "--max-tokens", "12")
	if data["truncated"] != true {
		t.Fatalf("truncated = %v, want true", data["truncated"])
	}
	if body := stringValue(data["body"]); !strings.Contains(body, "truncated") {
		t.Fatalf("body = %q, want a truncation marker", body)
	}
	if tokens, _ := data["tokens"].(float64); tokens > 12 {
		t.Fatalf("tokens = %v, want <= 12", tokens)
	}

	_, whole := runJSONForTest(t, "get", id, "--config", cfgPath, "--json", "--max-tokens", "0")
	if whole["truncated"] != false || !strings.Contains(stringValue(whole["body"]), "Context:") {
		t.Fatalf("unlimited get = %v", whole)
	}

	code, stdout, stderr := runForTest(t, "get", id, "--config", cfgPath, "--max-tokens", "12")
	if code != 0 || !strings.Contains(stdout, "truncated: true") {
		t.Fatalf("get text: exit %d, stdout %q (stderr %q)", code, stdout, stderr)
	}
	if code, _, _ := runForTest(t, "get", "--config", cfgPath); code != 2 {
		t.Fatalf("get without an id: exit %d, want 2", code)
	}
}

// timeline prints the window around one record and marks the anchor.
func TestTimelineCLIWindow(t *testing.T) {
	cfgPath, _, _ := codexFixture(t)
	for index := 0; index < 3; index++ {
		createNoteForTest(t, cfgPath, "Timeline "+string(rune('A'+index)), "timeline marker")
	}
	ids := listRecordIDs(t, cfgPath, "note")
	if len(ids) < 3 {
		t.Fatalf("notes = %v, want at least 3", ids)
	}
	anchor := ids[1]

	_, data := runJSONForTest(t, "timeline", anchor, "--config", cfgPath, "--json", "--window", "1")
	records := asArray(t, "records", data["records"])
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}
	if index, _ := data["anchor_index"].(float64); index != 1 {
		t.Fatalf("anchor_index = %v, want 1", data["anchor_index"])
	}
	if asObject(t, "anchor", data["anchor"])["id"] != anchor {
		t.Fatalf("anchor = %v, want %s", data["anchor"], anchor)
	}

	code, stdout, stderr := runForTest(t, "timeline", anchor, "--config", cfgPath, "--window", "1")
	if code != 0 {
		t.Fatalf("timeline exit %d (stderr %s)", code, stderr)
	}
	if marked := countLinesWithPrefix(stdout, ">"); marked != 1 {
		t.Fatalf("timeline stdout = %q, want exactly one > marker", stdout)
	}
	if !strings.Contains(stdout, anchor) {
		t.Fatalf("timeline stdout = %q, want the anchor id", stdout)
	}
}

// listRecordIDs returns the ids of one kind, newest first.
func listRecordIDs(t *testing.T, cfgPath, kind string) []string {
	t.Helper()
	data := runJSONData(t, "record", "list", "--config", cfgPath, "--project", "demo", "--kind", kind, "--json")
	records, ok := data.([]any)
	if !ok {
		t.Fatalf("record list data is %T, want array", data)
	}
	ids := make([]string, 0, len(records))
	for _, raw := range records {
		ids = append(ids, stringValue(asObject(t, "record", raw)["id"]))
	}
	return ids
}

// runJSONData runs a command whose JSON envelope carries an array payload.
func runJSONData(t *testing.T, args ...string) any {
	t.Helper()
	code, stdout, stderr := runForTest(t, args...)
	if code != 0 {
		t.Fatalf("%v exit code = %d (stderr: %s)", args, code, stderr)
	}
	var doc struct {
		Data any `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("%v stdout is not JSON: %v\n%s", args, err, stdout)
	}
	return doc.Data
}

// countLinesWithPrefix counts the lines that start with prefix.
func countLinesWithPrefix(text, prefix string) int {
	count := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

// A preview in the text output stays inside the budget as well.
func TestSearchCLIPreviewLength(t *testing.T) {
	cfgPath, _, _ := codexFixture(t)
	createNoteForTest(t, cfgPath, "Preview note", strings.TrimSpace(strings.Repeat("previewmarker ", 40)))
	page := searchJSON(t, "previewmarker", "--config", cfgPath)
	if len(page.Hits) == 0 {
		t.Fatal("no hits")
	}
	if got := utf8.RuneCountInString(page.Hits[0].Snippet); got > 160 {
		t.Fatalf("preview length = %d, want <= 160", got)
	}
}
