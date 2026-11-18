package app

import (
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// contradictionFixture is the LIFE-3 acceptance fixture: eight true pairs
// (negation, antonym, numeric value change and temporal override, including
// Russian wording) and eight near-miss pairs that must stay unflagged. Every
// pair has its own subject, so a finding that names a near-miss id is a leak of
// the overlap gate, not an extra true pair.
func contradictionFixture() reviewInputs {
	return reviewInputs{
		notes: []domain.Note{
			// True: negation in Russian.
			{ID: "note-cache-ru-on", Title: "Кеширование запросов включено в поиске", CreatedAt: dupAt(2)},
			{ID: "note-cache-ru-off", Title: "Кеширование запросов не включено в поиске", CreatedAt: dupAt(3)},
			// True: antonym keep/remove.
			{ID: "note-import-keep", Title: "Keep the legacy import path for old clients", CreatedAt: dupAt(6)},
			{ID: "note-import-remove", Title: "Remove the legacy import path for old clients", CreatedAt: dupAt(7)},
			// True: numeric change in Russian.
			{ID: "note-logs-ru-30", Title: "Храним логи подключения 30 дней", CreatedAt: dupAt(12)},
			{ID: "note-logs-ru-90", Title: "Храним логи подключения 90 дней", CreatedAt: dupAt(13)},
			// Near miss: different subjects.
			{ID: "note-unrelated-pipeline", Title: "Deploy pipeline uses GitHub Actions runners", CreatedAt: dupAt(16)},
			{ID: "note-unrelated-parser", Title: "Parser handles UTF-8 input and escapes controls", CreatedAt: dupAt(17)},
			// Near miss: antonym pair, but the shared subject is below the gate.
			{ID: "note-ant-miss-a", Title: "Enable verbose logging for the importer", CreatedAt: dupAt(18)},
			{ID: "note-ant-miss-b", Title: "Disable the database cache for imports", CreatedAt: dupAt(19)},
			// Near miss: same antonym side, so the records agree.
			{ID: "note-ant-same-a", Title: "Enable debug logging in the CLI", CreatedAt: dupAt(20)},
			{ID: "note-ant-same-b", Title: "Enable tracing spans in the CLI", CreatedAt: dupAt(21)},
			// Near miss: both records negate.
			{ID: "note-neg-both-a", Title: "Cache must not store secrets in plaintext", CreatedAt: dupAt(24)},
			{ID: "note-neg-both-b", Title: "Cache must not store tokens in plaintext", CreatedAt: dupAt(25)},
			// Near miss: both records carry the temporal marker.
			{ID: "note-temp-both-a", Title: "The webhook delivery path is deprecated but stable", CreatedAt: dupAt(26)},
			{ID: "note-temp-both-b", Title: "The webhook delivery path is deprecated and still supported", CreatedAt: dupAt(27)},
			// Near miss: a stub and a real record would be a numeric pair.
			{ID: "note-2026-11-18-cache-window", Title: "note-2026-11-18-cache-window", Body: "Cache window is 60 minutes", CreatedAt: dupAt(28)},
			{ID: "note-cache-window-real", Title: "Cache window is 120 minutes", CreatedAt: dupAt(29)},
		},
		decisions: []domain.Decision{
			// True: negation in English.
			{ID: "decision-cache-affirm", Title: "Enable request caching for the search endpoint", CreatedAt: dupAt(0)},
			{ID: "decision-cache-negate", Title: "Request caching is not enabled for the search endpoint", CreatedAt: dupAt(1)},
			// True: antonym enable/disable.
			{ID: "decision-telemetry-on", Title: "Telemetry feature flag is enabled for desktop builds", CreatedAt: dupAt(4)},
			{ID: "decision-telemetry-off", Title: "Telemetry feature flag is disabled for desktop builds", CreatedAt: dupAt(5)},
			// True: antonym before/after.
			{ID: "decision-migration-before", Title: "Run the schema migration before deploy", CreatedAt: dupAt(8)},
			{ID: "decision-migration-after", Title: "Run the schema migration after deploy", CreatedAt: dupAt(9)},
			// True: numeric change (days).
			{ID: "decision-token-90", Title: "Gateway tokens expire after 90 days in the bridge", CreatedAt: dupAt(10)},
			{ID: "decision-token-180", Title: "Gateway tokens expire after 180 days in the bridge", CreatedAt: dupAt(11)},
			// True: temporal override.
			{ID: "decision-export-stable", Title: "The legacy export endpoint stays available to all clients", CreatedAt: dupAt(14)},
			{ID: "decision-export-deprecated", Title: "The legacy export endpoint is deprecated for all clients", CreatedAt: dupAt(15)},
			// Near miss: numbers differ, units differ.
			{ID: "decision-artifact-days", Title: "Artifact retention uses 90 days of history", CreatedAt: dupAt(22)},
			{ID: "decision-artifact-records", Title: "Artifact retention keeps 180 records of history", CreatedAt: dupAt(23)},
			// Near miss: the older record's validity ended without naming a
			// replacement, so it is history, not a current belief. It becomes
			// a true numeric pair when history is requested.
			{ID: "decision-backup-old", Title: "Backup window is 7 days on each node", CreatedAt: dupAt(30),
				Validity: domain.Validity{ValidTo: dupAt(31)}},
			{ID: "decision-backup-new", Title: "Backup window is 14 days on each node", CreatedAt: dupAt(31)},
		},
	}
}

func contradictionTrueIDs() map[string]bool {
	return map[string]bool{
		"note-cache-ru-on": true, "note-cache-ru-off": true,
		"note-import-keep": true, "note-import-remove": true,
		"note-logs-ru-30": true, "note-logs-ru-90": true,
		"decision-cache-affirm": true, "decision-cache-negate": true,
		"decision-telemetry-on": true, "decision-telemetry-off": true,
		"decision-migration-before": true, "decision-migration-after": true,
		"decision-token-90": true, "decision-token-180": true,
		"decision-export-stable": true, "decision-export-deprecated": true,
	}
}

func contradictionReason(finding Finding) string {
	message := finding.Message
	open := strings.Index(message, "suspected contradiction (")
	if open < 0 {
		return ""
	}
	rest := message[open+len("suspected contradiction ("):]
	end := strings.Index(rest, ")")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// TestSuspectedContradictionEightTrueEightNearMiss pins the LIFE-3 fixture: all
// eight true pairs are flagged with the right reason, and none of the eight
// near-miss pairs leaks in.
func TestSuspectedContradictionEightTrueEightNearMiss(t *testing.T) {
	in := contradictionFixture()

	findings := contradictionFindings(in, dupAt(1000), false)
	if len(findings) != 8 {
		t.Fatalf("suspected contradictions = %d %+v, want 8", len(findings), findings)
	}
	trueIDs := contradictionTrueIDs()
	reasons := map[string]int{}
	seenPairs := map[string]bool{}
	for _, finding := range findings {
		if finding.Kind != FindingSuspectedContradiction {
			t.Fatalf("kind = %q, want %q", finding.Kind, FindingSuspectedContradiction)
		}
		if finding.Severity != SeverityWarn || finding.Fixable {
			t.Fatalf("finding not report-only warning: %+v", finding)
		}
		if finding.Why == "" || finding.Next == "" || finding.Confirm == "" {
			t.Fatalf("finding metadata incomplete: %+v", finding)
		}
		if !strings.Contains(finding.Next, "open both records") || !strings.Contains(finding.Confirm, "true now") {
			t.Fatalf("finding does not tell the owner to open both and decide: %+v", finding)
		}
		parts := strings.Split(finding.Evidence, " <-> ")
		if len(parts) != 2 {
			t.Fatalf("evidence = %q, want both ids", finding.Evidence)
		}
		for _, id := range parts {
			if !trueIDs[id] {
				t.Fatalf("finding names near-miss record %q: %+v", id, finding)
			}
			if !strings.Contains(finding.Message, id) {
				t.Fatalf("message omits id %q: %q", id, finding.Message)
			}
		}
		if seenPairs[finding.Evidence] {
			t.Fatalf("pair reported twice: %q", finding.Evidence)
		}
		seenPairs[finding.Evidence] = true
		reasons[contradictionReason(finding)]++
	}
	wantReasons := map[string]int{
		ContradictionNegation: 2,
		ContradictionAntonym:  3,
		ContradictionNumeric:  2,
		ContradictionTemporal: 1,
	}
	for reason, want := range wantReasons {
		if reasons[reason] != want {
			t.Fatalf("%s findings = %d, want %d (all: %+v)", reason, reasons[reason], want, reasons)
		}
	}
	if len(reasons) != len(wantReasons) {
		t.Fatalf("unexpected reasons: %+v", reasons)
	}

	// The heuristics are deterministic: a second run reports the same pairs.
	again := contradictionFindings(in, dupAt(1000), false)
	for i := range findings {
		if findings[i].Message != again[i].Message || findings[i].Evidence != again[i].Evidence {
			t.Fatalf("run %d differs: %+v vs %+v", i, findings[i], again[i])
		}
	}
}

// TestSuspectedContradictionHistoryIsOptIn proves superseded records stay out
// of the default review and join it only when history is requested.
func TestSuspectedContradictionHistoryIsOptIn(t *testing.T) {
	in := contradictionFixture()

	current := contradictionFindings(in, dupAt(1000), false)
	if len(current) != 8 {
		t.Fatalf("current findings = %d, want 8", len(current))
	}
	history := contradictionFindings(in, dupAt(1000), true)
	if len(history) != 9 {
		t.Fatalf("history findings = %d %+v, want 9", len(history), history)
	}
	found := false
	for _, finding := range history {
		if finding.Evidence == "decision-backup-old <-> decision-backup-new" {
			found = true
			if got := contradictionReason(finding); got != ContradictionNumeric {
				t.Fatalf("backup reason = %q, want numeric", got)
			}
		}
	}
	if !found {
		t.Fatal("history review did not report the superseded numeric pair")
	}
}

func TestSuspectedContradictionAntonymPairs(t *testing.T) {
	tests := []struct {
		name       string
		older      string
		newer      string
		wantReason string
	}{
		{name: "enable/disable", older: "Enable telemetry exports", newer: "Disable telemetry exports", wantReason: ContradictionAntonym},
		{name: "allow/deny", older: "Allow guest access to the dashboard", newer: "Deny guest access to the dashboard", wantReason: ContradictionAntonym},
		{name: "increase/decrease", older: "Increase the context budget for reviews", newer: "Decrease the context budget for reviews", wantReason: ContradictionAntonym},
		{name: "keep/remove", older: "Keep the legacy import path", newer: "Remove the legacy import path", wantReason: ContradictionAntonym},
		{name: "before/after", older: "Run the migration before deploy", newer: "Run the migration after deploy", wantReason: ContradictionAntonym},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := contradictionPairFindings(tt.older, tt.newer)
			if len(findings) != 1 || contradictionReason(findings[0]) != tt.wantReason {
				t.Fatalf("findings = %+v, want one %s", findings, tt.wantReason)
			}
		})
	}
}

