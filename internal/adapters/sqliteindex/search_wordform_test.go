package sqliteindex

import (
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestSearchEnglishPluralFindsSingular(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index,
		testDocument("singular", "brain", "one decision about storage"),
		testDocument("plural", "brain", "several decisions about storage"),
		testDocument("other", "brain", "unrelated text"),
	)

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "decisions"}), "singular", "plural")
	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "decision"}), "singular", "plural")
}

func TestSearchEnglishVerbForms(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index,
		testDocument("base", "brain", "deploy the server"),
		testDocument("past", "brain", "server deployed yesterday"),
		testDocument("policy", "brain", "retention policy"),
	)

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "deploying"}), "base", "past")
	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "policies"}), "policy")
}

func TestSearchRussianGenitivePluralFindsNominative(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index,
		testDocument("ru-1", "brain", "принято решение по хранилищу"),
		testDocument("ru-2", "brain", "журнал решений проекта"),
		testDocument("ru-3", "brain", "деплоем занимается CI"),
		testDocument("ru-4", "brain", "очистка браузера"),
	)

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "решений"}), "ru-1", "ru-2")
	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "решениям"}), "ru-1", "ru-2")
	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "деплой"}), "ru-3")
}

func TestSearchExactFormRanksAboveStemOnly(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index,
		testDocument("stem-only", "brain", "decision log"),
		testDocument("exact", "brain", "decisions log"),
	)

	hits := mustSearch(t, index, ports.Query{Text: "decisions"})
	if len(hits) != 2 || hits[0].ID != "exact" {
		t.Fatalf("hits = %v, want exact first", hits)
	}
}

func TestSearchSinceFiltersOlderDocuments(t *testing.T) {
	index := newTestIndex(t)
	old := testDocument("old", "brain", "alpha")
	fresh := testDocument("fresh", "brain", "alpha")
	fresh.UpdatedAt = baseTime.Add(48 * time.Hour)
	mustUpsert(t, index, old, fresh)

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "alpha", Since: baseTime.Add(time.Hour)}), "fresh")
	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "alpha", Since: baseTime}), "old", "fresh")
	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "alpha missing", Since: baseTime.Add(time.Hour)}), "fresh")
}
