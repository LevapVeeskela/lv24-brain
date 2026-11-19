package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/gitinfo"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/winproc"
)

// FindingKind names one review heuristic.
type FindingKind string

// Severity ranks how much a finding needs attention. It is the only ordering a
// caller may branch on: error findings describe knowledge that may be wrong,
// warn findings describe knowledge that may be stale, and info findings are
// curation hints.
type Severity string

const (
	// SeverityError marks a finding that can make context wrong.
	SeverityError Severity = "error"
	// SeverityWarn marks a finding that can make context stale.
	SeverityWarn Severity = "warn"
	// SeverityInfo marks a curation hint.
	SeverityInfo Severity = "info"
)

// Valid reports whether s is a documented severity.
func (s Severity) Valid() bool {
	switch s {
	case SeverityError, SeverityWarn, SeverityInfo:
		return true
	default:
		return false
	}
}

// Review heuristics. Only next-behind-handoff and question-answered carry a
// fix; the others need a human or agent decision.
const (
	FindingNextBehindHandoff    FindingKind = "next-behind-handoff"
	FindingNextMaybeDone        FindingKind = "next-maybe-done"
	FindingQuestionAnswered     FindingKind = "question-answered"
	FindingDecisionContradicted FindingKind = "decision-contradicted"
	FindingStateExpired         FindingKind = "state-expired"
	FindingStateBehindCommits   FindingKind = "state-behind-commits"
	FindingDecisionCodeChanged  FindingKind = "decision-code-changed"
	FindingQuarantined          FindingKind = "quarantined"
	FindingStateQuarantined     FindingKind = "state-quarantined"
	FindingReviewOverdue        FindingKind = "review-overdue"
	FindingPendingRelations     FindingKind = "pending-relations"
	FindingMergeConflict        FindingKind = "merge-conflict"
	FindingRegistryConflict     FindingKind = "registry-conflict"
)

var findingKinds = []FindingKind{
	FindingNextBehindHandoff, FindingNextMaybeDone, FindingQuestionAnswered,
	FindingDecisionContradicted, FindingSuspectedContradiction, FindingStateExpired, FindingStateBehindCommits,
	FindingDecisionCodeChanged, FindingBrokenLink, FindingNearDuplicate,
	FindingQuarantined, FindingStateQuarantined, FindingSupersessionGap, FindingStaleSuperseded,
	FindingProposalStale, FindingProposalOrphan, FindingProposalContradicted,
	FindingReviewOverdue, FindingPendingRelations,
	FindingMergeConflict, FindingRegistryConflict,
}

// findingSpec is the review metadata of one finding kind: how much it matters,
// why it matters and what to do about it. It is shared by every producer of the
// kind, so a finding always explains itself even when the producer only knows
// the ids.
type findingSpec struct {
	Severity Severity
	// Why is one sentence about the risk of leaving the finding alone.
	Why string
	// Next is the safe next action for a person or agent.
	Next string
	// Confirm is what the reviewer must verify before acting.
	Confirm string
}

