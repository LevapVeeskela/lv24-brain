package app

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

var dupBase = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

func dupAt(minutes int) time.Time { return dupBase.Add(time.Duration(minutes) * time.Minute) }

func TestNormalizeDupTextStripsMarkdownAndFoldsYo(t *testing.T) {
	// Arrange
	raw := "## Ещё **раз**: `brain`  review —\n\t[ссылка](x) ЁЛКА!"

	// Act
	got := normalizeDupText(raw)

	// Assert
	if want := "еще раз brain review ссылка x елка"; got != want {
		t.Fatalf("normalizeDupText = %q, want %q", got, want)
	}
}

func TestDupSimilarityEnglishPunctuationNoise(t *testing.T) {
	// Arrange
	a := newDupRecord("a", dupAt(0), "Search index", "The search index is rebuilt on every sync and the old index is removed afterwards.", false)
	b := newDupRecord("b", dupAt(1), "**Search index**", "- The search index is *rebuilt* on every sync; and the old index is removed, afterwards!", false)

	// Act
	score, ok := dupSimilarity(a, b)

	// Assert
	if !ok || score != 1 {
		t.Fatalf("score = %v ok = %v, want identical after normalization", score, ok)
	}
}

func TestDupSimilarityRussianNearCopy(t *testing.T) {
	// Arrange
	body := "Индекс поиска пересобирается при каждой синхронизации данных, старый индекс удаляется после успешной сборки, а ошибки пишутся в журнал"
	a := newDupRecord("a", dupAt(0), "Пересборка индекса", body, false)
	b := newDupRecord("b", dupAt(1), "Пересборка индекса", body+" проекта", false)

	// Act
	score, ok := dupSimilarity(a, b)

	// Assert
	if !ok || score < duplicateThreshold {
		t.Fatalf("score = %v ok = %v, want a near-duplicate", score, ok)
	}
}

func TestDupSimilarityDifferentTextIsNotDuplicate(t *testing.T) {
	// Arrange
	a := newDupRecord("a", dupAt(0), "Release", "Publish the npm package through trusted publishing with OIDC tokens", false)
	b := newDupRecord("b", dupAt(1), "Index", "Rebuild the sqlite search index when the schema version changes on disk", false)

	// Act
	_, ok := dupSimilarity(a, b)

	// Assert
	if ok {
		t.Fatal("unrelated records reported as duplicates")
	}
}

func TestDupSimilarityThresholdBoundary(t *testing.T) {
	// Arrange: 20 distinct words give 18 shingles; changing the last word of
	// b keeps 17 shared of 19 in the union (0.89), changing the last three
	// words keeps 15 of 21 (0.71).
	words := make([]string, 20)
	for i := range words {
		words[i] = fmt.Sprintf("w%d", i)
	}
	base := newDupRecord("a", dupAt(0), "", strings.Join(words, " "), false)
	oneChanged := append(append([]string(nil), words[:19]...), "x1")
	threeChanged := append(append([]string(nil), words[:17]...), "x1", "x2", "x3")

	// Act
	scoreAbove, okAbove := dupSimilarity(base, newDupRecord("b", dupAt(1), "", strings.Join(oneChanged, " "), false))
	scoreBelow, okBelow := dupSimilarity(base, newDupRecord("c", dupAt(2), "", strings.Join(threeChanged, " "), false))

	// Assert
	if !okAbove || scoreAbove < duplicateThreshold {
		t.Fatalf("one changed word: score %v ok %v, want above %v", scoreAbove, okAbove, duplicateThreshold)
	}
	if okBelow || scoreBelow >= duplicateThreshold {
		t.Fatalf("three changed words: score %v ok %v, want below %v", scoreBelow, okBelow, duplicateThreshold)
	}
	if duplicateThreshold != 0.8 {
		t.Fatalf("duplicateThreshold = %v; update the documented threshold too", duplicateThreshold)
	}
}

func TestDupSimilarityShortRecordsNeedExactText(t *testing.T) {
	// Arrange
	a := newDupRecord("a", dupAt(0), "", "fix tests", false)
	same := newDupRecord("b", dupAt(1), "", "Fix tests!", false)
	other := newDupRecord("c", dupAt(2), "", "fix tests now", false)

	// Act
	_, okSame := dupSimilarity(a, same)
	_, okOther := dupSimilarity(a, other)

	// Assert
	if !okSame || okOther {
		t.Fatalf("short records: same %v (want true), other %v (want false)", okSame, okOther)
	}
}

