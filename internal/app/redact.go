package app

import "regexp"

// redactedMarker replaces a secret found in record text.
const redactedMarker = "[redacted]"

// secretPatterns match credentials that must never reach brain data, which
// syncs through git and is injected into every agent session. Each pattern
// replaces the whole match except a leading label kept by group 1.
var secretPatterns = []*regexp.Regexp{
	// PEM private keys.
	regexp.MustCompile(`(?s)()-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	// Provider tokens with recognizable prefixes.
	regexp.MustCompile(`()\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,})`),
	regexp.MustCompile(`()\bglpat-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`()\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{24,}`),
	regexp.MustCompile(`()\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`()\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`()\bAIza[0-9A-Za-z_-]{35}\b`),
	// JSON Web Tokens.
	regexp.MustCompile(`()\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	// Credentials embedded in URLs: scheme://user:secret@host.
	regexp.MustCompile(`([a-z][a-z0-9+.-]*://[^/\s:@]+:)[^/\s@]+(@)`),
	// key = value / key: value assignments of secret-looking names.
	regexp.MustCompile(`(?i)(\b[a-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)["']?\s*[:=]\s*["']?)[^\s"'` + "`" + `]{8,}`),
}

// Redact replaces credentials in text with [redacted]. It reports how many
// were found. Text without secrets is returned unchanged.
func Redact(text string) (string, int) {
	if text == "" {
		return text, 0
	}
	count := 0
	for _, pattern := range secretPatterns {
		text = pattern.ReplaceAllStringFunc(text, func(match string) string {
			count++
			groups := pattern.FindStringSubmatch(match)
			prefix := ""
			suffix := ""
			if len(groups) > 1 {
				prefix = groups[1]
			}
			if len(groups) > 2 {
				suffix = groups[2]
			}
			return prefix + redactedMarker + suffix
		})
	}
	return text, count
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