var findingSpecs = map[FindingKind]findingSpec{
	FindingQuarantined: {
		Severity: SeverityError,
		Why:      "unreviewed external text is excluded from trusted context, so the brain cannot use what it already stores.",
		Next:     "read the record, then clear it with `brain record review <id>` (or `brain record review --all`).",
		Confirm:  "the record says what its source says and is safe to include in context.",
	},
	FindingStateQuarantined: {
		Severity: SeverityError,
		Why:      "the project state is an unreviewed write, so its goal and next action stay out of trusted context.",
		Next:     "read the state, then clear it with `brain state review` (optionally `--trust verified|stated|inferred`).",
		Confirm:  "the state describes the current work and is safe to include in context.",
	},
	FindingStateExpired: {
		Severity: SeverityWarn,
		Why:      "the project state stopped being declared current, so its goal and next action may describe an older situation.",
		Next:     "confirm the state, then extend it with `brain state set --fresh-for 14d`.",
		Confirm:  "the goal, blockers and next action still match the work.",
	},
	FindingStateBehindCommits: {
		Severity: SeverityWarn,
		Why:      "commits landed after the last state update, so the state may not describe HEAD.",
		Next:     "update it with `brain state set --goal ... --next ...`.",
		Confirm:  "the goal and next action still describe the latest commits.",
	},
	FindingNextBehindHandoff: {
		Severity: SeverityWarn,
		Why:      "a later handoff names a different next action, so the state points at older work.",
		Next:     "run `brain review --fix` to adopt the handoff's next action.",
		Confirm:  "the handoff's next action is the work that should continue.",
	},
	FindingNextMaybeDone: {
		Severity: SeverityInfo,
		Why:      "a commit message matches the next action, so the state may already be done.",
		Next:     "confirm the work landed, then set the following step with `brain state set --next ...`.",
		Confirm:  "the commit completes the next action rather than part of it.",
	},
	FindingQuestionAnswered: {
		Severity: SeverityInfo,
		Why:      "a decision or a later note covers the open question, so the question list is stale.",
		Next:     "run `brain review --fix` to remove the question (kept in a review note).",
		Confirm:  "the evidence answers the whole question.",
	},
	FindingDecisionContradicted: {
		Severity: SeverityError,
		Why:      "a later failed attempt covers the same ground, so the decision may no longer hold.",
		Next:     "supersede it with `brain record supersede <old> --by <new>` or reaffirm it.",
		Confirm:  "the attempt's failure applies to this decision.",
	},
	FindingDecisionCodeChanged: {
		Severity: SeverityWarn,
		Why:      "files the decision governs changed, so the decision may describe code that no longer exists.",
		Next:     "re-read the decision and supersede or reaffirm it.",
		Confirm:  "the rule still holds for the changed files.",
	},
	FindingSuspectedContradiction: {
		Severity: SeverityWarn,
		Why:      "two current records disagree about one subject, so search, context and the next agent may act on both as if each were still true.",
		Next:     "open both records, accept the one that is true now and reject or supersede the other with `brain record supersede <older-id> --by <newer-id>`.",
		Confirm:  "both ids name the same subject and only one of the two statements is true now.",
	},
	FindingBrokenLink: {
		Severity: SeverityInfo,
		Why:      "a [[id]] reference names a record that does not exist, so the link is dead.",
		Next:     "fix the [[id]] text or restore the referenced record.",
		Confirm:  "the intended target id.",
	},
	FindingNearDuplicate: {
		Severity: SeverityWarn,
		Why:      "two records describe the same thing, so search and context show it twice and the copies can diverge.",
		Next:     "keep one: `brain record trash <older> --yes` (reversible) or supersede it.",
		Confirm:  "which record is authoritative.",
	},
	FindingSupersessionGap: {
		Severity: SeverityInfo,
		Why:      "a newer record reads like a replacement but is not linked, so both look current.",
		Next:     "if the newer one replaces it, run `brain record supersede <older> --by <newer>`.",
		Confirm:  "the newer record really replaces the older claim.",
	},
	FindingStaleSuperseded: {
		Severity: SeverityWarn,
		Why:      "a record expired without naming a replacement, so nothing current covers the claim.",
		Next:     "supersede it or clear its valid_to.",
		Confirm:  "whether a replacement exists.",
	},
	FindingProposalStale: {
		Severity: SeverityInfo,
		Why:      "the proposal no longer matches its source, so promoting it would record outdated text.",
		Next:     "confirm it with `brain promote <id> --force` or dismiss it.",
		Confirm:  "the source still supports the candidate.",
	},
	FindingProposalOrphan: {
		Severity: SeverityWarn,
		Why:      "the proposal's source is gone, so its evidence cannot be checked.",
		Next:     "dismiss it with `brain promote <id> --dismiss`.",
		Confirm:  "whether the claim stands without the source.",
	},
	FindingProposalContradicted: {
		Severity: SeverityWarn,
		Why:      "a later decision or failed attempt covers the candidate, so promoting it may add a stale claim.",
		Next:     "confirm it with `brain promote <id> --force` or dismiss it.",
		Confirm:  "the candidate still adds something the later record does not.",
	},
	FindingReviewOverdue: {
		Severity: SeverityWarn,
		Why:      "a review date is a prompt to re-check, and these claims have not been re-checked since it passed.",
		Next:     "run `brain review --due`, then `brain record review <id> --review-after 90d` or supersede the record.",
		Confirm:  "each listed record still states what the project believes.",
	},
	FindingPendingRelations: {
		Severity: SeverityWarn,
		Why:      "a pending relation is unjudged, so it is ignored by search and context and the pair stays ambiguous.",
		Next:     "judge the pair with `brain relation judge <from> <to> --verdict <relation> --reason <text>`.",
		Confirm:  "whether the two records agree, contradict each other, or are unrelated.",
	},
	FindingMergeConflict: {
		Severity: SeverityWarn,
		Why:      "a merge left a conflict artifact, so the kept value may not be the one both machines meant and one side's change lives only in the artifact.",
		Next:     "open the newest artifact under state-conflicts/ (or the merges/ manifest), compare both sides, fix the record if needed, then delete the artifact.",
		Confirm:  "the merged file kept the right value, or was edited to the right one.",
	},
	FindingRegistryConflict: {
		Severity: SeverityWarn,
		Why:      "an alias or checkout path was claimed by two projects during a registry merge, so one claim was dropped and selection may resolve to the other project.",
		Next:     "open the artifact under registry-conflicts/ and move the alias or path to the right project with `brain project set <id> --alias/--add-path`.",
		Confirm:  "which project owns the aliased name or the checkout path.",
	},
}

