package sqliteindex

import (
	"context"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func resultIDs(hits []ports.Hit) map[string]bool {
	ids := make(map[string]bool, len(hits))
	for _, hit := range hits {
		ids[hit.ID] = true
	}
	return ids
}

func assertResultIDs(t *testing.T, hits []ports.Hit, want ...string) {
	t.Helper()
	got := resultIDs(hits)
	if len(got) != len(want) {
		t.Fatalf("hits = %v, want %v", got, want)
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("hits = %v, want %v", got, want)
		}
	}
}

func TestSearchRussianPrefixInflection(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index,
		testDocument("ru-1", "brain", "настройка кэширования запросов"),
		testDocument("ru-2", "brain", "кэшированием управляет сервис"),
		testDocument("ru-3", "brain", "очистка браузера"),
	)

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "кэширован"}), "ru-1", "ru-2")
}

func TestSearchShortRussianTermStaysExact(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index, testDocument("ru-1", "brain", "настройка кэширования"))

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "кэш"}))
}

func TestSearchEnglishPrefix(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index,
		testDocument("en-1", "brain", "caching layer design"),
		testDocument("en-2", "brain", "cache eviction policy"),
		testDocument("en-3", "brain", "unrelated text"),
	)

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "cach"}), "en-1", "en-2")
	// "caching" is stemmed to the prefix "cach", so it also finds "cache".
	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "caching"}), "en-1", "en-2")
}

func TestSearchShortTermIsNotPrefix(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index, testDocument("doc-1", "brain", "dbx tuning notes"))

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "db"}))
	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "dbx"}), "doc-1")
}

func TestSearchOrFallbackOnlyWhenAndIsEmpty(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index,
		testDocument("both", "brain", "alpha beta"),
		testDocument("alpha-only", "brain", "alpha gamma"),
	)

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "alpha beta"}), "both")
	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "alpha delta"}), "alpha-only", "both")
}

func TestSearchOrFallbackNotUsedForSingleTerm(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index, testDocument("doc-1", "brain", "alpha"))

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "missing"}))
}

func TestSearchOrFallbackRespectsFilters(t *testing.T) {
	index := newTestIndex(t)
	note := testDocument("note-brain", "brain", "alpha")
	decision := testDocument("decision-brain", "brain", "beta")
	decision.Kind = "decision"
	other := testDocument("note-other", "other", "beta")
	mustUpsert(t, index, note, decision, other)

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "alpha beta", ProjectID: "brain"}),
		"note-brain", "decision-brain")
	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "alpha beta", ProjectID: "brain", Kinds: []string{"note"}}),
		"note-brain")
	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "alpha beta", ProjectID: "brain", ExcludeKinds: []string{"note"}}),
		"decision-brain")
}

func TestSearchInjectionInputsStayText(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index, testDocument("doc-1", "brain", "alpha beta"))

	inputs := []string{
		`"alpha"`,
		`alpha" OR "beta`,
		`alpha*`,
		`alpha NEAR beta`,
		`NEAR(alpha beta, 1)`,
		`alpha AND`,
		`NOT alpha`,
		`(alpha) OR (beta`,
		`title:alpha`,
		`body:alpha*`,
		`alpha ^ beta +`,
		`{alpha beta}`,
	}
	for _, input := range inputs {
		if _, err := index.Search(context.Background(), ports.Query{Text: input}); err != nil {
			t.Fatalf("Search(%q) error = %v, want none", input, err)
		}
	}
}

func TestSearchColumnFilterIsNotInterpreted(t *testing.T) {
	index := newTestIndex(t)
	doc := testDocument("doc-1", "brain", "alpha")
	doc.Title = "Deployment runbook"
	mustUpsert(t, index, doc)

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "title:alpha"}))
}

func TestSearchNotIsNotAnOperator(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index, testDocument("doc-1", "brain", "alpha"))

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "NOT alpha"}), "doc-1")
}
