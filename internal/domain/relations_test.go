package domain_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestRelationKindVocabularyIsClosed(t *testing.T) {
	want := []string{
		"relates_to", "supersedes", "conflicts_with", "compatible", "scoped",
		"depends_on", "implements", "blocks", "evidences", "contradicts", "not_conflict",
	}
	got := domain.RelationKindNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("vocabulary = %v, want %v", got, want)
	}
	for _, name := range want {
		kind := domain.RelationKind(name)
		if !kind.Valid() {
			t.Fatalf("%q is not valid", name)
		}
	}
	if domain.RelationKind("related").Valid() {
		t.Fatal("unknown verb accepted")
	}
	if _, err := domain.ParseRelationKind("  depends_on  "); err != nil {
		t.Fatalf("ParseRelationKind: %v", err)
	}
	if _, err := domain.ParseRelationKind("depends"); err == nil {
		t.Fatal("unknown verb parsed")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want a validation error", err)
	}
}

func TestRelationKindCyclesOnlyForHierarchies(t *testing.T) {
	for _, kind := range []domain.RelationKind{domain.RelationSupersedes, domain.RelationDependsOn} {
		if !kind.MustBeAcyclic() {
			t.Fatalf("%s must be acyclic", kind)
		}
	}
	for _, kind := range []domain.RelationKind{domain.RelationRelatesTo, domain.RelationConflictsWith, domain.RelationBlocks} {
		if kind.MustBeAcyclic() {
			t.Fatalf("%s must not require an acyclic graph", kind)
		}
	}
}

func TestRelationDefaultStatusIsAccepted(t *testing.T) {
	relation := domain.Relation{Target: "decision-a", Relation: domain.RelationRelatesTo}
	if got := relation.Normalize().Status; got != domain.RelationAccepted {
		t.Fatalf("status = %q, want accepted", got)
	}
	if relation.IsPending() {
		t.Fatal("a relation without a status is accepted, not pending")
	}
	if !(domain.Relation{Target: "decision-a", Relation: domain.RelationRelatesTo, Status: domain.RelationPending}).IsPending() {
		t.Fatal("explicit pending relation is not pending")
	}
}