const (
	// staleCommitThreshold is the number of commits after the last state
	// update that makes the state look behind.
	staleCommitThreshold = 10
	// similarityThreshold is the token overlap two texts need to be treated
	// as being about the same thing.
	similarityThreshold = 0.6
	// minSharedTokens avoids matching on a single common word.
	minSharedTokens = 2
	// stemRunes truncates tokens so inflected forms (also in Russian) match.
	stemRunes = 6
)

// Finding is one piece of the brain that looks stale or contradictory.
type Finding struct {
	Kind FindingKind
	// Severity is error, warn or info; see the type documentation.
	Severity Severity
	// Message describes what was observed, with the ids a caller needs.
	Message string
	// Why is one sentence about the risk of leaving it alone.
	Why string
	// Next is the safe next action for a person or agent.
	Next string
	// Confirm is what the reviewer must verify before acting.
	Confirm string
	// Evidence is the record id, commit or text that supports the finding.
	Evidence string
	// Fixable reports whether `brain review --fix` repairs the finding.
	Fixable bool
	// Fix describes that repair.
	Fix string

	// Applied by FixFindings.
	nextAction   *string
	dropQuestion string
}

// newFinding builds a finding with the review metadata of its kind.
func newFinding(kind FindingKind, message, evidence string) Finding {
	spec := findingSpecs[kind]
	return Finding{
		Kind: kind, Severity: spec.Severity, Message: message,
		Why: spec.Why, Next: spec.Next, Confirm: spec.Confirm, Evidence: evidence,
	}
}

// withFix marks a finding as repairable by `brain review --fix`.
func (f Finding) withFix(fix string) Finding {
	f.Fixable = true
	f.Fix = fix
	return f
}

// withFixHint records the suggested repair without claiming `brain review
// --fix` applies it; the suggestion needs a person or agent.
func (f Finding) withFixHint(fix string) Finding {
	f.Fix = fix
	return f
}

// withSeverity overrides the kind's default severity, for findings whose
// urgency depends on their age.
func (f Finding) withSeverity(severity Severity) Finding {
	f.Severity = severity
	return f
}