func TestDupSimilaritySameTitleSimilarBody(t *testing.T) {
	// Arrange: body Jaccard is between duplicateTitleThreshold and duplicateThreshold.
	a := newDupRecord("a", dupAt(0), "Weekly sync", "one two three four five six seven eight nine ten eleven twelve", false)
	b := newDupRecord("b", dupAt(1), "Weekly sync", "one two three four five six seven eight nine ten zz yy", false)

	// Act
	score, ok := dupSimilarity(a, b)

	// Assert
	if !ok || score < duplicateTitleThreshold {
		t.Fatalf("score = %v ok = %v, want a duplicate through the title rule", score, ok)
	}
}

func TestNearDuplicateFindingsNeverCompareAcrossKinds(t *testing.T) {
	// Arrange
	text := "The search index is rebuilt on every sync and the old index is removed afterwards"
	in := reviewInputs{
		notes:     []domain.Note{{ID: "note-1", Title: "Index", Body: text, CreatedAt: dupAt(0)}},
		attempts:  []domain.Attempt{{ID: "attempt-1", Title: "Index", Hypothesis: text, CreatedAt: dupAt(1)}},
		decisions: []domain.Decision{{ID: "decision-1", Title: "Index", Context: text, Status: domain.DecisionAccepted, CreatedAt: dupAt(2)}},
	}

	// Act
	findings := nearDuplicateFindings(in)

	// Assert
	if len(findings) != 0 {
		t.Fatalf("cross-kind findings = %+v", findings)
	}
}

func TestNearDuplicateFindingsSkipDeadDecisions(t *testing.T) {
	// Arrange
	text := "Use SQLite FTS5 for the local search index because it ships with the driver"
	in := reviewInputs{decisions: []domain.Decision{
		{ID: "decision-old", Title: "Index engine", Context: text, Status: domain.DecisionSuperseded, CreatedAt: dupAt(0)},
		{ID: "decision-rej", Title: "Index engine", Context: text, Status: domain.DecisionRejected, CreatedAt: dupAt(1)},
		{ID: "decision-new", Title: "Index engine", Context: text, Status: domain.DecisionAccepted, CreatedAt: dupAt(2)},
	}}

	// Act
	findings := nearDuplicateFindings(in)

	// Assert
	if len(findings) != 0 {
		t.Fatalf("findings with superseded/rejected decisions = %+v", findings)
	}
}

func TestNearDuplicateFindingsOneFindingPerExtraCopy(t *testing.T) {
	// Arrange: three copies give two findings, each naming the older id first.
	text := "Publish the npm package through trusted publishing with OIDC instead of a token"
	in := reviewInputs{notes: []domain.Note{
		{ID: "note-c", Title: "npm", Body: text, CreatedAt: dupAt(2)},
		{ID: "note-a", Title: "npm", Body: text, CreatedAt: dupAt(0)},
		{ID: "note-b", Title: "npm", Body: text, CreatedAt: dupAt(1)},
	}}

	// Act
	findings := nearDuplicateFindings(in)

	// Assert
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want 2", findings)
	}
	first := findings[0]
	if !strings.Contains(first.Message, "note note-a and note-b") || !strings.Contains(first.Message, "100% similar") {
		t.Fatalf("message = %q", first.Message)
	}
	if first.Fixable || !strings.Contains(first.Fix, "brain record trash note-a --yes") {
		t.Fatalf("finding = %+v, want report-only with a trash suggestion", first)
	}
}