func TestSuspectedContradictionNumericPairs(t *testing.T) {
	tests := []struct {
		name       string
		older      string
		newer      string
		wantReason string
	}{
		{name: "days in English", older: "Tokens expire after 90 days", newer: "Tokens expire after 180 days", wantReason: ContradictionNumeric},
		{name: "days in Russian", older: "Храним логи 30 дней", newer: "Храним логи 90 дней", wantReason: ContradictionNumeric},
		{name: "compact day unit", older: "Retention window is 14d", newer: "Retention window is 30d", wantReason: ContradictionNumeric},
		{name: "different units are not a value change", older: "Retention uses 90 days", newer: "Retention keeps 180 records", wantReason: ""},
		{name: "same value is not a change", older: "Retention uses 90 days", newer: "Retention uses 90 days", wantReason: ""},
		{name: "one value restated plus an extra mention is not a change", older: "Retention uses 30 minutes", newer: "Retention uses 30 minutes and 5 minutes", wantReason: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := contradictionPairFindings(tt.older, tt.newer)
			if tt.wantReason == "" {
				if len(findings) != 0 {
					t.Fatalf("findings = %+v, want none", findings)
				}
				return
			}
			if len(findings) != 1 || contradictionReason(findings[0]) != tt.wantReason {
				t.Fatalf("findings = %+v, want one %s", findings, tt.wantReason)
			}
			if tt.name == "days in English" && !strings.Contains(findings[0].Message, "90 days") {
				t.Fatalf("numeric reason does not name the values: %q", findings[0].Message)
			}
		})
	}
}

