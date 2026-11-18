package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Duplicate cluster reasons. They say which detector found the cluster, so a
// person can judge how much text the members actually share.
const (
	// DuplicateReasonExact marks records whose normalized text is identical
	// (the SHA-256 fingerprint matches).
	DuplicateReasonExact = "exact"
	// DuplicateReasonSimhash marks records within the documented simhash
	// Hamming distance.
	DuplicateReasonSimhash = "simhash"
	// DuplicateReasonShingles marks records within the documented shingle
	// (Jaccard) similarity.
	DuplicateReasonShingles = "shingles"
)

// simhashBands splits the 64-bit sketch into equal bands for candidate
// generation. Two sketches within 3 bits must agree on at least one of four
// bands, so banding never hides a reportable pair.
const simhashBands = 4

// DuplicateMember is one record of a duplicate cluster.
type DuplicateMember struct {
	ID        string
	Title     string
	CreatedAt time.Time
}

// DuplicateCluster is a group of records of the same kind that say the same
// thing. Nothing is merged: the cluster is a report and a person or agent picks
// the survivor.
type DuplicateCluster struct {
	ProjectID  string
	Kind       string
	Reason     string
	Similarity float64
	Members    []DuplicateMember
}

// DuplicateReport is the `brain duplicates` result.
type DuplicateReport struct {
	// Clusters are ordered by project, kind and the oldest member id.
	Clusters []DuplicateCluster
	// Exact and Near count the clusters, not the records.
	Exact int
	Near  int
	// Projects is how many projects were inspected.
	Projects int
}

// DuplicateOptions selects what `brain duplicates` inspects.
type DuplicateOptions struct {
	// ProjectID limits the report to one project; empty selects the working
	// directory's project unless Scan is set.
	ProjectID string
	// Scan inspects every registered project instead of one.
	Scan bool
}

