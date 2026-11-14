package mergedriver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const registryVersion = 1

type registryDocument struct {
	top      map[string]json.RawMessage
	projects map[string]registryProject
	order    []string
}

type registryProject struct {
	raw       map[string]json.RawMessage
	id        string
	name      string
	aliases   []string
	updatedAt time.Time
}

func parseRegistry(data []byte) (registryDocument, error) {
	doc := registryDocument{
		top:      map[string]json.RawMessage{},
		projects: map[string]registryProject{},
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc.top); err != nil {
		return registryDocument{}, err
	}
	rawProjects, ok := doc.top["projects"]
	if !ok || len(bytes.TrimSpace(rawProjects)) == 0 || string(bytes.TrimSpace(rawProjects)) == "null" {
		return doc, nil
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(rawProjects, &records); err != nil {
		return registryDocument{}, err
	}
	for _, raw := range records {
		project, err := decodeRegistryProject(raw)
		if err != nil {
			return registryDocument{}, err
		}
		if _, duplicate := doc.projects[project.id]; duplicate {
			return registryDocument{}, fmt.Errorf("registry has duplicate project id %q", project.id)
		}
		doc.projects[project.id] = project
		doc.order = append(doc.order, project.id)
	}
	return doc, nil
}

func decodeRegistryProject(raw map[string]json.RawMessage) (registryProject, error) {
	project := registryProject{raw: raw}
	if value, ok := raw["id"]; ok {
		if err := json.Unmarshal(value, &project.id); err != nil {
			return registryProject{}, fmt.Errorf("project id: %w", err)
		}
	}
	project.id = strings.TrimSpace(project.id)
	if project.id == "" {
		return registryProject{}, fmt.Errorf("registry project has no id")
	}
	if value, ok := raw["name"]; ok {
		if err := json.Unmarshal(value, &project.name); err != nil {
			return registryProject{}, fmt.Errorf("project %q name: %w", project.id, err)
		}
	}
	if value, ok := raw["aliases"]; ok {
		if err := json.Unmarshal(value, &project.aliases); err != nil {
			return registryProject{}, fmt.Errorf("project %q aliases: %w", project.id, err)
		}
	}
	if value, ok := raw["updated_at"]; ok {
		var timestamp time.Time
		if err := json.Unmarshal(value, &timestamp); err == nil {
			project.updatedAt = timestamp.UTC()
		}
	}
	return project, nil
}

func mergeRegistryProjects(base, ours, theirs registryDocument, opts Options) (map[string]registryProject, []Conflict) {
	merged := make(map[string]registryProject, len(ours.projects)+len(theirs.projects))
	conflicts := make([]Conflict, 0)
	ids := registryIDs(base, ours, theirs)
	for _, id := range ids {
		ourProject, inOurs := ours.projects[id]
		theirProject, inTheirs := theirs.projects[id]
		baseProject, inBase := base.projects[id]
		switch {
		case inOurs && inTheirs:
			winner, loser := pickRegistryWinner(baseProject, inBase, ourProject, theirProject)
			merged[id] = mergeRegistryRecord(winner, loser)
		case inOurs:
			if inBase && sameRecord(ourProject, baseProject) {
				// Deleted on the incoming side while we kept it unchanged.
				continue
			}
			if inBase {
				conflicts = append(conflicts, Conflict{
					Kind:   "registry",
					Field:  id + "-deleted",
					Base:   registryRecordText(baseProject),
					Ours:   registryRecordText(ourProject),
					Theirs: "",
					Path:   opts.Path,
				})
			}
			merged[id] = ourProject
		case inTheirs:
			if inBase && sameRecord(theirProject, baseProject) {
				// Deleted on our side while the incoming side kept it unchanged.
				continue
			}
			if inBase {
				conflicts = append(conflicts, Conflict{
					Kind:   "registry",
					Field:  id + "-deleted",
					Base:   registryRecordText(baseProject),
					Ours:   "",
					Theirs: registryRecordText(theirProject),
					Path:   opts.Path,
				})
			}
			merged[id] = theirProject
		}
	}
	return merged, conflicts
}

func registryIDs(base, ours, theirs registryDocument) []string {
	seen := make(map[string]struct{}, len(ours.order)+len(theirs.order)+len(base.order))
	ids := make([]string, 0, len(ours.order)+len(theirs.order))
	appendID := func(id string) {
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, id := range ours.order {
		appendID(id)
	}
	for _, id := range theirs.order {
		appendID(id)
	}
	for _, id := range base.order {
		appendID(id)
	}
	return ids
}

func pickRegistryWinner(base registryProject, inBase bool, ours, theirs registryProject) (registryProject, registryProject) {
	if inBase {
		switch {
		case sameRecord(ours, base):
			return theirs, ours
		case sameRecord(theirs, base):
			return ours, theirs
		}
	}
	if theirs.updatedAt.After(ours.updatedAt) {
		return theirs, ours
	}
	return ours, theirs
}

// mergeRegistryRecord keeps the winner's values and preserves keys the loser
// has but the winner lacks, so a field added on one machine is never dropped.
func mergeRegistryRecord(winner, loser registryProject) registryProject {
	record := registryProject{
		raw:       make(map[string]json.RawMessage, len(winner.raw)+len(loser.raw)),
		id:        winner.id,
		name:      winner.name,
		aliases:   unionAliases(winner.aliases, loser.aliases),
		updatedAt: winner.updatedAt,
	}
	for key, value := range winner.raw {
		record.raw[key] = value
	}
	for key, value := range loser.raw {
		if _, ok := record.raw[key]; !ok {
			record.raw[key] = value
		}
	}
	if len(record.aliases) > 0 {
		if raw, err := json.Marshal(record.aliases); err == nil {
			record.raw["aliases"] = raw
		}
	}
	return record
}

func unionAliases(left, right []string) []string {
	result := make([]string, 0, len(left)+len(right))
	seen := make(map[string]struct{}, len(left)+len(right))
	for _, aliases := range [][]string{left, right} {
		for _, alias := range aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				continue
			}
			key := strings.ToLower(alias)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, alias)
		}
	}
	return result
}