func TestSuspectedContradictionNegationPairs(t *testing.T) {
	tests := []struct {
		name       string
		older      string
		newer      string
		wantReason string
	}{
		{name: "affirm against negate", older: "Caching is enabled for requests", newer: "Caching is not enabled for requests", wantReason: ContradictionNegation},
		{name: "russian negation", older: "Кеширование запросов включено", newer: "Кеширование запросов не включено", wantReason: ContradictionNegation},
		{name: "both negate", older: "Cache must not store secrets", newer: "Cache must not store tokens", wantReason: ""},
		{name: "neither negates", older: "The parser reads UTF-8 input", newer: "The parser writes UTF-8 input", wantReason: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := contradictionPairFindings(tt.older, tt.newer)
			if tt.wantReason == "" {
				if len(findings) != 0 {
					t.Fatalf("findings = %+v, want none", findings)
				}
				return
			}
			if len(findings) != 1 || contradictionReason(findings[0]) != tt.wantReason {
				t.Fatalf("findings = %+v, want one %s", findings, tt.wantReason)
			}
		})
	}
}

// TestSuspectedContradictionDoubleNegationAgrees covers the opposite reading:
// "not enabled" and "disabled" say the same thing, so neither the negation nor
// the antonym heuristic may report them.
func TestSuspectedContradictionDoubleNegationAgrees(t *testing.T) {
	for _, titles := range [][2]string{
		{"Caching is not enabled for requests", "Caching is disabled for requests"},
		{"Caching is enabled for requests", "Caching is not disabled for requests"},
	} {
		if findings := contradictionPairFindings(titles[0], titles[1]); len(findings) != 0 {
			t.Fatalf("%q against %q flagged: %+v", titles[0], titles[1], findings)
		}
	}
}