// finalizeFinding fills review metadata a producer left empty, so no caller has
// to branch on an empty why or next.
func finalizeFinding(f Finding) Finding {
	spec := findingSpecs[f.Kind]
	if !f.Severity.Valid() {
		f.Severity = spec.Severity
	}
	if !f.Severity.Valid() {
		f.Severity = SeverityWarn
	}
	if f.Why == "" {
		f.Why = spec.Why
	}
	if f.Next == "" {
		f.Next = spec.Next
	}
	if f.Confirm == "" {
		f.Confirm = spec.Confirm
	}
	return f
}

func finalizeFindings(findings []Finding) []Finding {
	for i, finding := range findings {
		findings[i] = finalizeFinding(finding)
	}
	return findings
}

// FixResult lists the applied fixes and the resulting state.
type FixResult struct {
	Applied []Finding
	Changes []FixChange
	State   domain.State
}

// FixChange is one state field a safe review fix changes.
type FixChange struct {
	// Kind is the finding kind that caused the change.
	Kind FindingKind
	// Action is set_next_action or drop_question.
	Action string
	// Field is the state field the change writes.
	Field string
	// Value is the new value (the later handoff's next action or the removed
	// question).
	Value string
	// Evidence is the record id that justifies the change.
	Evidence string
}

// The safe review fixes, as fields of FixChange.Action.
const (
	FixSetNextAction = "set_next_action"
	FixDropQuestion  = "drop_question"
)

// FixPlan is what a review fix targets, computed without writing. It is the
// dry-run form of FixFindings and always describes the same safe repairs:
// adopting the next action of a later handoff and removing open questions a
// later record answers. Semantic fixes are never planned.
type FixPlan struct {
	ProjectID string
	// Findings lists the fixable findings the plan targets.
	Findings []Finding
	// Changes lists the exact state fields the plan writes.
	Changes []FixChange
	// State is the stored state the plan was computed from; it is unchanged.
	State domain.State
}

// ParseFindingKinds parses a comma-separated kind list; "" means all.
func ParseFindingKinds(csv string) ([]FindingKind, error) {
	if strings.TrimSpace(csv) == "" {
		return nil, nil
	}
	var kinds []FindingKind
	for _, raw := range strings.Split(csv, ",") {
		kind := FindingKind(strings.TrimSpace(raw))
		if !containsKind(findingKinds, kind) {
			names := make([]string, len(findingKinds))
			for i, known := range findingKinds {
				names[i] = string(known)
			}
			return nil, domain.NewValidationError("kind", fmt.Sprintf("unknown finding kind %q (known: %s)", kind, strings.Join(names, ", ")))
		}
		kinds = append(kinds, kind)
	}
	return kinds, nil
}

func containsKind(kinds []FindingKind, kind FindingKind) bool {
	for _, known := range kinds {
		if known == kind {
			return true
		}
	}
	return false
}

// reviewInputs is everything the heuristics read.
type reviewInputs struct {
	state     domain.State
	handoffs  []domain.Handoff // newest first
	notes     []domain.Note
	decisions []domain.Decision
	attempts  []domain.Attempt
	commits   []ports.JournalEntry // after the last state update, newest first
	checkout  string               // local checkout, "" when this machine has none
}

// The isStub* helpers adapt a record to IsStubRecord. Review and duplicate
// detection read the text a person would read: a note title and body, a
// decision title and context, an attempt title and its narrative, a handoff
// summary and next action. Generated label+id records and "nothing happened"
// records then produce no findings while staying in lists, counts and search
// results that do not filter stubs.
func isStubNote(note domain.Note) bool {
	return IsStubRecord(ports.RecordNote, note.Title, note.Body)
}

func isStubDecision(decision domain.Decision) bool {
	return IsStubRecord(ports.RecordDecision, decision.Title, decision.Context)
}

func isStubAttempt(attempt domain.Attempt) bool {
	return IsStubRecord(ports.RecordAttempt, attempt.Title,
		joinText(attempt.Hypothesis, attempt.Result, attempt.Reason))
}