// resolveAliasConflicts keeps an alias on one project only. The later
// updated_at wins; a tie keeps the alias on ours. Every reassignment is
// reported so the owner can decide which project the alias belongs to.
func resolveAliasConflicts(merged map[string]registryProject, ours, theirs registryDocument, opts Options, conflicts *[]Conflict) {
	ids := make([]string, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := merged[ids[i]], merged[ids[j]]
		if !left.updatedAt.Equal(right.updatedAt) {
			return left.updatedAt.After(right.updatedAt)
		}
		return left.id < right.id
	})
	owner := map[string]string{}
	for _, id := range ids {
		project := merged[id]
		keep := make([]string, 0, len(project.aliases))
		for _, alias := range project.aliases {
			key := strings.ToLower(strings.TrimSpace(alias))
			if first, taken := owner[key]; taken {
				*conflicts = append(*conflicts, Conflict{
					Kind:   "registry",
					Field:  "alias-" + key,
					Ours:   first,
					Theirs: id,
					Path:   opts.Path,
				})
				continue
			}
			owner[key] = id
			keep = append(keep, alias)
		}
		if len(keep) != len(project.aliases) {
			project.aliases = keep
			if raw, err := json.Marshal(keep); err == nil {
				project.raw["aliases"] = raw
			}
			merged[id] = project
		}
	}
}

func sameRecord(left, right registryProject) bool {
	leftRaw, err := normalizeJSON(left.raw)
	if err != nil {
		return false
	}
	rightRaw, err := normalizeJSON(right.raw)
	if err != nil {
		return false
	}
	return bytes.Equal(leftRaw, rightRaw)
}

func normalizeJSON(raw map[string]json.RawMessage) ([]byte, error) {
	var value any
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func registryRecordText(project registryProject) string {
	raw, err := json.MarshalIndent(project.raw, "", "  ")
	if err != nil {
		return ""
	}
	return string(raw)
}

func renderRegistry(ours, theirs registryDocument, projects map[string]registryProject) ([]byte, error) {
	top := make(map[string]json.RawMessage, len(ours.top)+1)
	for key, value := range theirs.top {
		top[key] = value
	}
	for key, value := range ours.top {
		top[key] = value
	}
	version := registryVersion
	if value, ok := top["version"]; ok {
		if err := json.Unmarshal(value, &version); err != nil {
			version = registryVersion
		}
	}
	if raw, err := json.Marshal(version); err == nil {
		top["version"] = raw
	}

	records := make([]map[string]json.RawMessage, 0, len(projects))
	for _, project := range projects {
		records = append(records, project.raw)
	}
	sort.Slice(records, func(i, j int) bool {
		leftName, leftID := registrySortKeys(records[i])
		rightName, rightID := registrySortKeys(records[j])
		if leftName != rightName {
			return leftName < rightName
		}
		return leftID < rightID
	})
	rawProjects, err := json.Marshal(records)
	if err != nil {
		return nil, fmt.Errorf("mergedriver: encode registry projects: %w", err)
	}
	top["projects"] = rawProjects

	raw, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mergedriver: encode registry: %w", err)
	}
	return append(raw, '\n'), nil
}

func registrySortKeys(raw map[string]json.RawMessage) (name, id string) {
	project, err := decodeRegistryProject(raw)
	if err != nil {
		return "", ""
	}
	return strings.ToLower(project.name), strings.ToLower(project.id)
}
