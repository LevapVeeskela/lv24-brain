package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

// A daily notes file can only carry one updated_at in its frontmatter, and the
// store applies that value to every note in the file. Earlier engram imports
// therefore lost the per-note updated_at of every note that shared a day with a
// later edit. This repair re-renders the engram note files, one file per exact
// updated_at, so the store reads each note back with its own timestamp.

// legacyEngramNoteFile reports a name written by the old renderer:
// engram-<date>.md or engram-<date>-<chunk>.md. The current renderer always
// includes a six-digit HHMMSS stamp, so the two shapes cannot be confused.
func legacyEngramNoteFile(name string) bool {
	base, ok := engramNoteFileName(name)
	if !ok {
		return false
	}
	return len(base) == 3 || (len(base) == 4 && len(base[3]) <= 2)
}

// engramNoteFileName splits engram-<date>[-<part>...].md into its parts.
func engramNoteFileName(name string) ([]string, bool) {
	if !strings.HasPrefix(name, "engram-") || !strings.HasSuffix(name, ".md") {
		return nil, false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(name, "engram-"), ".md"), "-")
	if len(parts) < 3 {
		return nil, false
	}
	if len(parts[0]) != 4 || len(parts[1]) != 2 || len(parts[2]) != 2 {
		return nil, false
	}
	for _, part := range parts {
		if part == "" || strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return nil, false
		}
	}
	return parts, true
}

type engramNoteSegment struct {
	source string
	text   string
}

// engramNoteSegments splits a note body into its marker entries. The text is
// whitespace-normalized so a re-render that only moves blank lines still
// matches, while any added, removed or edited entry does not. ok is false when
// the body starts with owner text or a marker cannot be decoded.
func engramNoteSegments(body string) (map[string]engramNoteSegment, bool) {
	segments := map[string]engramNoteSegment{}
	lines := strings.Split(body, "\n")
	id := ""
	source := ""
	buffer := make([]string, 0, 16)
	flush := func() {
		if id == "" {
			return
		}
		segments[id] = engramNoteSegment{source: source, text: strings.Join(strings.Fields(strings.Join(buffer, "\n")), " ")}
		buffer = buffer[:0]
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, noteMarkerPrefix) && strings.HasSuffix(trimmed, noteMarkerSuffix) {
			flush()
			payload := strings.TrimSuffix(strings.TrimPrefix(trimmed, noteMarkerPrefix), noteMarkerSuffix)
			var marker struct {
				ID     string           `json:"id"`
				Source *engramSourceOut `json:"source,omitempty"`
			}
			if err := json.Unmarshal([]byte(payload), &marker); err != nil || marker.ID == "" {
				return nil, false
			}
			if _, seen := segments[marker.ID]; seen {
				return nil, false
			}
			id = marker.ID
			source = ""
			if marker.Source != nil {
				source = marker.Source.ExternalID
			}
			continue
		}
		if id == "" {
			if trimmed != "" {
				// Owner text before the first entry: never replace this file.
				return nil, false
			}
			continue
		}
		buffer = append(buffer, line)
	}
	flush()
	if len(segments) == 0 {
		return nil, false
	}
	return segments, true
}

// engramPlannedSegments maps project\x00date to the note entries this run
// renders for that day, so a legacy file can be compared with the plan before it
// is set aside. A day that cannot be rendered unambiguously is left out.
func engramPlannedSegments(plan *engramPlan) map[string]map[string]engramNoteSegment {
	expected := map[string]map[string]engramNoteSegment{}
	invalid := map[string]bool{}
	for _, file := range plan.files {
		if file.kind != "note" {
			continue
		}
		parts, ok := engramNoteFileName(path.Base(file.rel))
		if !ok {
			continue
		}
		key := file.project + "\x00" + strings.Join(parts[:3], "-")
		if invalid[key] {
			continue
		}
		body, err := markdown.Parse(file.content, nil)
		if err != nil {
			invalid[key] = true
			delete(expected, key)
			continue
		}
		segments, ok := engramNoteSegments(body)
		if !ok {
			invalid[key] = true
			delete(expected, key)
			continue
		}
		group := expected[key]
		if group == nil {
			group = map[string]engramNoteSegment{}
			expected[key] = group
		}
		for id, segment := range segments {
			if _, clash := group[id]; clash {
				invalid[key] = true
				delete(expected, key)
				break
			}
			group[id] = segment
		}
	}
	return expected
}

// engramOwnedNoteFile reports whether the legacy file holds exactly the notes
// this plan renders for that day, with the same bodies. Only then is setting it
// aside safe: an added note, a hand edit or a source change keeps the file.
func engramOwnedNoteFile(file, project, date string, expected map[string]map[string]engramNoteSegment) (bool, error) {
	want := expected[project+"\x00"+date]
	if want == nil {
		return false, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return false, fmt.Errorf("migrate: read %s: %w", file, err)
	}
	body, err := markdown.Parse(raw, nil)
	if err != nil {
		return false, nil
	}
	got, ok := engramNoteSegments(body)
	if !ok || len(got) != len(want) {
		return false, nil
	}
	for id, segment := range want {
		found, exists := got[id]
		if !exists || found.text != segment.text || !strings.HasPrefix(found.source, EngramSourceSystem+":") {
			return false, nil
		}
	}
	return true, nil
}

// engramNoteBody reads one note file and returns its body, or ok=false when the
// file is unreadable or has no frontmatter.
func engramNoteBody(file string) (string, bool) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	body, err := markdown.Parse(raw, nil)
	if err != nil {
		return "", false
	}
	return body, true
}