// isStubHandoff checks both free-text fields: handoffs carry no title, so the
// summary is the text and a placeholder next action carries nothing either.
func isStubHandoff(handoff domain.Handoff) bool {
	return IsStubRecord(ports.RecordHandoff, "", handoff.Summary) ||
		IsStubRecord(ports.RecordHandoff, "", handoff.NextAction)
}

// Findings runs the review heuristics: deterministic token-overlap checks, no
// model calls. They flag candidates; a person or agent confirms them.
func (a *App) Findings(ctx context.Context, projectID string) ([]Finding, error) {
	in, err := a.reviewInputs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	now := a.now()
	var findings []Finding
	// Quarantine comes first so a short context or session summary never
	// truncates the one finding that says external text is waiting for review.
	findings = append(findings, quarantinedFindings(in)...)
	findings = append(findings, stateQuarantinedFinding(in)...)
	findings = append(findings, stateFindings(in, now)...)
	findings = append(findings, a.conflictFindings(in.state.ProjectID)...)
	findings = append(findings, nextActionFindings(in)...)
	findings = append(findings, questionFindings(in)...)
	findings = append(findings, decisionFindings(in)...)
	findings = append(findings, contradictionFindings(in, now, false)...)
	findings = append(findings, decisionCodeFindings(ctx, in)...)
	findings = append(findings, a.linkFindings(ctx, in)...)
	findings = append(findings, supersessionGapFindings(in)...)
	findings = append(findings, staleSupersededFindings(in, now)...)
	findings = append(findings, pendingRelationFindings(in, now)...)
	findings = append(findings, a.reviewOverdueFindings(in, now)...)
	// Last, so the shorter lists (context, session start) show the state
	// findings first.
	findings = append(findings, nearDuplicateFindings(in)...)
	return finalizeFindings(findings), nil
}

// FixFindings recomputes the findings and applies the fixable ones (kinds
// filters them; nil means every fixable kind) in one state update. Removed
// open questions are kept in a note so nothing is lost.
func (a *App) FixFindings(ctx context.Context, projectID string, kinds []FindingKind) (FixResult, error) {
	plan, input, dropped, err := a.planFixes(ctx, projectID, kinds)
	if err != nil {
		return FixResult{}, err
	}
	if len(plan.Findings) == 0 {
		return FixResult{State: plan.State}, nil
	}
	updated, err := a.SetState(ctx, input)
	if err != nil {
		return FixResult{}, err
	}
	// The note is written after the state change succeeded, so it never
	// reports a removal that did not happen.
	if len(dropped) > 0 {
		if err := a.noteDroppedQuestions(ctx, plan.ProjectID, dropped); err != nil {
			return FixResult{Applied: plan.Findings, Changes: plan.Changes, State: updated}, err
		}
	}
	return FixResult{Applied: plan.Findings, Changes: plan.Changes, State: updated}, nil
}

// PlanFixFindings computes the fixes FixFindings would apply without writing
// anything. It is the dry-run form of the safe review fix and never returns a
// semantic repair.
func (a *App) PlanFixFindings(ctx context.Context, projectID string, kinds []FindingKind) (FixPlan, error) {
	plan, _, _, err := a.planFixes(ctx, projectID, kinds)
	return plan, err
}

