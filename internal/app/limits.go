package app

import (
	"path"
	"strings"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Limits are enforced here, at the app boundary, so every write path (CLI,
// MCP, session hooks, promote and future importers) shares one rule set. The
// helper runs before the store is touched and before the index is updated: a
// refused write leaves neither a record file nor an index row behind.

// writeContent is the bounded part of one record. The title is counted in
// Unicode code points, the body and metadata lists in bytes. A handoff has no
// title field; its summary is body text.
type writeContent struct {
	titleField string
	title      string
	bodyField  string
	body       []string
	// metadataField names the group in a limit error.
	metadataField string
	metadata      [][]string
}

func noteWriteContent(note domain.Note) writeContent {
	return writeContent{
		titleField:    "note.title",
		title:         note.Title,
		bodyField:     "note.body",
		body:          []string{note.Body},
		metadataField: "note.tags",
		metadata:      [][]string{note.Tags},
	}
}

func decisionWriteContent(decision domain.Decision) writeContent {
	return writeContent{
		titleField:    "decision.title",
		title:         decision.Title,
		bodyField:     "decision.context",
		body:          []string{decision.Context},
		metadataField: "decision.metadata",
		metadata:      [][]string{decision.Alternatives, decision.Consequences, decision.Evidence},
	}
}

func attemptWriteContent(attempt domain.Attempt) writeContent {
	return writeContent{
		titleField: "attempt.title",
		title:      attempt.Title,
		bodyField:  "attempt.body",
		body:       []string{attempt.Hypothesis, attempt.Result, attempt.Reason},
	}
}

func handoffWriteContent(handoff domain.Handoff) writeContent {
	return writeContent{
		bodyField: "handoff.body",
		body:      []string{handoff.Summary, handoff.NextAction, handoff.GitContext},
	}
}

func ideaWriteContent(idea domain.Idea) writeContent {
	return writeContent{
		titleField:    "idea.title",
		title:         idea.Title,
		bodyField:     "idea.body",
		body:          []string{idea.Body},
		metadataField: "idea.tags",
		metadata:      [][]string{idea.Tags},
	}
}

// recordWriteContent returns the bounded content of whichever record is set.
func recordWriteContent(record Record) writeContent {
	switch {
	case record.Note != nil:
		return noteWriteContent(*record.Note)
	case record.Decision != nil:
		return decisionWriteContent(*record.Decision)
	case record.Attempt != nil:
		return attemptWriteContent(*record.Attempt)
	case record.Handoff != nil:
		return handoffWriteContent(*record.Handoff)
	case record.Idea != nil:
		return ideaWriteContent(*record.Idea)
	default:
		return writeContent{}
	}
}

// limitViolations returns every configured cap the content exceeds. The write
// path refuses on the first one; doctor reports all of them for legacy data.
func limitViolations(limits config.LimitsConfig, content writeContent) []error {
	var violations []error
	if bodyBytes := textBytes(content.body...); bodyBytes > limits.MaxBodyBytes {
		violations = append(violations, domain.NewLimitError(
			content.bodyField, "limits.max_body_bytes", limits.MaxBodyBytes, bodyBytes, "bytes"))
	}
	if content.titleField != "" {
		if chars := utf8.RuneCountInString(content.title); chars > limits.MaxTitleChars {
			violations = append(violations, domain.NewLimitError(
				content.titleField, "limits.max_title_chars", limits.MaxTitleChars, chars, "chars"))
		}
	}
	if metadataBytes := metadataSize(content.metadata); metadataBytes > limits.MaxMetadataBytes {
		violations = append(violations, domain.NewLimitError(
			content.metadataField, "limits.max_metadata_bytes", limits.MaxMetadataBytes, metadataBytes, "bytes"))
	}
	return violations
}

// textBytes is the byte size of the free-text fields of one record.
func textBytes(parts ...string) int {
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	return total
}

// metadataSize is the combined byte size of the bounded lists.
func metadataSize(lists [][]string) int {
	total := 0
	for _, values := range lists {
		for _, value := range values {
			total += len(value)
		}
	}
	return total
}

// checkProtectedSelector is the early half of protection: a literal protected
// project id or a glob matching the previewed record path is refused before
// ensureProject can register the project. The canonical id is checked again
// after resolution, so an alias cannot bypass the rule.
func (a *App) checkProtectedSelector(selector, kind, title string, allowProtected bool) error {
	if allowProtected {
		return nil
	}
	trimmed := strings.TrimSpace(selector)
	if trimmed == "" {
		return nil
	}
	limits := a.cfg.EffectiveLimits()
	for _, project := range limits.ProtectedProjects {
		if strings.EqualFold(project, trimmed) {
			return domain.NewProtectedProjectError(trimmed)
		}
	}
	if len(limits.ProtectedGlobs) == 0 {
		return nil
	}
	recordPath := recordDataPath(trimmed, kind, recordFileHint(kind, title))
	for _, pattern := range limits.ProtectedGlobs {
		if protectedGlobMatch(pattern, recordPath) {
			return domain.NewProtectedGlobError(recordPath, pattern)
		}
		if protectedGlobMatch(pattern, trimmed) {
			return domain.NewProtectedGlobError(trimmed, pattern)
		}
	}
	return nil
}

// checkRecordWrite is the single enforcement point of the write-time limits.
// It refuses a protected project or data-directory path and then the size
// caps. It is called before any record file is published and before the index
// is updated.
func (a *App) checkRecordWrite(projectID, kind, file string, content writeContent, allowProtected bool) error {
	if err := a.checkWriteProtection(projectID, recordDataPath(projectID, kind, file), allowProtected); err != nil {
		return err
	}
	if violations := limitViolations(a.cfg.EffectiveLimits(), content); len(violations) > 0 {
		return violations[0]
	}
	return nil
}

// checkWriteProtection refuses a write to a protected project or to a path a
// protected glob matches.
func (a *App) checkWriteProtection(projectID, recordPath string, allowProtected bool) error {
	if allowProtected {
		return nil
	}
	limits := a.cfg.EffectiveLimits()
	for _, project := range limits.ProtectedProjects {
		if strings.EqualFold(project, projectID) {
			return domain.NewProtectedProjectError(projectID)
		}
	}
	for _, pattern := range limits.ProtectedGlobs {
		if protectedGlobMatch(pattern, recordPath) {
			return domain.NewProtectedGlobError(recordPath, pattern)
		}
		if protectedGlobMatch(pattern, projectID) {
			return domain.NewProtectedGlobError(projectID, pattern)
		}
	}
	return nil
}

// enforceCreateLimits checks a new record. allowProtected is the explicit
// override the CLI exposes as --allow-protected; MCP never sets it.
func (a *App) enforceCreateLimits(projectID, kind string, content writeContent, allowProtected bool) error {
	return a.checkRecordWrite(projectID, kind, recordFileHint(kind, content.title), content, allowProtected)
}

// enforceUpdateLimits checks a record the app is about to rewrite. A rewrite
// carries the whole record, so the caps apply to the stored body even when the
// change itself is a status or supersede link. Legacy oversized records are
// reported by doctor and must be shrunk before they can be updated.
func (a *App) enforceUpdateLimits(projectID, kind, id string, content writeContent) error {
	return a.checkRecordWrite(projectID, kind, id+".md", content, false)
}

// recordDataPath renders the data-directory path of a record. Globs use
// forward slashes on every platform.
func recordDataPath(projectID, kind, file string) string {
	if file == "" {
		file = recordFileHint(kind, "")
	}
	return path.Join("projects", projectID, recordDir(kind), file)
}

// recordFileHint previews the record file name before the id is allocated.
// The real name adds the date and a ULID, but protection targets a project or
// a directory, so the slug is the stable part a glob can describe.
func recordFileHint(kind, title string) string {
	slug := domain.Slugify(title)
	if slug == "" {
		return "record.md"
	}
	return slug + ".md"
}

// recordDir maps a record kind to its directory under the project.
func recordDir(kind string) string {
	switch kind {
	case ports.RecordNote:
		return "notes"
	case ports.RecordDecision:
		return "decisions"
	case ports.RecordAttempt:
		return "attempts"
	case ports.RecordHandoff:
		return "handoffs"
	case ports.RecordIdea:
		return "ideas"
	default:
		return "records"
	}
}

// protectedGlobMatch reports whether a slash-separated glob protects a record
// path. A pattern protects everything below a directory it matches, so
// "projects/global/**" covers every record of that project and
// "projects/templates" covers the whole tree.
func protectedGlobMatch(pattern, recordPath string) bool {
	pattern = strings.TrimSuffix(strings.TrimSpace(pattern), "/")
	if pattern == "" {
		return false
	}
	recordPath = path.Clean(strings.TrimPrefix(recordPath, "./"))
	for current := recordPath; ; current = path.Dir(current) {
		if matched, err := path.Match(pattern, current); err == nil && matched {
			return true
		}
		if current == "." || current == "/" || current == "" {
			break
		}
	}
	return false
}