// legacyLayoutDays reports the days whose legacy daily engram file still holds
// notes this plan would import. Writing the per-timestamp files for such a day
// would import every note twice, so the caller refuses, skips the day or repairs
// the file. The key is project\x00date, the value the legacy file name.
func legacyLayoutDays(target string, plan *engramPlan) (map[string]string, error) {
	planned := make(map[string]bool, len(plan.records))
	projects := map[string]struct{}{}
	for _, record := range plan.records {
		planned[record.project+"\x00"+record.id] = true
		projects[record.project] = struct{}{}
	}
	keep := make(map[string]bool, len(plan.files))
	for _, file := range plan.files {
		keep[file.rel] = true
	}
	blocked := map[string]string{}
	for _, project := range sortedKeys(projects) {
		dirRel := path.Join(projectsDir, project, notesDir)
		entries, err := os.ReadDir(filepath.Join(target, filepath.FromSlash(dirRel)))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("migrate: scan %s: %w", dirRel, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.Type().IsRegular() || !legacyEngramNoteFile(name) {
				continue
			}
			rel := path.Join(dirRel, name)
			if keep[rel] {
				continue
			}
			parts, _ := engramNoteFileName(name)
			key := project + "\x00" + strings.Join(parts[:3], "-")
			if _, seen := blocked[key]; seen {
				continue
			}
			body, ok := engramNoteBody(filepath.Join(target, filepath.FromSlash(rel)))
			if !ok {
				continue
			}
			segments, ok := engramNoteSegments(body)
			if !ok {
				continue
			}
			for id, segment := range segments {
				if planned[project+"\x00"+id] && strings.HasPrefix(segment.source, EngramSourceSystem+":") {
					blocked[key] = name
					break
				}
			}
		}
	}
	return blocked, nil
}

// legacyConflictResults turns blocked days into conflict rows for the report.
func legacyConflictResults(blocked map[string]string) []FileResult {
	keys := make([]string, 0, len(blocked))
	for key := range blocked {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	results := make([]FileResult, 0, len(keys))
	for _, key := range keys {
		parts := strings.SplitN(key, "\x00", 2)
		results = append(results, FileResult{
			ProjectID: parts[0], Kind: "note",
			TargetPath: path.Join(projectsDir, parts[0], notesDir, blocked[key]),
			Status:     StatusConflict,
			Error:      "daily file from an earlier import; rerun with --repair-timestamps so each note keeps its own updated_at",
		})
	}
	return results
}

// dropBlockedDays removes the planned note files of a blocked day and records
// one warning per day.
func dropBlockedDays(plan *engramPlan, blocked map[string]string, result *Result) {
	if len(blocked) == 0 {
		return
	}
	kept := make([]engramPlannedFile, 0, len(plan.files))
	warned := map[string]bool{}
	for _, file := range plan.files {
		if file.kind == "note" {
			if parts, ok := engramNoteFileName(path.Base(file.rel)); ok {
				key := file.project + "\x00" + strings.Join(parts[:3], "-")
				if name, exists := blocked[key]; exists {
					if !warned[key] {
						warned[key] = true
						result.warn(fmt.Sprintf(
							"skipped %s: it holds notes this run would import again; move it aside and rerun with --repair-timestamps",
							path.Join(projectsDir, file.project, notesDir, name)))
					}
					continue
				}
			}
		}
		kept = append(kept, file)
	}
	plan.files = kept
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// repairEngramNoteFiles moves legacy daily engram files aside once their notes
// are reproduced by the per-timestamp plan. A legacy file that differs from the
// source is kept, and the day's planned files are dropped instead, so a repair
// can never import the same note twice. It runs before the write pass, so a
// conflicting plan still aborts with nothing changed.
func repairEngramNoteFiles(target string, plan *engramPlan, dryRun bool, result *Result) error {
	expected := engramPlannedSegments(plan)
	projects := map[string]struct{}{}
	for _, record := range plan.records {
		projects[record.project] = struct{}{}
	}
	keep := make(map[string]bool, len(plan.files))
	for _, file := range plan.files {
		keep[file.rel] = true
	}
	blocked := map[string]string{}
	for _, project := range sortedKeys(projects) {
		dirRel := path.Join(projectsDir, project, notesDir)
		entries, err := os.ReadDir(filepath.Join(target, filepath.FromSlash(dirRel)))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("migrate: scan %s: %w", dirRel, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.Type().IsRegular() || !legacyEngramNoteFile(name) {
				continue
			}
			rel := path.Join(dirRel, name)
			if keep[rel] {
				continue
			}
			parts, _ := engramNoteFileName(name)
			date := strings.Join(parts[:3], "-")
			owned, err := engramOwnedNoteFile(filepath.Join(target, filepath.FromSlash(rel)), project, date, expected)
			if err != nil {
				return err
			}
			if !owned {
				blocked[project+"\x00"+date] = name
				continue
			}
			aside := rel + ".replaced"
			if !dryRun {
				if err := os.Rename(
					filepath.Join(target, filepath.FromSlash(rel)),
					filepath.Join(target, filepath.FromSlash(aside)),
				); err != nil {
					return fmt.Errorf("migrate: set aside %s: %w", rel, err)
				}
			}
			result.Removed++
			result.Files = append(result.Files, FileResult{
				ProjectID: project, Kind: "note", TargetPath: rel, Status: StatusRemoved,
				Error: "legacy daily file replaced by per-timestamp note files; kept as " + aside,
			})
		}
	}
	dropBlockedDays(plan, blocked, result)
	return nil
}
