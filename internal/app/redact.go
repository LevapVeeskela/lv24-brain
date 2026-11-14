package app

import "github.com/levapveeskela/lv24-brain/internal/redact"

// redactedMarker replaces a secret found in record text.
const redactedMarker = redact.Marker

// Redact replaces credentials in text with [redacted]. It reports how many
// were found. Text without secrets is returned unchanged. The patterns live in
// internal/redact, shared with the server write path and ingestion.
func Redact(text string) (string, int) {
	return redact.Apply(text)
}

// RedactIngest is Redact for ingested external text: the curated patterns plus
// bearer and authorization headers, URL query tokens and more provider token
// shapes. The curated path (Redact) is unchanged.
func RedactIngest(text string) (string, int) {
	return redact.ApplyIngest(text)
}

// redactAll redacts every string the pointers refer to and returns the total.
func redactAll(values ...*string) int {
	total := 0
	for _, value := range values {
		if value == nil {
			continue
		}
		var count int
		*value, count = Redact(*value)
		total += count
	}
	return total
}

// redactList redacts every element of a list in place.
func redactList(values []string) int {
	total := 0
	for i := range values {
		var count int
		values[i], count = Redact(values[i])
		total += count
	}
	return total
}

func redactCopy(values []string) []string {
	if values == nil {
		return nil
	}
	out := append([]string(nil), values...)
	redactList(out)
	return out
}

func (in *NoteInput) redact() {
	redactAll(&in.Title, &in.Body)
	in.Tags = redactCopy(in.Tags)
}

func (in *DecisionInput) redact() {
	redactAll(&in.Title, &in.Context)
	in.Alternatives = redactCopy(in.Alternatives)
	in.Consequences = redactCopy(in.Consequences)
	in.Evidence = redactCopy(in.Evidence)
}

func (in *AttemptInput) redact() {
	redactAll(&in.Title, &in.Hypothesis, &in.Result, &in.Reason)
}

func (in *HandoffInput) redact() {
	redactAll(&in.Summary, &in.NextAction, &in.GitContext)
}

func (in *StateInput) redact() {
	if in.Goal != nil {
		goal := *in.Goal
		redactAll(&goal)
		in.Goal = &goal
	}
	if in.NextAction != nil {
		next := *in.NextAction
		redactAll(&next)
		in.NextAction = &next
	}
	for _, list := range []**[]string{&in.Active, &in.Blocked, &in.OpenQuestions} {
		if *list != nil {
			copied := redactCopy(**list)
			if copied == nil {
				copied = []string{}
			}
			*list = &copied
		}
	}
}
