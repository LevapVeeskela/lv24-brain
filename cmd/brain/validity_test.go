package main

import (
	"encoding/json"
	"strings"
	"testing"
)

type cliValidity struct {
	ValidFrom    *string `json:"valid_from"`
	ValidTo      *string `json:"valid_to"`
	SupersededBy string  `json:"superseded_by"`
	Supersedes   string  `json:"supersedes"`
}

type cliRecordSummary struct {
	ID       string      `json:"id"`
	Title    string      `json:"title"`
	Validity cliValidity `json:"validity"`
}

type cliSearchHits struct {
	Hits []struct {
		ID        string  `json:"id"`
		ValidFrom *string `json:"valid_from"`
		ValidTo   *string `json:"valid_to"`
	} `json:"hits"`
}

func createDecisionForTest(t *testing.T, cfgPath, title, contextText, supersedes string) string {
	t.Helper()
	args := []string{"decision", "--config", cfgPath, "--project", "demo", "--title", title, "--context", contextText}
	if supersedes != "" {
		args = append(args, "--supersedes", supersedes)
	}
	code, stdout, stderr := runForTest(t, args...)
	if code != 0 {
		t.Fatalf("decision %q: %s", title, stderr)
	}
	id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(stdout), "decision "))
	if id == "" || strings.ContainsAny(id, " \n") {
		t.Fatalf("decision id = %q (stdout %q)", id, stdout)
	}
	return id
}

func TestRecordCLIValidityAndHistory(t *testing.T) {
	cfgPath, _, _ := codexFixture(t)
	first := createDecisionForTest(t, cfgPath, "Storage alpha", "zebra cache decision", "")
	second := createDecisionForTest(t, cfgPath, "Storage beta", "zebra cache decision", first)

	code, stdout, stderr := runForTest(t, "record", "list", "demo", "--kind", "decision", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("record list: %s", stderr)
	}
	var current struct {
		Data []cliRecordSummary `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &current); err != nil {
		t.Fatalf("decode %s: %v", stdout, err)
	}
	if len(current.Data) != 1 || current.Data[0].ID != second {
		t.Fatalf("current records = %+v, want only %s", current.Data, second)
	}
	if current.Data[0].Validity.ValidTo != nil || current.Data[0].Validity.ValidFrom == nil {
		t.Fatalf("current validity = %+v", current.Data[0].Validity)
	}

	code, stdout, stderr = runForTest(t, "record", "list", "demo", "--kind", "decision", "--all-time", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("record list --all-time: %s", stderr)
	}
	var all struct {
		Data []cliRecordSummary `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &all); err != nil {
		t.Fatalf("decode %s: %v", stdout, err)
	}
	if len(all.Data) != 2 {
		t.Fatalf("all-time records = %+v", all.Data)
	}
	var old *cliRecordSummary
	for i := range all.Data {
		if all.Data[i].ID == first {
			old = &all.Data[i]
		}
	}
	if old == nil || old.Validity.ValidTo == nil || old.Validity.SupersededBy != second {
		t.Fatalf("superseded record = %+v", old)
	}

	code, stdout, stderr = runForTest(t, "record", "show", first, "--history", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("record show: %s", stderr)
	}
	var show struct {
		Data struct {
			Kind     string             `json:"kind"`
			Record   map[string]any     `json:"record"`
			Validity cliValidity        `json:"validity"`
			History  []cliRecordSummary `json:"history"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &show); err != nil {
		t.Fatalf("decode %s: %v", stdout, err)
	}
	if show.Data.Kind != "decision" || show.Data.Validity.ValidTo == nil || show.Data.Validity.SupersededBy != second {
		t.Fatalf("record show = %+v", show.Data)
	}
	if len(show.Data.History) != 2 || show.Data.History[0].ID != first || show.Data.History[1].ID != second {
		t.Fatalf("history = %+v", show.Data.History)
	}

	// --as-of marks whether the record was valid then; the past is outside
	// both records' windows.
	code, stdout, stderr = runForTest(t, "record", "show", first, "--as-of", "2020-01-01", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("record show --as-of: %s", stderr)
	}
	var asOf struct {
		Data struct {
			ValidAt *bool `json:"valid_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &asOf); err != nil {
		t.Fatalf("decode %s: %v", stdout, err)
	}
	if asOf.Data.ValidAt == nil || *asOf.Data.ValidAt {
		t.Fatalf("valid_at = %v, want false", asOf.Data.ValidAt)
	}

	code, stdout, _ = runForTest(t, "record", "supersede", first, "--by", second, "--config", cfgPath)
	if code != 0 || !strings.Contains(stdout, "already superseded") {
		t.Fatalf("idempotent supersede: code %d, output %q", code, stdout)
	}
}

func TestSearchCLIValidityFlags(t *testing.T) {
	cfgPath, _, _ := codexFixture(t)
	first := createDecisionForTest(t, cfgPath, "Storage alpha", "zebra cache decision", "")
	second := createDecisionForTest(t, cfgPath, "Storage beta", "zebra cache decision", first)

	searchIDs := func(t *testing.T, args ...string) []string {
		t.Helper()
		code, stdout, stderr := runForTest(t, append([]string{"search", "--config", cfgPath, "--project", "demo", "--json", "zebra"}, args...)...)
		if code != 0 {
			t.Fatalf("search %v: %s", args, stderr)
		}
		var result struct {
			Data cliSearchHits `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatalf("decode %s: %v", stdout, err)
		}
		ids := make([]string, 0, len(result.Data.Hits))
		for _, hit := range result.Data.Hits {
			ids = append(ids, hit.ID)
		}
		return ids
	}

	if ids := searchIDs(t); len(ids) != 1 || ids[0] != second {
		t.Fatalf("default search = %v, want only %s", ids, second)
	}
	if ids := searchIDs(t, "--all-time"); len(ids) != 2 {
		t.Fatalf("--all-time search = %v, want both generations", ids)
	}
	code, stdout, stderr := runForTest(t, "search", "--config", cfgPath, "--project", "demo", "--json", "--as-of", "2020-01-01", "zebra")
	if code != 0 {
		t.Fatalf("search --as-of: %s", stderr)
	}
	var historical struct {
		Data cliSearchHits `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &historical); err != nil {
		t.Fatalf("decode %s: %v", stdout, err)
	}
	if len(historical.Data.Hits) != 0 {
		t.Fatalf("as-of before both records = %+v, want none", historical.Data.Hits)
	}
	if code, _, stderr := runForTest(t, "search", "--config", cfgPath, "--as-of", "not-a-date", "zebra"); code != 2 {
		t.Fatalf("bad --as-of exit = %d (%s), want 2", code, stderr)
	}
}
