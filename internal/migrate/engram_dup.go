package migrate

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

const (
	duplicateThreshold      = 0.8
	duplicateTitleThreshold = 0.6
	duplicateMinShingles    = 3
	duplicateShingleWords   = 3
	duplicateReportLimit    = 500
)

type comparisonText struct {
	project     string
	kind        string
	id          string
	externalID  string
	title       string
	body        string
	text        string
	shingles    map[string]struct{}
	bodyShing   map[string]struct{}
	fingerprint string
}

func newComparisonText(project, kind, id, externalID, title, body string) comparisonText {
	normalizedTitle := domain.NormalizeDedupText(title)
	normalizedBody := domain.NormalizeDedupText(body)
	text := strings.TrimSpace(normalizedTitle + " " + normalizedBody)
	return comparisonText{
		project: project, kind: kind, id: id, externalID: externalID,
		title: normalizedTitle, body: normalizedBody, text: text,
		shingles: wordShingles(text), bodyShing: wordShingles(normalizedBody),
		fingerprint: domain.BodyFingerprint(text),
	}
}

func wordShingles(text string) map[string]struct{} {
	words := strings.Fields(text)
	set := make(map[string]struct{}, len(words))
	for i := 0; i+duplicateShingleWords <= len(words); i++ {
		set[strings.Join(words[i:i+duplicateShingleWords], " ")] = struct{}{}
	}
	return set
}

func jaccard(left, right map[string]struct{}) float64 {
	if len(left) > len(right) {
		left, right = right, left
	}
	shared := 0
	for shingle := range left {
		if _, ok := right[shingle]; ok {
			shared++
		}
	}
	union := len(left) + len(right) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

func comparisonSimilarity(left, right comparisonText) (float64, bool) {
	if left.text == "" || right.text == "" {
		return 0, false
	}
	if left.text == right.text {
		return 1, true
	}
	if len(left.shingles) < duplicateMinShingles || len(right.shingles) < duplicateMinShingles {
		return 0, false
	}
	score := jaccard(left.shingles, right.shingles)
	if score >= duplicateThreshold {
		return score, true
	}
	if left.title != "" && left.title == right.title &&
		len(left.bodyShing) >= duplicateMinShingles && len(right.bodyShing) >= duplicateMinShingles {
		bodyScore := jaccard(left.bodyShing, right.bodyShing)
		if bodyScore >= duplicateTitleThreshold {
			return bodyScore, true
		}
	}
	return score, false
}

func importedComparisonText(record *engramRecord) comparisonText {
	body := record.body
	if record.kind == engramRecordDecision {
		body = strings.Join([]string{
			record.context,
			strings.Join(record.alternatives, " "),
			strings.Join(record.consequences, " "),
		}, " ")
	}
	return newComparisonText(record.project, record.kind, record.id, record.externalID, record.title, body)
}

// findCrossSourceDuplicates compares imported records with records that already
// exist in the target. It reports only: an import never edits or merges a
// curated record, and never resolves a contradiction by itself.
func findCrossSourceDuplicates(result *Result, records []*engramRecord) {
	existing, err := scanExistingRecords(result.TargetDir)
	if err != nil {
		result.warn("existing records could not be scanned for duplicates: " + err.Error())
		return
	}
	if len(existing) == 0 || len(records) == 0 {
		return
	}
	byTitle := map[string][]comparisonText{}
	byFingerprint := map[string][]comparisonText{}
	for _, record := range existing {
		byTitle[record.project+"\x00"+record.kind+"\x00"+record.title] = append(
			byTitle[record.project+"\x00"+record.kind+"\x00"+record.title], record)
		byFingerprint[record.project+"\x00"+record.kind+"\x00"+record.fingerprint] = append(
			byFingerprint[record.project+"\x00"+record.kind+"\x00"+record.fingerprint], record)
	}
	for _, imported := range records {
		if imported.quarantine {
			continue
		}
		text := importedComparisonText(imported)
		candidates := append([]comparisonText{}, byTitle[text.project+"\x00"+text.kind+"\x00"+text.title]...)
		candidates = append(candidates, byFingerprint[text.project+"\x00"+text.kind+"\x00"+text.fingerprint]...)
		best := comparisonText{}
		bestScore := 0.0
		seen := map[string]bool{}
		for _, candidate := range candidates {
			if candidate.id == text.id || seen[candidate.id] {
				continue
			}
			seen[candidate.id] = true
			if excludedExternal(candidate.externalID, text.externalID) {
				continue
			}
			if score, ok := comparisonSimilarity(text, candidate); ok && score > bestScore {
				best, bestScore = candidate, score
			}
		}
		if best.id == "" {
			continue
		}
		result.Duplicates = append(result.Duplicates, DuplicateRecord{
			Scope: "cross-source", Kind: imported.kind, Project: imported.project,
			ImportedID: imported.id, OtherID: best.id, Similarity: bestScore, Title: imported.title,
		})
	}
	limitDuplicates(result)
}

// findInternalDuplicates reports near-duplicates between imported rows. Only
// session summaries collapse automatically; every other pair is a review task.
func findInternalDuplicates(result *Result, records []*engramRecord) {
	ordered := make([]comparisonText, 0, len(records))
	for _, record := range records {
		if record.subtype == "session_summary" {
			continue
		}
		ordered = append(ordered, importedComparisonText(record))
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].project != ordered[j].project {
			return ordered[i].project < ordered[j].project
		}
		if ordered[i].kind != ordered[j].kind {
			return ordered[i].kind < ordered[j].kind
		}
		return ordered[i].id < ordered[j].id
	})
	byTitle := map[string]int{}
	byFingerprint := map[string]int{}
	for index := range ordered {
		current := ordered[index]
		candidateIndexes := []int{}
		if previous, ok := byTitle[current.project+"\x00"+current.kind+"\x00"+current.title]; ok {
			candidateIndexes = append(candidateIndexes, previous)
		}
		if previous, ok := byFingerprint[current.project+"\x00"+current.kind+"\x00"+current.fingerprint]; ok {
			candidateIndexes = append(candidateIndexes, previous)
		}
		best := -1
		bestScore := 0.0
		for _, candidateIndex := range candidateIndexes {
			candidate := ordered[candidateIndex]
			if candidate.id == current.id {
				continue
			}
			if score, ok := comparisonSimilarity(current, candidate); ok && score > bestScore {
				best, bestScore = candidateIndex, score
			}
		}
		if best >= 0 {
			result.Duplicates = append(result.Duplicates, DuplicateRecord{
				Scope: "internal", Kind: current.kind, Project: current.project,
				ImportedID: current.id, OtherID: ordered[best].id,
				Similarity: bestScore, Title: current.title,
			})
		}
		byTitle[current.project+"\x00"+current.kind+"\x00"+current.title] = index
		byFingerprint[current.project+"\x00"+current.kind+"\x00"+current.fingerprint] = index
	}
	limitDuplicates(result)
}

