package mcpserver

import (
	"context"
	"errors"
	"testing"
)

// similarHits is a ranked page with a same-project original first, the record
// each append tool is about to create, a fourth candidate and a foreign-project
// record. The append tools must drop the new id, the foreign project and the
// overflow, leaving three.
func similarHits(newID string) []Hit {
	return []Hit{
		{ID: "note-alpha-7", ProjectID: "alpha", Kind: "note", Title: "Cache fix in SQLite", Score: 0.91},
		{ID: "decision-alpha-2", ProjectID: "alpha", Kind: "decision", Title: "Cache fix", Score: 0.72},
		{ID: newID, ProjectID: "alpha", Kind: "note", Title: "just written", Score: 0.66},
		{ID: "note-alpha-8", ProjectID: "alpha", Kind: "note", Title: "Cache", Score: 0.5},
		{ID: "note-alpha-9", ProjectID: "alpha", Kind: "note", Title: "Cache warmup", Score: 0.4},
		{ID: "note-beta-1", ProjectID: "beta", Kind: "note", Title: "Cache elsewhere", Score: 0.95},
	}
}

func TestAppendToolsReportSimilarRecords(t *testing.T) {
	cases := []struct {
		tool    string
		args    string
		newID   string
		wantTop string
	}{
		{"brain_note", `{"text":"Cache fix in SQLite","title":"Cache fix","project":"alpha"}`, "note-alpha-1", "note-alpha-7"},
		{"brain_decision", `{"title":"Cache fix","context":"slow","project":"alpha"}`, "decision-alpha-1", "note-alpha-7"},
		{"brain_attempt", `{"title":"Cache fix","result":"no gain","project":"alpha"}`, "attempt-alpha-1", "note-alpha-7"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			backend := &fullFake{hits: similarHits(tc.newID)}
			result := toolCall(t, backend, tc.tool, tc.args)
			if result["isError"] != false {
				t.Fatalf("isError = %v", result["isError"])
			}
			obj := structured(t, result)
			if obj["id"] != tc.newID {
				t.Fatalf("id = %v, want %s", obj["id"], tc.newID)
			}
			similar, ok := obj["similar"].([]any)
			if !ok {
				t.Fatalf("similar = %v, want an array", obj["similar"])
			}
			if len(similar) != similarLimit {
				t.Fatalf("similar = %d records, want %d: %v", len(similar), similarLimit, similar)
			}
			got := similar[0].(map[string]any)
			if got["id"] != tc.wantTop {
				t.Fatalf("top similar = %v, want %s", got["id"], tc.wantTop)
			}
			for _, raw := range similar {
				entry := raw.(map[string]any)
				if entry["id"] == tc.newID {
					t.Fatalf("new record %s reported as similar: %v", tc.newID, similar)
				}
				if entry["kind"] == "" || entry["title"] == "" {
					t.Fatalf("incomplete similar entry: %v", entry)
				}
				if score, _ := entry["score"].(float64); score <= 0 {
					t.Fatalf("score = %v, want > 0", entry["score"])
				}
			}
			if backend.search.ProjectID != "alpha" || backend.search.Limit != similarSearchLimit {
				t.Fatalf("search = %+v, want alpha with limit %d", backend.search, similarSearchLimit)
			}
		})
	}
}

// searchErrorFake fails every search after the write.
type searchErrorFake struct{ *fullFake }

func (searchErrorFake) Search(context.Context, SearchInput) ([]Hit, error) {
	return nil, errors.New("index is rebuilding")
}

func TestSimilarSearchErrorDoesNotFailTheWrite(t *testing.T) {
	backend := &fullFake{}
	result := toolCall(t, searchErrorFake{backend}, "brain_note", `{"text":"Cache fix","title":"Cache fix","project":"alpha"}`)
	if result["isError"] != false {
		t.Fatalf("isError = %v, want false", result["isError"])
	}
	obj := structured(t, result)
	if obj["id"] != "note-alpha-1" {
		t.Fatalf("id = %v, want note-alpha-1", obj["id"])
	}
	similar, ok := obj["similar"].([]any)
	if !ok {
		t.Fatalf("similar = %v, want an array", obj["similar"])
	}
	if len(similar) != 0 {
		t.Fatalf("similar = %v, want empty on search failure", similar)
	}
	if len(backend.notes) != 1 {
		t.Fatalf("notes = %+v, want the write to land", backend.notes)
	}
}