// TestSuspectedContradictionLiveFalsePositives replays the review noise found
// on the lv24-brain corpus: broad session summaries whose body negates many
// unrelated notes, and Step 7 notes that share a heading but make different
// claims around "instead"/"no longer".
func TestSuspectedContradictionLiveFalsePositives(t *testing.T) {
	tests := []struct {
		name  string
		older domain.Note
		newer domain.Note
	}{
		{
			name: "session summary against step note",
			older: domain.Note{
				ID: "note-lv24-brain-20261105-160712", CreatedAt: dupAt(0),
				Title: "Step 7: brain.ps1 delegates open/bootstrap/note/decision/attempt/handoff/review/",
				Body:  "The wrapper calls the Go engine.",
			},
			newer: domain.Note{
				ID: "note-2026-11-05-7bb65f83", CreatedAt: dupAt(1),
				Title: "Session summary: lv24-brain",
				Body: "Session work covered the release pipeline, the parser and the docs. " +
					"The wrapper did not call the Go engine on Windows.",
			},
		},
		{
			name: "session summary against parser note",
			older: domain.Note{
				ID: "note-parser-grammar", CreatedAt: dupAt(2),
				Title: "Parser grammar update",
				Body:  "Parser changed the grammar for escapes.",
			},
			newer: domain.Note{
				ID: "note-2026-11-05-session-parser", CreatedAt: dupAt(3),
				Title: "Session summary: parser work",
				Body:  "Parser did not change the grammar. Parser telemetry was reviewed.",
			},
		},
		{
			name: "step 7 heading with instead",
			older: domain.Note{
				ID: "note-lv24-brain-20261105-160712", CreatedAt: dupAt(4),
				Title: "Step 7: brain.ps1 delegates open/bootstrap/note/decision/attempt/handoff/review/",
				Body:  "The wrapper resolves the engine binary and forwards arguments.",
			},
			newer: domain.Note{
				ID: "note-2026-11-05-7ec3d16f", CreatedAt: dupAt(5),
				Title: "Step 7: brain.ps1 delegates to Go engine; migrate imports registry metadata",
				Body:  "Wrappers call brain.ps1 instead of the native binary.",
			},
		},
		{
			name: "step 7 heading with no longer",
			older: domain.Note{
				ID: "note-lv24-brain-20261105-161109", CreatedAt: dupAt(6),
				Title: "Step 7: brain.ps1 delegates open/bootstrap/note/decision/attempt/handoff/review/",
				Body:  "Diagnostics run from the desktop panel.",
			},
			newer: domain.Note{
				ID: "note-2026-11-05-7ec3d16f", CreatedAt: dupAt(7),
				Title: "Step 7: brain.ps1 delegates to Go engine; migrate imports registry metadata",
				Body:  "The Python bridge is no longer used.",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := contradictionFindings(reviewInputs{notes: []domain.Note{tt.older, tt.newer}}, dupAt(1000), false)
			if len(findings) != 0 {
				t.Fatalf("live false positive flagged: %+v", findings)
			}
		})
	}
}