func excludedExternal(existing, imported string) bool {
	if existing == "" || imported == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(existing), strings.TrimSpace(imported))
}

func limitDuplicates(result *Result) {
	sort.SliceStable(result.Duplicates, func(i, j int) bool {
		if result.Duplicates[i].Project != result.Duplicates[j].Project {
			return result.Duplicates[i].Project < result.Duplicates[j].Project
		}
		if result.Duplicates[i].Kind != result.Duplicates[j].Kind {
			return result.Duplicates[i].Kind < result.Duplicates[j].Kind
		}
		return result.Duplicates[i].ImportedID < result.Duplicates[j].ImportedID
	})
	if len(result.Duplicates) > duplicateReportLimit {
		result.Duplicates = result.Duplicates[:duplicateReportLimit]
		result.warn("duplicate report was truncated; run brain duplicates after the import for the full list")
	}
}

type existingMarker struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	Source    *struct {
		ExternalID string `json:"externalId"`
	} `json:"source"`
}

type existingFrontmatter struct {
	ID        string `yaml:"id"`
	Title     string `yaml:"title"`
	CreatedAt string `yaml:"created_at"`
	Source    *struct {
		ExternalID string `yaml:"external_id"`
	} `yaml:"source"`
}

// scanExistingRecords reads just enough of the target to compare titles and
// bodies. A damaged or unreadable file is skipped: duplicate detection is a
// report, never a reason to fail an import.
func scanExistingRecords(target string) ([]comparisonText, error) {
	projectsDirPath := filepath.Join(target, projectsDir)
	entries, err := os.ReadDir(projectsDirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := []comparisonText{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		project := entry.Name()
		out = append(out, scanExistingNotes(filepath.Join(projectsDirPath, project, notesDir), project)...)
		out = append(out, scanExistingFiles(filepath.Join(projectsDirPath, project, decisionsDir), project, "decision")...)
	}
	return out, nil
}

func scanExistingNotes(dir, project string) []comparisonText {
	out := []comparisonText{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		body, err := markdown.Parse(raw, nil)
		if err != nil {
			continue
		}
		out = append(out, parseExistingNoteBody(project, body)...)
	}
	return out
}

func parseExistingNoteBody(project, body string) []comparisonText {
	out := []comparisonText{}
	lines := strings.Split(body, "\n")
	index := 0
	for index < len(lines) {
		trimmed := strings.TrimSpace(lines[index])
		if !strings.HasPrefix(trimmed, noteMarkerPrefix) || !strings.HasSuffix(trimmed, noteMarkerSuffix) {
			index++
			continue
		}
		payload := strings.TrimSuffix(strings.TrimPrefix(trimmed, noteMarkerPrefix), noteMarkerSuffix)
		var marker existingMarker
		if err := json.Unmarshal([]byte(payload), &marker); err != nil {
			index++
			continue
		}
		index++
		if index < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[index]), "## ") {
			index++
		}
		start := index
		for index < len(lines) {
			candidate := strings.TrimSpace(lines[index])
			if strings.HasPrefix(candidate, noteMarkerPrefix) && strings.HasSuffix(candidate, noteMarkerSuffix) {
				break
			}
			index++
		}
		external := ""
		if marker.Source != nil {
			external = marker.Source.ExternalID
		}
		out = append(out, newComparisonText(project, "note", marker.ID, external, marker.Title,
			strings.Join(lines[start:index], "\n")))
	}
	return out
}

func scanExistingFiles(dir, project, kind string) []comparisonText {
	out := []comparisonText{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var frontmatter existingFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			continue
		}
		external := ""
		if frontmatter.Source != nil {
			external = frontmatter.Source.ExternalID
		}
		id := frontmatter.ID
		if id == "" {
			id = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		}
		out = append(out, newComparisonText(project, kind, id, external, frontmatter.Title, body))
	}
	return out
}

// walkRegularFiles is shared by the target-tree hash.
func walkRegularFiles(root string, visit func(rel, full string, entry fs.DirEntry) error) error {
	return filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == backupsDir || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return visit(filepath.ToSlash(rel), current, entry)
	})
}