// planFixes is the shared selection of both the dry run and the write. It
// returns the plan, the state input to apply and the findings that drop an
// open question.
func (a *App) planFixes(ctx context.Context, projectID string, kinds []FindingKind) (FixPlan, StateInput, []Finding, error) {
	findings, err := a.Findings(ctx, projectID)
	if err != nil {
		return FixPlan{}, StateInput{}, nil, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return FixPlan{}, StateInput{}, nil, err
	}
	_, state, err := a.GetState(ctx, project.ID)
	if err != nil {
		return FixPlan{}, StateInput{}, nil, err
	}
	input := StateInput{ProjectID: project.ID}
	questions := append([]string(nil), state.OpenQuestions...)
	var applied, dropped []Finding
	var changes []FixChange
	for _, finding := range findings {
		if !finding.Fixable || (kinds != nil && !containsKind(kinds, finding.Kind)) {
			continue
		}
		switch {
		case finding.nextAction != nil:
			input.NextAction = finding.nextAction
			changes = append(changes, FixChange{
				Kind: finding.Kind, Action: FixSetNextAction, Field: "next_action",
				Value: *finding.nextAction, Evidence: finding.Evidence,
			})
		case finding.dropQuestion != "":
			questions = removeString(questions, finding.dropQuestion)
			dropped = append(dropped, finding)
			changes = append(changes, FixChange{
				Kind: finding.Kind, Action: FixDropQuestion, Field: "open_questions",
				Value: finding.dropQuestion, Evidence: finding.Evidence,
			})
		default:
			continue
		}
		applied = append(applied, finding)
	}
	if len(applied) == 0 {
		if state.ProjectID == "" {
			state.ProjectID = project.ID
		}
		return FixPlan{ProjectID: project.ID, State: state}, input, nil, nil
	}
	if len(dropped) > 0 {
		input.OpenQuestions = &questions
	}
	return FixPlan{ProjectID: project.ID, Findings: applied, Changes: changes, State: state}, input, dropped, nil
}

func (a *App) noteDroppedQuestions(ctx context.Context, projectID string, dropped []Finding) error {
	var body strings.Builder
	body.WriteString("`brain review --fix` removed open questions that later records seem to answer:\n")
	for _, finding := range dropped {
		fmt.Fprintf(&body, "- %s (answered by %s)\n", finding.dropQuestion, finding.Evidence)
	}
	_, err := a.Note(ctx, NoteInput{ProjectID: projectID, Title: "Review: answered open questions", Body: body.String()})
	return err
}

func (a *App) reviewInputs(ctx context.Context, projectID string) (reviewInputs, error) {
	if err := a.requireStore(); err != nil {
		return reviewInputs{}, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return reviewInputs{}, err
	}
	var in reviewInputs
	in.checkout = checkoutDir(project)
	if in.state, err = a.deps.Store.GetState(ctx, project.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return reviewInputs{}, err
	}
	// A project without a state file still reviews: the id anchors the state
	// section and the proposal lookup.
	in.state.ProjectID = project.ID
	if in.handoffs, err = a.listHandoffs(ctx, project.ID); err != nil {
		return reviewInputs{}, err
	}
	sort.Slice(in.handoffs, func(i, j int) bool { return in.handoffs[i].CreatedAt.After(in.handoffs[j].CreatedAt) })
	if in.notes, err = a.listNotes(ctx, project.ID); err != nil {
		return reviewInputs{}, err
	}
	if in.decisions, err = a.listDecisions(ctx, project.ID); err != nil {
		return reviewInputs{}, err
	}
	if in.attempts, err = a.listAttempts(ctx, project.ID); err != nil {
		return reviewInputs{}, err
	}
	if a.deps.Journal != nil && !in.state.UpdatedAt.IsZero() {
		entries, err := a.deps.Journal.Read(ctx, ports.JournalQuery{Project: project.ID, Event: JournalCommit, Since: in.state.UpdatedAt.Add(time.Second)})
		if err != nil {
			return reviewInputs{}, err
		}
		in.commits = entries
	}
	return in, nil
}

func quarantinedFindings(in reviewInputs) []Finding {
	ids := quarantinedIDs(in.notes, in.decisions, in.attempts, in.handoffs)
	if len(ids) == 0 {
		return nil
	}
	return []Finding{newFinding(FindingQuarantined,
		fmt.Sprintf("%d quarantined records are excluded from context until reviewed: %s; clear one with `brain record review <id>` or all with `brain record review --all`", len(ids), quarantineIDSample(ids)),
		strings.Join(ids, ", "))}
}

func stateQuarantinedFinding(in reviewInputs) []Finding {
	if !in.state.Provenance.Quarantined() {
		return nil
	}
	return []Finding{newFinding(FindingStateQuarantined,
		"project state is quarantined and excluded from trusted context until reviewed: clear it with `brain state review`",
		"state")}
}