// TestSuspectedContradictionNegationIsSentenceScoped proves a negation in one
// sentence of a long note cannot pair with another record that only shares the
// note's document topic.
func TestSuspectedContradictionNegationIsSentenceScoped(t *testing.T) {
	older := domain.Note{
		ID: "note-pipeline", CreatedAt: dupAt(0),
		Title: "Release pipeline notes",
		Body:  "The release pipeline runs on Windows and Linux. The parser did not regress.",
	}
	newer := domain.Note{
		ID: "note-parser", CreatedAt: dupAt(1),
		Title: "Release pipeline parser grammar",
		Body:  "The release pipeline changed the parser grammar.",
	}
	if findings := contradictionFindings(reviewInputs{notes: []domain.Note{older, newer}}, dupAt(1000), false); len(findings) != 0 {
		t.Fatalf("unrelated negated sentence flagged: %+v", findings)
	}
}

// TestSuspectedContradictionSkipsBroadBodies covers the body-size half of the
// broad-record rule: a long auto-summary is context, not a claim.
func TestSuspectedContradictionSkipsBroadBodies(t *testing.T) {
	long := "The wrapper calls the Go engine. "
	long += strings.Repeat("Release context was reviewed. ", 200)
	summary := domain.Note{
		ID: "note-long-summary", CreatedAt: dupAt(1),
		Title: "Release context",
		Body:  long + "The wrapper did not call the Go engine on Windows.",
	}
	note := domain.Note{ID: "note-wrapper", CreatedAt: dupAt(0), Title: "The wrapper calls the Go engine"}
	if !contradictionBroadRecord(summary.Title, summary.Body) {
		t.Fatal("fixture body is not over the broad-record limit")
	}
	if findings := contradictionFindings(reviewInputs{notes: []domain.Note{note, summary}}, dupAt(1000), false); len(findings) != 0 {
		t.Fatalf("broad body flagged: %+v", findings)
	}
}

// TestSuspectedContradictionSkipsReleaseSeries proves two release notes that
// name different versions are a series, even when one says a step failed and
// the other says it works.
func TestSuspectedContradictionSkipsReleaseSeries(t *testing.T) {
	older := domain.Note{
		ID: "note-v080", CreatedAt: dupAt(0),
		Title: "v0.8.0 published: npm via npm-publish.yml",
		Body:  "The npm OIDC publish did not work in release.yml.",
	}
	newer := domain.Note{
		ID: "note-v081", CreatedAt: dupAt(1),
		Title: "v0.8.1 published: npm via npm-publish.yml",
		Body:  "The npm OIDC publish works in release.yml.",
	}
	if findings := contradictionFindings(reviewInputs{notes: []domain.Note{older, newer}}, dupAt(1000), false); len(findings) != 0 {
		t.Fatalf("release series flagged: %+v", findings)
	}
}

// TestSuspectedContradictionTemporalNeedsNewerMarker keeps the override
// directional: only the newer record can retire the older claim.
func TestSuspectedContradictionTemporalNeedsNewerMarker(t *testing.T) {
	if findings := contradictionPairFindings("The old export endpoint is deprecated", "The old export endpoint is supported"); len(findings) != 0 {
		t.Fatalf("older marker flagged as an override: %+v", findings)
	}
	findings := contradictionPairFindings("The old export endpoint is supported", "The old export endpoint is deprecated")
	if len(findings) != 1 || contradictionReason(findings[0]) != ContradictionTemporal {
		t.Fatalf("newer marker not flagged: %+v", findings)
	}
}

// TestSuspectedContradictionSkipsStubs proves the stub filter is what keeps a
// generated label record out: the same body under a real title is reported.
func TestSuspectedContradictionSkipsStubs(t *testing.T) {
	stub := domain.Note{
		ID:        "note-2026-11-18-cache-window",
		Title:     "note-2026-11-18-cache-window",
		Body:      "Cache window is 60 minutes",
		CreatedAt: dupAt(0),
	}
	real := domain.Note{ID: "note-cache-window-real", Title: "Cache window is 120 minutes", CreatedAt: dupAt(1)}
	if !IsStubRecord(ports.RecordNote, stub.Title, stub.Body) {
		t.Fatal("fixture stub is not a stub")
	}
	if findings := contradictionFindings(reviewInputs{notes: []domain.Note{stub, real}}, dupAt(1000), false); len(findings) != 0 {
		t.Fatalf("stub produced findings: %+v", findings)
	}

	// The gate and the numeric heuristic would flag the same words without the
	// stub, so the empty result above is the stub filter, not a weak fixture.
	unstub := stub
	unstub.Title = "Cache window is 60 minutes"
	findings := contradictionFindings(reviewInputs{notes: []domain.Note{unstub, real}}, dupAt(1000), false)
	if len(findings) != 1 || contradictionReason(findings[0]) != ContradictionNumeric {
		t.Fatalf("real numeric pair = %+v, want one numeric finding", findings)
	}
}