func TestNearDuplicateDecisionSuggestsSupersede(t *testing.T) {
	// Arrange
	text := "Use SQLite FTS5 for the local search index because it ships with the driver"
	in := reviewInputs{decisions: []domain.Decision{
		{ID: "decision-1", Title: "Index engine", Context: text, Status: domain.DecisionProposed, CreatedAt: dupAt(0)},
		{ID: "decision-2", Title: "Index engine", Context: text, Status: domain.DecisionAccepted, CreatedAt: dupAt(1)},
	}}

	// Act
	findings := nearDuplicateFindings(in)

	// Assert
	if len(findings) != 1 || !strings.Contains(findings[0].Fix, "brain record supersede decision-1 --by decision-2") {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestNearDuplicateAutoHandoffsDoNotFlood(t *testing.T) {
	// Arrange: 40 automatic handoffs with a short templated summary that
	// differs per session and the long next action the hook copies from the
	// state; plus one exact repeat that must still be found. Compared like
	// manual handoffs (summary + next action) they would all match.
	next := "release the next minor version after the docs, the changelog, the npm wrapper and the site " +
		"are updated and every check passes on both machines and the owner confirms the publishing plan"
	var auto, manual []domain.Handoff
	for i := 0; i < 40; i++ {
		summary := fmt.Sprintf("Session finished, tests pass. Changed module%d.", i)
		id := fmt.Sprintf("handoff-%02d", i)
		auto = append(auto, domain.Handoff{ID: id, Summary: "[auto] " + summary, NextAction: next, CreatedAt: dupAt(i)})
		manual = append(manual, domain.Handoff{ID: id, Summary: summary, NextAction: next, CreatedAt: dupAt(i)})
	}
	auto = append(auto, domain.Handoff{ID: "handoff-repeat", Summary: auto[5].Summary, NextAction: next, CreatedAt: dupAt(100)})

	// Act
	autoFindings := nearDuplicateFindings(reviewInputs{handoffs: auto})
	manualFindings := nearDuplicateFindings(reviewInputs{handoffs: manual})

	// Assert
	if len(autoFindings) != 1 || !strings.Contains(autoFindings[0].Message, "handoff-05 and handoff-repeat") {
		t.Fatalf("auto findings = %d %+v, want only the exact repeat", len(autoFindings), autoFindings)
	}
	if len(manualFindings) != 39 {
		t.Fatalf("manual findings = %d, want 39 (the guard is what keeps auto handoffs quiet)", len(manualFindings))
	}
}

func TestNearDuplicateAutoHandoffNeedsHigherThreshold(t *testing.T) {
	// Arrange: the same pair of texts is a duplicate as manual handoffs and
	// not as automatic ones (Jaccard 0.85, between the two thresholds).
	words := make([]string, 40)
	for i := range words {
		words[i] = fmt.Sprintf("w%d", i)
	}
	changed := append(append([]string(nil), words[:37]...), "x1", "x2", "w39")
	manual := []domain.Handoff{
		{ID: "h1", Summary: strings.Join(words, " "), CreatedAt: dupAt(0)},
		{ID: "h2", Summary: strings.Join(changed, " "), CreatedAt: dupAt(1)},
	}
	auto := []domain.Handoff{
		{ID: "h1", Summary: "[auto] " + manual[0].Summary, CreatedAt: dupAt(0)},
		{ID: "h2", Summary: "[auto] " + manual[1].Summary, CreatedAt: dupAt(1)},
	}

	// Act
	manualFindings := nearDuplicateFindings(reviewInputs{handoffs: manual})
	autoFindings := nearDuplicateFindings(reviewInputs{handoffs: auto})

	// Assert
	if len(manualFindings) != 1 || len(autoFindings) != 0 {
		t.Fatalf("manual %d (want 1), auto %d (want 0)", len(manualFindings), len(autoFindings))
	}
}

func TestNearDuplicateFindingsCapLargeSets(t *testing.T) {
	// Arrange: 3000 distinct short notes, two planted copies among the most
	// recent records and one planted copy older than the cap.
	notes := make([]domain.Note, 0, 3003)
	for i := 0; i < 3000; i++ {
		notes = append(notes, domain.Note{
			ID: fmt.Sprintf("note-%04d", i), Title: fmt.Sprintf("topic %d", i),
			Body:      fmt.Sprintf("record %d covers area %d with detail %d and owner %d", i, i%97, i*31, i%13),
			CreatedAt: dupAt(i),
		})
	}
	notes = append(notes,
		domain.Note{ID: "copy-recent-1", Title: notes[2990].Title, Body: notes[2990].Body, CreatedAt: dupAt(5000)},
		domain.Note{ID: "copy-recent-2", Title: notes[2995].Title, Body: notes[2995].Body, CreatedAt: dupAt(5001)},
		domain.Note{ID: "copy-old", Title: notes[10].Title, Body: notes[10].Body, CreatedAt: dupAt(11)},
	)

	// Act
	findings := nearDuplicateFindings(reviewInputs{notes: notes})

	// Assert
	if len(findings) != 2 {
		t.Fatalf("findings = %d %+v, want the 2 recent copies only", len(findings), findings)
	}
	for _, finding := range findings {
		if strings.Contains(finding.Message, "copy-old") {
			t.Fatalf("copy outside the %d most recent records reported: %q", duplicateMaxRecords, finding.Message)
		}
	}
}