// Duplicates reports exact and near-duplicate clusters. It never edits, merges
// or trashes a record; the ids are what a person needs to choose.
func (a *App) Duplicates(ctx context.Context, opts DuplicateOptions) (DuplicateReport, error) {
	if err := a.requireStore(); err != nil {
		return DuplicateReport{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var projects []domain.Project
	if opts.Scan {
		listed, err := a.deps.Store.ListProjects(ctx)
		if err != nil {
			return DuplicateReport{}, err
		}
		projects = listed
	} else {
		project, err := a.ensureProject(ctx, opts.ProjectID, false)
		if err != nil {
			return DuplicateReport{}, err
		}
		projects = []domain.Project{project}
	}
	report := DuplicateReport{Projects: len(projects)}
	for _, project := range projects {
		in, err := a.duplicateInputs(ctx, project.ID)
		if err != nil {
			return DuplicateReport{}, fmt.Errorf("%s: %w", project.ID, err)
		}
		clusters := duplicatesOfProject(project.ID, in)
		for _, cluster := range clusters {
			switch cluster.Reason {
			case DuplicateReasonExact:
				report.Exact++
			default:
				report.Near++
			}
		}
		report.Clusters = append(report.Clusters, clusters...)
	}
	sort.SliceStable(report.Clusters, func(i, j int) bool {
		left, right := report.Clusters[i], report.Clusters[j]
		if left.ProjectID != right.ProjectID {
			return left.ProjectID < right.ProjectID
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Members[0].ID < right.Members[0].ID
	})
	return report, nil
}

// duplicatesOfProject clusters the records of one project's duplicate inputs.
func duplicatesOfProject(projectID string, in reviewInputs) []DuplicateCluster {
	var clusters []DuplicateCluster
	clusters = append(clusters, duplicateClustersOfKind(projectID, ports.RecordNote, noteDupRecords(in.notes))...)
	clusters = append(clusters, duplicateClustersOfKind(projectID, ports.RecordDecision, decisionDupRecords(in.decisions))...)
	clusters = append(clusters, duplicateClustersOfKind(projectID, ports.RecordAttempt, attemptDupRecords(in.attempts))...)
	clusters = append(clusters, duplicateClustersOfKind(projectID, ports.RecordHandoff, handoffDupRecords(in.handoffs))...)
	return clusters
}

// duplicateClustersOfKind groups one kind's records into exact clusters and
// near clusters. Records already reported in an exact group are not repeated
// as near duplicates, so each record appears in the smallest useful group.
func duplicateClustersOfKind(projectID, kind string, records []dupRecord) []DuplicateCluster {
	records = recentDupRecords(records)
	if len(records) < 2 {
		return nil
	}
	clusters, singles := exactDuplicateClusters(projectID, kind, records)
	clusters = append(clusters, nearDuplicateClusters(projectID, kind, singles)...)
	return clusters
}

// exactDuplicateClusters returns the fingerprint groups with more than one
// member and the records that are alone in their group.
func exactDuplicateClusters(projectID, kind string, records []dupRecord) ([]DuplicateCluster, []dupRecord) {
	groups := make(map[string][]dupRecord, len(records))
	order := make([]string, 0, len(records))
	for _, record := range records {
		if record.text == "" {
			continue
		}
		if _, seen := groups[record.fingerprint]; !seen {
			order = append(order, record.fingerprint)
		}
		groups[record.fingerprint] = append(groups[record.fingerprint], record)
	}
	var clusters []DuplicateCluster
	var singles []dupRecord
	for _, fingerprint := range order {
		group := groups[fingerprint]
		if len(group) < 2 {
			singles = append(singles, group...)
			continue
		}
		clusters = append(clusters, DuplicateCluster{
			ProjectID:  projectID,
			Kind:       kind,
			Reason:     DuplicateReasonExact,
			Similarity: 1,
			Members:    duplicateMembers(group),
		})
	}
	return clusters, singles
}

// duplicatePairFinding is one pair the near-detectors linked.
type duplicatePairFinding struct {
	older int
	score float64
	why   string
}

// nearDuplicateClusters links the records that either the simhash or the
// shingle detector pairs, then reports each connected group once.
func nearDuplicateClusters(projectID, kind string, records []dupRecord) []DuplicateCluster {
	if len(records) < 2 {
		return nil
	}
	candidates := dupCandidates(records)
	addSimhashCandidates(records, candidates)
	parent := make([]int, len(records))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		rootA, rootB := find(a), find(b)
		if rootA != rootB {
			parent[rootB] = rootA
		}
	}
	var pairs []duplicatePairFinding
	for newer := 1; newer < len(records); newer++ {
		for _, older := range candidates[newer] {
			score, why, ok := duplicatePair(records[older], records[newer])
			if !ok {
				continue
			}
			pairs = append(pairs, duplicatePairFinding{older: older, score: score, why: why})
			union(older, newer)
		}
	}
	best := map[int]float64{}
	reason := map[int]string{}
	for _, pair := range pairs {
		root := find(pair.older)
		if pair.score > best[root] {
			best[root] = pair.score
		}
		// The shingle detector says more about the text than the sketch, so a
		// group that matched both is reported as a shingle match.
		if pair.why == DuplicateReasonShingles || reason[root] == "" {
			reason[root] = pair.why
		}
	}
	grouped := map[int][]dupRecord{}
	order := make([]int, 0)
	for index, record := range records {
		root := find(index)
		if _, seen := grouped[root]; !seen {
			order = append(order, root)
		}
		grouped[root] = append(grouped[root], record)
	}
	var clusters []DuplicateCluster
	for _, root := range order {
		group := grouped[root]
		if len(group) < 2 {
			continue
		}
		why := reason[root]
		if why == "" {
			why = DuplicateReasonShingles
		}
		clusters = append(clusters, DuplicateCluster{
			ProjectID:  projectID,
			Kind:       kind,
			Reason:     why,
			Similarity: best[root],
			Members:    duplicateMembers(group),
		})
	}
	return clusters
}

// duplicatePair scores two records with both near-duplicate detectors and
// reports which one matched. Records whose normalized text is identical are
// exact duplicates and belong to an exact cluster instead.
func duplicatePair(a, b dupRecord) (float64, string, bool) {
	if a.text == "" || b.text == "" || a.fingerprint == b.fingerprint {
		return 0, "", false
	}
	if !nearDuplicateGate(a, b) {
		return 0, "", false
	}
	if a.words >= domain.SimhashMinWords && b.words >= domain.SimhashMinWords {
		distance := domain.HammingDistance(a.simhash, b.simhash)
		if distance <= domain.NearDuplicateHammingDistance {
			return 1 - float64(distance)/64, DuplicateReasonSimhash, true
		}
	}
	if score, ok := dupSimilarity(a, b); ok {
		return score, DuplicateReasonShingles, true
	}
	return 0, "", false
}

// addSimhashCandidates adds the older records that share a simhash band. Two
// sketches within the documented Hamming distance always share a band, so this
// keeps the comparison bounded without hiding a pair.
func addSimhashCandidates(records []dupRecord, candidates map[int][]int) {
	bands := make(map[uint64][]int, len(records))
	width := 64 / simhashBands
	mask := uint64(1)<<uint(width) - 1
	for index, record := range records {
		if record.words < domain.SimhashMinWords {
			continue
		}
		for band := 0; band < simhashBands; band++ {
			key := (record.simhash >> uint(band*width)) & mask
			key = key | uint64(band)<<uint(64-width)
			for _, older := range bands[key] {
				candidates[index] = appendCandidate(candidates[index], older)
			}
			bands[key] = append(bands[key], index)
		}
	}
}

func appendCandidate(list []int, index int) []int {
	for _, existing := range list {
		if existing == index {
			return list
		}
	}
	return append(list, index)
}

// duplicateMembers orders a cluster oldest first, so the first id is the record
// a person most likely keeps.
func duplicateMembers(records []dupRecord) []DuplicateMember {
	ordered := append([]dupRecord(nil), records...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].created.Equal(ordered[j].created) {
			return ordered[i].created.Before(ordered[j].created)
		}
		return ordered[i].id < ordered[j].id
	})
	members := make([]DuplicateMember, 0, len(ordered))
	for _, record := range ordered {
		members = append(members, DuplicateMember{ID: record.id, Title: record.display, CreatedAt: record.created})
	}
	return members
}