func TestRelationValidation(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	valid := domain.Relation{Target: "decision-a", Relation: domain.RelationRelatesTo}
	if err := valid.Validate("relations[0]", "decision-b"); err != nil {
		t.Fatalf("valid relation rejected: %v", err)
	}
	cases := []struct {
		name     string
		relation domain.Relation
		self     string
		want     string
	}{
		{"self link", valid, "decision-a", "cannot relate to itself"},
		{"qualified target", domain.Relation{Target: "other/decision-a", Relation: domain.RelationRelatesTo}, "", "same project"},
		{"bad target", domain.Relation{Target: "nonsense", Relation: domain.RelationRelatesTo}, "", "not a record id"},
		{"unknown verb", domain.Relation{Target: "decision-a", Relation: "likes"}, "", "unknown relation"},
		{"unknown status", domain.Relation{Target: "decision-a", Relation: domain.RelationRelatesTo, Status: "maybe"}, "", "unknown status"},
		{"bad confidence", domain.Relation{Target: "decision-a", Relation: domain.RelationRelatesTo, Confidence: 1.5}, "", "between 0 and 1"},
		{"bad judge", domain.Relation{Target: "decision-a", Relation: domain.RelationRelatesTo, Status: domain.RelationAccepted, JudgedAt: now, VerdictBy: "model"}, "", "human or agent"},
		{"pending with verdict", domain.Relation{Target: "decision-a", Relation: domain.RelationRelatesTo, Status: domain.RelationPending, JudgedAt: now}, "", "unjudged"},
		{"long reason", domain.Relation{Target: "decision-a", Relation: domain.RelationRelatesTo, Reason: strings.Repeat("x", 501)}, "", "at most 500"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := test.relation.Validate("relations[0]", test.self)
			if err == nil {
				t.Fatal("invalid relation accepted")
			}
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err = %v, want a validation error", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRelationListNormalizeDedupesByTargetAndVerb(t *testing.T) {
	list := domain.RelationList{
		{Target: " decision-a ", Relation: " relates_to ", Status: domain.RelationPending, Reason: "candidate"},
		{Target: "decision-b", Relation: domain.RelationConflictsWith},
		{Target: "decision-a", Relation: domain.RelationRelatesTo, Status: domain.RelationAccepted, Reason: "judged"},
	}
	normalized := list.Normalize()
	if len(normalized) != 2 {
		t.Fatalf("normalized = %+v, want two entries", normalized)
	}
	if normalized[0].Reason != "judged" || normalized[0].Status != domain.RelationAccepted {
		t.Fatalf("later judgment did not replace the candidate: %+v", normalized[0])
	}
	if normalized[1].Status != domain.RelationAccepted {
		t.Fatalf("default status = %q", normalized[1].Status)
	}
}

func TestRelationListValidateRejectsDuplicatesAndTwoVerdicts(t *testing.T) {
	duplicate := domain.RelationList{
		{Target: "decision-a", Relation: domain.RelationRelatesTo},
		{Target: "decision-a", Relation: domain.RelationRelatesTo, Reason: "again"},
	}
	if err := duplicate.Validate("relations", "decision-b"); err == nil || !strings.Contains(err.Error(), "already has") {
		t.Fatalf("duplicate pair err = %v", err)
	}
	twoVerdicts := domain.RelationList{
		{Target: "decision-a", Relation: domain.RelationCompatible},
		{Target: "decision-a", Relation: domain.RelationConflictsWith},
	}
	if err := twoVerdicts.Validate("relations", "decision-b"); err == nil || !strings.Contains(err.Error(), "one verdict") {
		t.Fatalf("two verdicts err = %v", err)
	}
	withGeneric := domain.RelationList{
		{Target: "decision-a", Relation: domain.RelationRelatesTo},
		{Target: "decision-a", Relation: domain.RelationEvidences},
	}
	if err := withGeneric.Validate("relations", "decision-b"); err != nil {
		t.Fatalf("relates_to + verdict rejected: %v", err)
	}
	pendingPlusVerdict := domain.RelationList{
		{Target: "decision-a", Relation: domain.RelationRelatesTo, Status: domain.RelationPending},
		{Target: "decision-a", Relation: domain.RelationConflictsWith, Status: domain.RelationAccepted},
	}
	if err := pendingPlusVerdict.Validate("relations", "decision-b"); err != nil {
		t.Fatalf("pending candidate + verdict rejected: %v", err)
	}
}

func TestRelationListHelpers(t *testing.T) {
	list := domain.RelationList{
		{Target: "decision-a", Relation: domain.RelationRelatesTo, Status: domain.RelationPending},
		{Target: "decision-b", Relation: domain.RelationDependsOn, Status: domain.RelationAccepted},
	}
	withA := list.With(domain.Relation{Target: "decision-a", Relation: domain.RelationRelatesTo, Status: domain.RelationAccepted, Reason: "judged"})
	if len(withA) != 2 || withA[0].Status != domain.RelationAccepted {
		t.Fatalf("With did not upsert: %+v", withA)
	}
	if got := len(withA.Accepted()); got != 2 {
		t.Fatalf("accepted = %d, want 2", got)
	}
	if got := len(withA.Pending()); got != 0 {
		t.Fatalf("pending = %d, want 0", got)
	}
	if got := withA.Without("decision-a"); len(got) != 1 || got[0].Target != "decision-b" {
		t.Fatalf("Without = %+v", got)
	}
	if relation, ok := withA.Find("decision-b", domain.RelationDependsOn); !ok || relation.Target != "decision-b" {
		t.Fatalf("Find = %+v, %v", relation, ok)
	}
	if got := withA.ForTarget("decision-a"); len(got) != 1 {
		t.Fatalf("ForTarget = %+v", got)
	}
}

func TestValidateRelationGraphRejectsHierarchyCycles(t *testing.T) {
	cycle := map[string]domain.RelationList{
		"decision-a": {{Target: "decision-b", Relation: domain.RelationDependsOn}},
		"decision-b": {{Target: "decision-c", Relation: domain.RelationDependsOn}},
		"decision-c": {{Target: "decision-a", Relation: domain.RelationDependsOn}},
	}
	err := domain.ValidateRelationGraph(cycle)
	if err == nil || !strings.Contains(err.Error(), "depends_on chain would loop") {
		t.Fatalf("cycle err = %v", err)
	}
	supersede := map[string]domain.RelationList{
		"decision-a": {{Target: "decision-b", Relation: domain.RelationSupersedes}},
		"decision-b": {{Target: "decision-a", Relation: domain.RelationSupersedes}},
	}
	if err := domain.ValidateRelationGraph(supersede); err == nil || !strings.Contains(err.Error(), "supersedes chain would loop") {
		t.Fatalf("supersede cycle err = %v", err)
	}
	// A related_to loop is not a hierarchy and stays allowed.
	loose := map[string]domain.RelationList{
		"note-a": {{Target: "note-b", Relation: domain.RelationRelatesTo}},
		"note-b": {{Target: "note-a", Relation: domain.RelationRelatesTo}},
	}
	if err := domain.ValidateRelationGraph(loose); err != nil {
		t.Fatalf("relates_to loop rejected: %v", err)
	}
	// Pending edges claim nothing and cannot close a cycle.
	pending := map[string]domain.RelationList{
		"decision-a": {{Target: "decision-b", Relation: domain.RelationDependsOn, Status: domain.RelationPending}},
		"decision-b": {{Target: "decision-a", Relation: domain.RelationDependsOn}},
	}
	if err := domain.ValidateRelationGraph(pending); err != nil {
		t.Fatalf("pending edge closed a cycle: %v", err)
	}
	chain := map[string]domain.RelationList{
		"decision-a": {{Target: "decision-b", Relation: domain.RelationSupersedes}},
		"decision-b": {{Target: "decision-c", Relation: domain.RelationSupersedes}},
	}
	if err := domain.ValidateRelationGraph(chain); err != nil {
		t.Fatalf("linear chain rejected: %v", err)
	}
	// A long chain must not overflow the stack or be reported as a cycle.
	long := map[string]domain.RelationList{}
	for i := 0; i < 5000; i++ {
		long[recordID(i)] = domain.RelationList{{Target: recordID(i + 1), Relation: domain.RelationDependsOn}}
	}
	if err := domain.ValidateRelationGraph(long); err != nil {
		t.Fatalf("long chain rejected: %v", err)
	}
}

func recordID(index int) string {
	return "decision-" + strconv.Itoa(index)
}

func TestMergeInfoValidation(t *testing.T) {
	valid := domain.MergeInfo{MergedFrom: []string{"note-b", "note-c", "note-b"}}
	normalized := valid.Normalized()
	if len(normalized.MergedFrom) != 2 {
		t.Fatalf("merged_from = %v, want two entries", normalized.MergedFrom)
	}
	if err := valid.Validate("note.merge", "note-a"); err != nil {
		t.Fatalf("valid merge info rejected: %v", err)
	}
	if err := (domain.MergeInfo{MergedInto: "note-a"}).Validate("note.merge", "note-a"); err == nil {
		t.Fatal("self merge accepted")
	}
	both := domain.MergeInfo{MergedInto: "note-a", MergedFrom: []string{"note-b"}}
	if err := both.Validate("note.merge", "note-c"); err == nil || !strings.Contains(err.Error(), "tombstone") {
		t.Fatalf("tombstone with merged_from err = %v", err)
	}
	selfAbsorbed := domain.MergeInfo{MergedFrom: []string{"note-c"}}
	if err := selfAbsorbed.Validate("note.merge", "note-c"); err == nil {
		t.Fatal("record absorbed itself")
	}
	if !(domain.MergeInfo{MergedInto: "note-a"}).IsTombstone() {
		t.Fatal("merged_into is not reported as a tombstone")
	}
}