// quarantineIDSample lists at most maxListedItems ids in a finding message; the
// full list stays in Evidence and `brain review`.
func quarantineIDSample(ids []string) string {
	if len(ids) <= maxListedItems {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:maxListedItems], ", ") + fmt.Sprintf(", … %d more", len(ids)-maxListedItems)
}

func stateFindings(in reviewInputs, now time.Time) []Finding {
	var findings []Finding
	if !in.state.FreshUntil.IsZero() && in.state.FreshUntil.Before(now) {
		findings = append(findings, newFinding(FindingStateExpired,
			fmt.Sprintf("state was marked current until %s; confirm it with `brain state set --fresh-for`", in.state.FreshUntil.UTC().Format(time.RFC3339)), ""))
	}
	if len(in.commits) >= staleCommitThreshold {
		findings = append(findings, newFinding(FindingStateBehindCommits,
			fmt.Sprintf("%d commits since the last state update; goal, active and next action may be behind", len(in.commits)),
			"commit "+in.commits[0].Fields["head"]))
	}
	return findings
}

func nextActionFindings(in reviewInputs) []Finding {
	next := strings.TrimSpace(in.state.NextAction)
	var findings []Finding
	for _, handoff := range in.handoffs {
		if !handoff.CreatedAt.After(in.state.UpdatedAt) {
			break
		}
		handoffNext := strings.TrimSpace(handoff.NextAction)
		if handoffNext == "" || strings.EqualFold(handoffNext, next) {
			continue
		}
		finding := newFinding(FindingNextBehindHandoff,
			fmt.Sprintf("a later handoff names a different next action: %q", SummarizeLine(handoffNext, 120)),
			handoff.ID).withFix("set next_action to " + quote(SummarizeLine(handoffNext, 120)))
		finding.nextAction = &handoffNext
		findings = append(findings, finding)
		break
	}
	if next == "" {
		return findings
	}
	for _, commit := range in.commits {
		subject := commit.Fields["subject"]
		if similar(next, subject) {
			findings = append(findings, newFinding(FindingNextMaybeDone,
				fmt.Sprintf("next action looks done by commit %q", SummarizeLine(subject, 100)),
				"commit "+commit.Fields["head"]))
			return findings
		}
	}
	return findings
}

func questionFindings(in reviewInputs) []Finding {
	var findings []Finding
	for _, question := range in.state.OpenQuestions {
		if evidence, ok := answerFor(question, in); ok {
			finding := newFinding(FindingQuestionAnswered,
				fmt.Sprintf("open question %q looks answered", SummarizeLine(question, 100)),
				evidence).withFix("remove the open question (kept in a review note)")
			finding.dropQuestion = question
			findings = append(findings, finding)
		}
	}
	return findings
}

// answerFor finds a decision (any time) or a note written after the last
// state update that covers the question.
//
// Removing a question is the one fix that drops owner-written text, so it
// needs a stronger match than the report-only heuristics.
func answerFor(question string, in reviewInputs) (string, bool) {
	for _, decision := range in.decisions {
		if similarWith(question, decision.Title+" "+decision.Context, answerSharedTokens) {
			return decision.ID, true
		}
	}
	for _, note := range in.notes {
		if note.CreatedAt.After(in.state.UpdatedAt) && similarWith(question, note.Title+" "+firstLine(note.Body), answerSharedTokens) {
			return note.ID, true
		}
	}
	return "", false
}

func decisionFindings(in reviewInputs) []Finding {
	var findings []Finding
	for _, decision := range in.decisions {
		if decision.Status == domain.DecisionRejected || decision.Status == domain.DecisionSuperseded || isStubDecision(decision) {
			continue
		}
		for _, attempt := range in.attempts {
			if isStubAttempt(attempt) || !attemptRecordsFailure(attempt) || !attempt.CreatedAt.After(decision.CreatedAt) || !similar(decision.Title, attempt.Title+" "+attempt.Hypothesis) {
				continue
			}
			findings = append(findings, newFinding(FindingDecisionContradicted,
				fmt.Sprintf("decision %q has a later failed attempt %q; supersede or confirm it", SummarizeLine(decision.Title, 80), SummarizeLine(attempt.Title, 80)),
				attempt.ID))
			break
		}
	}
	return findings
}