// recordDedupText returns the normalized-exact text two records of one kind
// must share to be duplicates. It is the curated claim, not its metadata:
// evidence, paths, tags and provenance may legitimately differ.
func recordDedupText(texts ...string) string {
	return strings.Join(texts, "\n")
}

// duplicateCandidate is one live record prepared for the write-time check.
type duplicateCandidate struct {
	id    string
	title string
	hash  string
}

// noteDuplicateCandidates prepares the live notes of one project.
func noteDuplicateCandidates(notes []domain.Note) []duplicateCandidate {
	out := make([]duplicateCandidate, 0, len(notes))
	for _, note := range notes {
		if !note.Validity.IsCurrent() {
			continue
		}
		out = append(out, duplicateCandidate{
			id: note.ID, title: note.Title,
			hash: domain.BodyFingerprint(recordDedupText(note.Title, note.Body)),
		})
	}
	return out
}

// decisionDuplicateCandidates prepares the current decisions of one project.
func decisionDuplicateCandidates(decisions []domain.Decision) []duplicateCandidate {
	out := make([]duplicateCandidate, 0, len(decisions))
	for _, decision := range decisions {
		if decision.Status == domain.DecisionRejected || decision.Status == domain.DecisionSuperseded || !decision.Validity.IsCurrent() {
			continue
		}
		out = append(out, duplicateCandidate{
			id: decision.ID, title: decision.Title,
			hash: domain.BodyFingerprint(decisionDedupText(decision)),
		})
	}
	return out
}

// attemptDuplicateCandidates prepares the current attempts of one project.
func attemptDuplicateCandidates(attempts []domain.Attempt) []duplicateCandidate {
	out := make([]duplicateCandidate, 0, len(attempts))
	for _, attempt := range attempts {
		if !attempt.Validity.IsCurrent() {
			continue
		}
		out = append(out, duplicateCandidate{
			id: attempt.ID, title: attempt.Title,
			hash: domain.BodyFingerprint(attemptDedupText(attempt)),
		})
	}
	return out
}

// handoffDuplicateCandidates prepares the current handoffs of one project.
func handoffDuplicateCandidates(handoffs []domain.Handoff) []duplicateCandidate {
	out := make([]duplicateCandidate, 0, len(handoffs))
	for _, handoff := range handoffs {
		if !handoff.Validity.IsCurrent() {
			continue
		}
		out = append(out, duplicateCandidate{
			id: handoff.ID, title: SummarizeLine(handoff.Summary, 80),
			hash: domain.BodyFingerprint(handoffDedupText(handoff)),
		})
	}
	return out
}

func decisionDedupText(decision domain.Decision) string {
	return recordDedupText(decision.Title, decision.Context,
		strings.Join(decision.Alternatives, " "), strings.Join(decision.Consequences, " "))
}

func attemptDedupText(attempt domain.Attempt) string {
	return recordDedupText(attempt.Title, attempt.Hypothesis, attempt.Result, attempt.Reason)
}

func handoffDedupText(handoff domain.Handoff) string {
	return recordDedupText(handoff.Summary, handoff.NextAction)
}

// findExactDuplicate returns the existing record with the same fingerprint.
func findExactDuplicate(candidates []duplicateCandidate, text string) (duplicateCandidate, bool) {
	hash := domain.BodyFingerprint(text)
	for _, candidate := range candidates {
		if candidate.hash == hash {
			return candidate, true
		}
	}
	return duplicateCandidate{}, false
}
