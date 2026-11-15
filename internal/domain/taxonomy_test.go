package domain

import (
	"strings"
	"testing"
)

func TestNormalizeTopicKey(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "empty", raw: "", want: ""},
		{name: "already normalized", raw: "architecture/auth-model", want: "architecture/auth-model"},
		{name: "uppercase and spaces", raw: " Architecture/Auth Model ", want: "architecture/auth-model"},
		{name: "underscores", raw: "architecture__auth_model", want: "architecture-auth-model"},
		{name: "collapses runs", raw: "a   b___c", want: "a-b-c"},
		{name: "keeps dots and colons", raw: "mcp.tool:brain_search", want: "mcp.tool:brain-search"},
		{name: "trims separators", raw: "--topic--", want: "topic"},
		{name: "invalid character", raw: "topic!", wantErr: true},
		{name: "only separators", raw: "---", wantErr: true},
		{name: "too long", raw: strings.Repeat("a", TopicKeyMaxRunes+1), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeTopicKey(test.raw)
			if test.wantErr {
				if err == nil {
					t.Fatalf("NormalizeTopicKey(%q) = %q, want error", test.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeTopicKey(%q): %v", test.raw, err)
			}
			if got != test.want {
				t.Fatalf("NormalizeTopicKey(%q) = %q, want %q", test.raw, got, test.want)
			}
			again, err := NormalizeTopicKey(got)
			if err != nil || again != got {
				t.Fatalf("normalization is not stable: %q -> %q (%v)", got, again, err)
			}
		})
	}
}

func TestTaxonomyValidation(t *testing.T) {
	valid := []Taxonomy{
		{},
		{Scope: ScopeProject},
		{Scope: ScopePersonal, Subtype: "discovery", TopicKey: "a/b", Occurrences: 2},
	}
	for _, taxonomy := range valid {
		if err := taxonomy.Validate("taxonomy", "id"); err != nil {
			t.Fatalf("valid taxonomy %+v rejected: %v", taxonomy, err)
		}
	}
	invalid := []Taxonomy{
		{Scope: RecordScope("team")},
		{Subtype: "two\nlines"},
		{Subtype: strings.Repeat("a", SubtypeMaxRunes+1)},
		{TopicKey: "Not Normalized"},
		{TopicKey: "bad!"},
		{Occurrences: -1},
	}
	for _, taxonomy := range invalid {
		if err := taxonomy.Validate("taxonomy", "id"); err == nil {
			t.Fatalf("invalid taxonomy %+v accepted", taxonomy)
		}
	}
}

func TestNormalizedTaxonomyDefaults(t *testing.T) {
	got := Taxonomy{}.Normalized()
	if got.Scope != ScopeProject {
		t.Fatalf("scope = %q, want project", got.Scope)
	}
	if got.NormalizedOccurrences() != 1 {
		t.Fatalf("occurrences = %d, want 1", got.NormalizedOccurrences())
	}
}

func TestPrependedHistoryIsBoundedAndNewestFirst(t *testing.T) {
	var history []HistoryEntry
	for i := 0; i < MaxTopicHistoryEntries+5; i++ {
		history = PrependedHistory(history, HistoryEntry{Body: "body"})
	}
	if len(history) != MaxTopicHistoryEntries {
		t.Fatalf("history length = %d, want %d", len(history), MaxTopicHistoryEntries)
	}
	history = PrependedHistory(history, HistoryEntry{Body: "newest"})
	if history[0].Body != "newest" {
		t.Fatalf("history[0] = %q, want newest first", history[0].Body)
	}
	if len(history) != MaxTopicHistoryEntries {
		t.Fatalf("history length after prepend = %d, want %d", len(history), MaxTopicHistoryEntries)
	}
	if got := PrependedHistory(history, HistoryEntry{Body: "  "}); len(got) != len(history) {
		t.Fatalf("empty entry changed history: %d -> %d", len(history), len(got))
	}
}
