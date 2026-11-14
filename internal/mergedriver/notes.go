package mergedriver

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

const (
	noteMarkerPrefix = "<!-- brain:note "
	noteMarkerSuffix = " -->"
)

type notesDocument struct {
	frontmatter map[string]any
	entries     []noteEntry
}

type noteEntry struct {
	raw       string
	heading   string
	content   string
	id        string
	createdAt time.Time
	hash      string
}

func parseNotes(data []byte) (notesDocument, error) {
	doc := notesDocument{frontmatter: map[string]any{}}
	if len(strings.TrimSpace(string(data))) == 0 {
		return doc, nil
	}
	body, err := markdown.Parse(data, &doc.frontmatter)
	if err != nil {
		return notesDocument{}, err
	}
	doc.entries = splitNoteEntries(body)
	for i := range doc.entries {
		doc.entries[i].hash = entryHash(doc.entries[i].heading, doc.entries[i].content)
	}
	return doc, nil
}

// splitNoteEntries splits a notes body into marker blocks. A legacy file
// without markers is split on level-2 headings instead.
func splitNoteEntries(body string) []noteEntry {
	lines := strings.Split(body, "\n")
	var markerStarts []int
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), noteMarkerPrefix) {
			markerStarts = append(markerStarts, i)
		}
	}
	if len(markerStarts) > 0 {
		entries := make([]noteEntry, 0, len(markerStarts))
		for i, start := range markerStarts {
			end := len(lines)
			if i+1 < len(markerStarts) {
				end = markerStarts[i+1]
			}
			entries = append(entries, newNoteEntry(lines[start:end]))
		}
		return entries
	}

	var entries []noteEntry
	var preamble []string
	var block []string
	flushPreamble := func() {
		text := strings.TrimSpace(strings.Join(preamble, "\n"))
		if text == "" {
			return
		}
		entries = append(entries, noteEntry{raw: text, content: text})
		preamble = nil
	}
	flushBlock := func() {
		if len(block) == 0 {
			return
		}
		entries = append(entries, newNoteEntry(block))
		block = nil
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			flushPreamble()
			flushBlock()
			block = []string{line}
			continue
		}
		if len(block) == 0 {
			preamble = append(preamble, line)
			continue
		}
		block = append(block, line)
	}
	flushPreamble()
	flushBlock()
	if len(entries) == 0 {
		if text := strings.TrimSpace(strings.Join(lines, "\n")); text != "" {
			entries = append(entries, noteEntry{raw: text, content: text})
		}
	}
	return entries
}

func newNoteEntry(lines []string) noteEntry {
	entry := noteEntry{raw: strings.Trim(strings.Join(lines, "\n"), "\n")}
	headingIndex := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, noteMarkerPrefix) {
			entry.id, entry.createdAt = noteMarkerMetadata(trimmed)
			continue
		}
		if headingIndex < 0 && strings.HasPrefix(line, "## ") {
			headingIndex = i
			entry.heading = strings.TrimSpace(strings.TrimPrefix(line, "## "))
		}
	}
	if headingIndex >= 0 {
		entry.content = strings.TrimSpace(strings.Join(lines[headingIndex+1:], "\n"))
	} else {
		var content []string
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), noteMarkerPrefix) {
				continue
			}
			content = append(content, line)
		}
		entry.content = strings.TrimSpace(strings.Join(content, "\n"))
	}
	return entry
}

func noteMarkerMetadata(line string) (string, time.Time) {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, noteMarkerPrefix)
	line = strings.TrimSuffix(line, noteMarkerSuffix)
	var metadata struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &metadata); err != nil {
		return "", time.Time{}
	}
	return metadata.ID, metadata.CreatedAt.UTC()
}

// mergeNoteEntries unions both sides by heading plus body hash and orders the
// result by creation time. An entry the base had and one side removed is
// dropped only while the other side kept it unchanged.
func mergeNoteEntries(base, ours, theirs []noteEntry) []noteEntry {
	baseByHash := make(map[string]struct{}, len(base))
	for _, entry := range base {
		baseByHash[entry.hash] = struct{}{}
	}
	theirsHashes := make(map[string]struct{}, len(theirs))
	for _, entry := range theirs {
		theirsHashes[entry.hash] = struct{}{}
	}
	oursHashes := make(map[string]struct{}, len(ours))
	for _, entry := range ours {
		oursHashes[entry.hash] = struct{}{}
	}

	seen := make(map[string]struct{}, len(ours)+len(theirs))
	merged := make([]noteEntry, 0, len(ours)+len(theirs))
	add := func(entry noteEntry) {
		if entry.hash == "" {
			entry.hash = entryHash(entry.heading, entry.content)
		}
		if _, ok := seen[entry.hash]; ok {
			return
		}
		seen[entry.hash] = struct{}{}
		merged = append(merged, entry)
	}
	for _, entry := range ours {
		if _, inBase := baseByHash[entry.hash]; inBase {
			if _, keptByTheirs := theirsHashes[entry.hash]; !keptByTheirs {
				continue
			}
		}
		add(entry)
	}
	for _, entry := range theirs {
		if _, inBase := baseByHash[entry.hash]; inBase {
			if _, keptByOurs := oursHashes[entry.hash]; !keptByOurs {
				continue
			}
		}
		add(entry)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		left, right := merged[i], merged[j]
		if !left.createdAt.Equal(right.createdAt) {
			if left.createdAt.IsZero() {
				return false
			}
			if right.createdAt.IsZero() {
				return true
			}
			return left.createdAt.Before(right.createdAt)
		}
		if left.id != right.id {
			return left.id < right.id
		}
		if left.heading != right.heading {
			return left.heading < right.heading
		}
		return left.hash < right.hash
	})
	return merged
}