// TestSuspectedContradictionKeepsFutureValidTo pins the validity boundary: a
// record whose validity ends in the future is current at the review instant,
// so it is compared even though ValidTo is set.
func TestSuspectedContradictionKeepsFutureValidTo(t *testing.T) {
	in := reviewInputs{notes: []domain.Note{
		{ID: "note-window-old", Title: "Retention uses 90 days", CreatedAt: dupAt(0),
			Validity: domain.Validity{ValidTo: dupAt(5000)}},
		{ID: "note-window-new", Title: "Retention uses 180 days", CreatedAt: dupAt(1)},
	}}
	if findings := contradictionFindings(in, dupAt(1000), false); len(findings) != 1 {
		t.Fatalf("future-ended record dropped from review: %+v", findings)
	}
}

// TestSuspectedContradictionSkipsLinkedPairs proves a real supersede is a
// decision, not a suspicion, even when history is reviewed.
func TestSuspectedContradictionSkipsLinkedPairs(t *testing.T) {
	in := reviewInputs{decisions: []domain.Decision{
		{ID: "decision-window-old", Title: "Backup window is 7 days", CreatedAt: dupAt(0),
			Validity: domain.Validity{ValidTo: dupAt(2), SupersededBy: "decision-window-new"}},
		{ID: "decision-window-new", Title: "Backup window is 14 days", CreatedAt: dupAt(1),
			Validity: domain.Validity{Supersedes: "decision-window-old"}},
	}}
	if findings := contradictionFindings(in, dupAt(1000), false); len(findings) != 0 {
		t.Fatalf("default review reported a superseded pair: %+v", findings)
	}
	if findings := contradictionFindings(in, dupAt(1000), true); len(findings) != 0 {
		t.Fatalf("history review repeated a linked pair: %+v", findings)
	}
}

// TestSuspectedContradictionRespectsProject keeps the check inside one project:
// the same words in two projects are not a contradiction of one belief.
func TestSuspectedContradictionRespectsProject(t *testing.T) {
	in := reviewInputs{notes: []domain.Note{
		{ID: "note-a", ProjectID: "alpha", Title: "Retention uses 90 days", CreatedAt: dupAt(0)},
		{ID: "note-b", ProjectID: "beta", Title: "Retention uses 180 days", CreatedAt: dupAt(1)},
	}}
	if findings := contradictionFindings(in, dupAt(1000), false); len(findings) != 0 {
		t.Fatalf("cross-project finding: %+v", findings)
	}
}

// TestSuspectedContradictionDoesNotRepeatDecisionContradicted pins the split
// from the existing kind: a failed later attempt is decision-contradicted, not
// a text contradiction, so the pair is never reported twice.
func TestSuspectedContradictionDoesNotRepeatDecisionContradicted(t *testing.T) {
	in := reviewInputs{
		decisions: []domain.Decision{{
			ID: "decision-union", Title: "Sync journals with git union merge",
			Status: domain.DecisionAccepted, CreatedAt: dupAt(0),
		}},
		attempts: []domain.Attempt{{
			ID: "attempt-union", Title: "Union merge for journals",
			Result: "failed: duplicated lines", CreatedAt: dupAt(1),
		}},
	}
	if findings := decisionFindings(in); len(findings) != 1 || findings[0].Kind != FindingDecisionContradicted {
		t.Fatalf("decision-contradicted findings = %+v, want one", findings)
	}
	if findings := contradictionFindings(in, dupAt(1000), false); len(findings) != 0 {
		t.Fatalf("suspected contradiction repeated the attempt pair: %+v", findings)
	}
}

// contradictionPairFindings runs the check over one isolated note pair.
func contradictionPairFindings(olderTitle, newerTitle string) []Finding {
	return contradictionFindings(reviewInputs{notes: []domain.Note{
		{ID: "note-older", Title: olderTitle, CreatedAt: dupAt(0)},
		{ID: "note-newer", Title: newerTitle, CreatedAt: dupAt(1)},
	}}, dupAt(1000), false)
}