// commitsTouching lists the short hashes of commits after since that changed
// any of paths, newest first. It is a variable so tests can fake git.
var commitsTouching = gitCommitsTouching

// decisionCodeFindings flags accepted or proposed decisions whose governed
// paths changed in the checkout after the decision was last updated. It is
// report-only: a person or agent must confirm or supersede the decision.
func decisionCodeFindings(ctx context.Context, in reviewInputs) []Finding {
	if in.checkout == "" {
		return nil
	}
	var findings []Finding
	for _, decision := range in.decisions {
		if len(decision.Paths) == 0 || (decision.Status != domain.DecisionAccepted && decision.Status != domain.DecisionProposed) {
			continue
		}
		hashes, err := commitsTouching(ctx, in.checkout, decision.UpdatedAt, decision.Paths)
		if err != nil || len(hashes) == 0 {
			// Not a git checkout, git missing or timed out: nothing to report.
			continue
		}
		findings = append(findings, newFinding(FindingDecisionCodeChanged,
			fmt.Sprintf("decision %q governs %s; %d commits changed them since %s — confirm it still holds or supersede it",
				SummarizeLine(decision.Title, 80), strings.Join(decision.Paths, ", "), len(hashes), decision.UpdatedAt.UTC().Format("2006-01-02")),
			hashes[0]))
	}
	return findings
}

// checkoutDir returns the project's checkout on this machine, or "" when no
// registered local path exists here.
func checkoutDir(project domain.Project) string {
	dir := firstPath(project)
	if dir == "" {
		return ""
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

// gitCommitsTouching runs git log without a shell; the argument list is fixed
// and the paths follow "--" so they are never read as options.
func gitCommitsTouching(ctx context.Context, dir string, since time.Time, paths []string) ([]string, error) {
	runCtx, cancel := context.WithTimeout(ctx, gitinfo.Timeout)
	defer cancel()
	args := []string{"-C", dir, "log", "--since=" + since.UTC().Format(time.RFC3339), "--format=%h", "--"}
	args = append(args, paths...)
	cmd := exec.CommandContext(runCtx, "git", args...)
	winproc.Hide(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// similar reports whether b covers most of a's significant tokens (overlap
// coefficient over stemmed tokens, at least minSharedTokens shared).
func similar(a, b string) bool {
	return similarWith(a, b, minSharedTokens)
}

// answerSharedTokens is the overlap an open question needs before --fix may
// remove it.
const answerSharedTokens = 3

func similarWith(a, b string, minShared int) bool {
	left, right := tokenSet(a), tokenSet(b)
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	shared := 0
	for token := range left {
		if _, ok := right[token]; ok {
			shared++
		}
	}
	smaller := min(len(left), len(right))
	return shared >= minShared && float64(shared)/float64(smaller) >= similarityThreshold
}

func tokenSet(text string) map[string]struct{} {
	tokens := map[string]struct{}{}
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		runes := []rune(word)
		if len(runes) < 3 || stopWords[word] {
			continue
		}
		if len(runes) > stemRunes {
			runes = runes[:stemRunes]
		}
		tokens[string(runes)] = struct{}{}
	}
	return tokens
}

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "that": true, "this": true,
	"then": true, "into": true, "when": true, "should": true, "use": true, "not": true, "are": true,
	"was": true, "will": true, "can": true, "all": true, "our": true, "new": true, "add": true,
	"для": true, "что": true, "как": true, "это": true, "или": true, "при": true, "без": true,
	"надо": true, "нужно": true, "уже": true, "его": true, "она": true, "они": true, "еще": true, "ещё": true,
}

func removeString(values []string, target string) []string {
	out := values[:0]
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}

func quote(text string) string {
	return fmt.Sprintf("%q", text)
}
