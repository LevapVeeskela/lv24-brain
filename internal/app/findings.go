package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// FindingKind names one review heuristic.
type FindingKind string

// Review heuristics. Only next-behind-handoff and question-answered carry a
// fix; the others need a human or agent decision.
const (
	FindingNextBehindHandoff    FindingKind = "next-behind-handoff"
	FindingNextMaybeDone        FindingKind = "next-maybe-done"
	FindingQuestionAnswered     FindingKind = "question-answered"
	FindingDecisionContradicted FindingKind = "decision-contradicted"
	FindingStateExpired         FindingKind = "state-expired"
	FindingStateBehindCommits   FindingKind = "state-behind-commits"
)

var findingKinds = []FindingKind{
	FindingNextBehindHandoff, FindingNextMaybeDone, FindingQuestionAnswered,
	FindingDecisionContradicted, FindingStateExpired, FindingStateBehindCommits,
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
	Kind     FindingKind
	Message  string
	Evidence string
	Fixable  bool
	Fix      string

	// Applied by FixFindings.
	nextAction   *string
	dropQuestion string
}

// FixResult lists the applied fixes and the resulting state.
type FixResult struct {
	Applied []Finding
	State   domain.State
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
	findings = append(findings, stateFindings(in, now)...)
	findings = append(findings, nextActionFindings(in)...)
	findings = append(findings, questionFindings(in)...)
	findings = append(findings, decisionFindings(in)...)
	return findings, nil
}

// FixFindings recomputes the findings and applies the fixable ones (kinds
// filters them; nil means every fixable kind) in one state update. Removed
// open questions are kept in a note so nothing is lost.
func (a *App) FixFindings(ctx context.Context, projectID string, kinds []FindingKind) (FixResult, error) {
	findings, err := a.Findings(ctx, projectID)
	if err != nil {
		return FixResult{}, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return FixResult{}, err
	}
	_, state, err := a.GetState(ctx, project.ID)
	if err != nil {
		return FixResult{}, err
	}
	input := StateInput{ProjectID: project.ID}
	questions := append([]string(nil), state.OpenQuestions...)
	var applied, dropped []Finding
	for _, finding := range findings {
		if !finding.Fixable || (kinds != nil && !containsKind(kinds, finding.Kind)) {
			continue
		}
		switch {
		case finding.nextAction != nil:
			input.NextAction = finding.nextAction
		case finding.dropQuestion != "":
			questions = removeString(questions, finding.dropQuestion)
			dropped = append(dropped, finding)
		default:
			continue
		}
		applied = append(applied, finding)
	}
	if len(applied) == 0 {
		return FixResult{State: state}, nil
	}
	if len(dropped) > 0 {
		input.OpenQuestions = &questions
		if err := a.noteDroppedQuestions(ctx, project.ID, dropped); err != nil {
			return FixResult{}, err
		}
	}
	updated, err := a.SetState(ctx, input)
	if err != nil {
		return FixResult{}, err
	}
	return FixResult{Applied: applied, State: updated}, nil
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
	if in.state, err = a.deps.Store.GetState(ctx, project.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return reviewInputs{}, err
	}
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

func stateFindings(in reviewInputs, now time.Time) []Finding {
	var findings []Finding
	if !in.state.FreshUntil.IsZero() && in.state.FreshUntil.Before(now) {
		findings = append(findings, Finding{
			Kind:    FindingStateExpired,
			Message: fmt.Sprintf("state was marked current until %s; confirm it with `brain state set --fresh-for`", in.state.FreshUntil.UTC().Format(time.RFC3339)),
		})
	}
	if len(in.commits) >= staleCommitThreshold {
		findings = append(findings, Finding{
			Kind:     FindingStateBehindCommits,
			Message:  fmt.Sprintf("%d commits since the last state update; goal, active and next action may be behind", len(in.commits)),
			Evidence: "commit " + in.commits[0].Fields["head"],
		})
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
		findings = append(findings, Finding{
			Kind:       FindingNextBehindHandoff,
			Message:    fmt.Sprintf("a later handoff names a different next action: %q", SummarizeLine(handoffNext, 120)),
			Evidence:   handoff.ID,
			Fixable:    true,
			Fix:        "set next_action to " + quote(SummarizeLine(handoffNext, 120)),
			nextAction: &handoffNext,
		})
		break
	}
	if next == "" {
		return findings
	}
	for _, commit := range in.commits {
		subject := commit.Fields["subject"]
		if similar(next, subject) {
			findings = append(findings, Finding{
				Kind:     FindingNextMaybeDone,
				Message:  fmt.Sprintf("next action looks done by commit %q", SummarizeLine(subject, 100)),
				Evidence: "commit " + commit.Fields["head"],
			})
			return findings
		}
	}
	return findings
}

func questionFindings(in reviewInputs) []Finding {
	var findings []Finding
	for _, question := range in.state.OpenQuestions {
		if evidence, ok := answerFor(question, in); ok {
			findings = append(findings, Finding{
				Kind:         FindingQuestionAnswered,
				Message:      fmt.Sprintf("open question %q looks answered", SummarizeLine(question, 100)),
				Evidence:     evidence,
				Fixable:      true,
				Fix:          "remove the open question (kept in a review note)",
				dropQuestion: question,
			})
		}
	}
	return findings
}

// answerFor finds a decision (any time) or a note written after the last
// state update that covers the question.
func answerFor(question string, in reviewInputs) (string, bool) {
	for _, decision := range in.decisions {
		if similar(question, decision.Title+" "+decision.Context) {
			return decision.ID, true
		}
	}
	for _, note := range in.notes {
		if note.CreatedAt.After(in.state.UpdatedAt) && similar(question, note.Title+" "+firstLine(note.Body)) {
			return note.ID, true
		}
	}
	return "", false
}

func decisionFindings(in reviewInputs) []Finding {
	var findings []Finding
	for _, decision := range in.decisions {
		if decision.Status == domain.DecisionRejected || decision.Status == domain.DecisionSuperseded {
			continue
		}
		for _, attempt := range in.attempts {
			if !attempt.CreatedAt.After(decision.CreatedAt) || !similar(decision.Title, attempt.Title+" "+attempt.Hypothesis) {
				continue
			}
			findings = append(findings, Finding{
				Kind:     FindingDecisionContradicted,
				Message:  fmt.Sprintf("decision %q has a later failed attempt %q; supersede or confirm it", SummarizeLine(decision.Title, 80), SummarizeLine(attempt.Title, 80)),
				Evidence: attempt.ID,
			})
			break
		}
	}
	return findings
}

// similar reports whether b covers most of a's significant tokens (overlap
// coefficient over stemmed tokens, at least minSharedTokens shared).
func similar(a, b string) bool {
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
	return shared >= minSharedTokens && float64(shared)/float64(smaller) >= similarityThreshold
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
