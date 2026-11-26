package app

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/textbudget"
	"github.com/levapveeskela/lv24-brain/internal/winproc"
)

// Context layer names, in packing order. The order is the priority: an
// earlier layer is filled before a later one and the core is never dropped.
const (
	ContextLayerCore      = "core"
	ContextLayerIdeas     = "ideas"
	ContextLayerDecisions = "decisions"
	ContextLayerAttempts  = "attempts"
	ContextLayerQuery     = "query"
	ContextLayerLinked    = "linked"
)

// contextIdeasCapTokens is the fixed cap of the bounded `brain open` ideas
// pointer (about 400 Latin characters). It is not a proportional share: the
// pointer is a small convenience layer, never a reason to starve decisions.
const contextIdeasCapTokens = 220

// Per-layer shares of the total token budget. The cap of a layer is
// proportional to the budget instead of a fixed number, so a 500-token answer
// and a 32000-token answer both spend their tokens where they matter. The
// shares sum to one; a layer may borrow the part of an earlier share that was
// left unused, which is what makes the caps fungible.
//
// Packing order is the priority: the query layer, the records that match the
// task, comes first after the core; the curated background and the 1-hop
// neighbours follow.
var contextLayerOrder = []struct {
	name  string
	share float64
}{
	{ContextLayerCore, 0.25},
	{ContextLayerQuery, 0.35},
	{ContextLayerDecisions, 0.20},
	{ContextLayerAttempts, 0.12},
	{ContextLayerLinked, 0.08},
}

// ContextSectionRecord is one record inside a packed layer. The fields are the
// metadata an agent needs to decide whether to expand the record; Detail is
// the one-line preview the Markdown summary renders and is not repeated in the
// MCP section metadata.
type ContextSectionRecord struct {
	ID          string
	Kind        string
	Title       string
	Status      string
	Path        string
	Detail      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Quarantined bool
	Tokens      int
	Score       float64
	// Paths are the repo-relative paths a decision governs.
	Paths []string
	// PathSignal is the changed-file bonus folded into Score, and
	// PathSignalMatch names the tier that earned it. Both are zero unless
	// ranking.paths is on.
	PathSignal      float64
	PathSignalMatch string
}

// ContextSection is one packed layer: its configured cap, the tokens it uses
// and the records that fit. Text carries the rendered prose of the core and
// ideas layers; the other layers are rendered from Records by section name.
type ContextSection struct {
	Name    string
	Cap     int
	Tokens  int
	Records []ContextSectionRecord
	Text    string
}

// Reason codes for records left out of a context answer. They are stable:
// the JSON excluded[].reason and the Markdown dropped line use them verbatim.
const (
	ContextExcludeOverBudget  = "over-budget"
	ContextExcludeLowerRank   = "lower-rank"
	ContextExcludeSuperseded  = "superseded"
	ContextExcludeStale       = "stale"
	ContextExcludeQuarantined = "quarantined"
	ContextExcludeOtherOwner  = "other-owner"
	// ContextExcludeOther is the fallback for a drop with no known code. A
	// current answer never produces it; it keeps an old truncation honest.
	ContextExcludeOther = "other"
	// contextNearMissLimit is the number of below-the-cut records an answer
	// keeps with their scores. More than three is noise for a reader.
	contextNearMissLimit = 3
)

// contextExcludeReasonOrder fixes the order reasons are printed in, so the
// dropped line is deterministic instead of following a map.
var contextExcludeReasonOrder = []string{
	ContextExcludeOverBudget, ContextExcludeLowerRank, ContextExcludeSuperseded,
	ContextExcludeStale, ContextExcludeQuarantined, ContextExcludeOtherOwner,
	ContextExcludeOther,
}

// ContextExclusion is one record left out of the answer, with the stable code
// that says why. Section names the layer that would have held it.
type ContextExclusion struct {
	ID      string
	Kind    string
	Section string
	Reason  string
	// Score is the ranking score at the moment the record was dropped. It is
	// zero for a record the filters rejected before ranking.
	Score float64
}

// ContextTruncation explains one record or block that did not fit. Reason is
// the human-readable detail and starts with the code, so a transport that only
// carries truncated[] can still name why the record is absent.
type ContextTruncation struct {
	ID      string
	Kind    string
	Section string
	Reason  string
	// Code is the stable reason: over-budget, lower-rank, superseded, stale,
	// quarantined or other-owner. It is empty for the core and summary blocks.
	Code  string
	Score float64
}

// ContextPack is the budget accounting of one context answer.
type ContextPack struct {
	// Budget is the total token budget that was applied.
	Budget int
	// TotalTokens is the sum of every section's tokens.
	TotalTokens int
	Sections    []ContextSection
	// Truncated is the single drop bookkeeping: filter drops carry the reason
	// they were rejected for, budget drops over-budget or lower-rank.
	Truncated []ContextTruncation
	// Quarantined is the number of ids excluded from trusted context.
	Quarantined int
	// PathSignal reports that ranking.paths was on and the checkout produced
	// a changed-file signal. PathChangedFiles and PathBranch describe it for
	// --explain; a checkout without git leaves PathSignal false.
	PathSignal       bool
	PathChangedFiles int
	PathBranch       string
}

// contextPackInput is what the packer needs to fill the layers.
type contextPackInput struct {
	Budget      int
	Header      string
	CoreText    string
	CoreRecords []ContextSectionRecord
	// Ideas is the already rendered `brain open` pointer block. It is prose,
	// not records, and is bounded by its own fixed cap.
	Ideas       string
	Decisions   []ContextRecord
	Attempts    []ContextRecord
	Query       []ContextRecord
	Quarantined int
	// Dropped are the records the filters left out before packing. They join
	// the truncation bookkeeping so the layers can report them.
	Dropped []ContextTruncation
	// QueryAlways renders the query layer even when it packs nothing: `brain
	// context` prints its headings and "none" lines in every answer, so their
	// cost must be part of the budget. Bootstrap has no query layer.
	QueryAlways bool
	// Linked is called only when the earlier layers left room. It returns the
	// 1-hop neighbours of the selected records and the neighbours it left out.
	Linked func(selected []string) ([]ContextRecord, []ContextTruncation, error)
}

// buildContextPack fills the layers in order under one total budget. A record
// is never split: a record that does not fit is skipped and listed in
// Truncated. The core layer is always present; only its prose is truncated,
// with a marker, when it alone exceeds its cap. Every other layer may borrow
// the share an earlier layer left unused, so a thin core does not waste the
// tokens it did not spend.
func buildContextPack(in contextPackInput) (ContextPack, error) {
	pack := ContextPack{Budget: in.Budget, Quarantined: in.Quarantined, Sections: []ContextSection{}}
	// The filters already dropped some records; they belong to the same
	// bookkeeping as the budget drops, so every layer can report them.
	pack.Truncated = append(pack.Truncated, in.Dropped...)
	remaining := in.Budget
	borrowed := 0

	// Core: state, latest handoff and findings. The header (project identity
	// and mode) counts against the core share too, so the rendered answer stays
	// inside the total budget.
	coreAllowance := minInt(contextLayerCap(ContextLayerCore, in.Budget), in.Budget)
	core := ContextSection{Name: ContextLayerCore, Cap: coreAllowance}
	headerTokens := textbudget.Tokens(in.Header)
	bodyBudget := coreAllowance - headerTokens
	coreText := ""
	truncated := in.CoreText != ""
	if bodyBudget > 0 {
		coreText, truncated = textbudget.Truncate(in.CoreText, bodyBudget)
	}
	if truncated {
		pack.Truncated = append(pack.Truncated, ContextTruncation{
			ID: "core", Section: ContextLayerCore,
			Reason: fmt.Sprintf("core needs more than %d tokens; prose truncated with a marker", coreAllowance),
		})
	}
	core.Text = coreText
	core.Records = in.CoreRecords
	core.Tokens = headerTokens + textbudget.Tokens(coreText)
	pack.Sections = append(pack.Sections, core)
	remaining -= core.Tokens
	borrowed = contextBorrowLeft(borrowed, coreAllowance, core.Tokens)
	if remaining < 0 {
		remaining = 0
	}

	selected := make(map[string]bool, 32)
	selectedOrder := make([]string, 0, 32)

	// Ideas: bounded prose, placed right after the core and before the ranked
	// layers. A pointer that does not fit is dropped whole; a half item line
	// is worse than no line.
	if text := strings.TrimSpace(in.Ideas); text != "" {
		allowance := minInt(contextIdeasCapTokens, remaining)
		tokens := textbudget.Tokens(text)
		if allowance > 0 && tokens <= allowance {
			pack.Sections = append(pack.Sections, ContextSection{
				Name: ContextLayerIdeas, Cap: contextIdeasCapTokens, Tokens: tokens, Text: text,
			})
			remaining -= tokens
			if remaining < 0 {
				remaining = 0
			}
			borrowed = contextBorrowLeft(borrowed, contextIdeasCapTokens, tokens)
		}
	}

	addLayer := func(name string, candidates []ContextRecord, always bool) {
		share := contextLayerCap(name, in.Budget)
		allowance := minInt(share+borrowed, remaining)
		if allowance < 0 {
			allowance = 0
		}
		sortContextCandidates(candidates)
		// The "… N more dropped (reason)" line is part of the layer, so its
		// room is reserved before the first record is packed. Records the
		// filters already left out count towards the same line.
		reserved := contextDroppedMarkerTokens(name, len(candidates)+contextDroppedCount(pack.Truncated, name))
		section := ContextSection{
			Name: name, Cap: allowance, Records: []ContextSectionRecord{},
			Tokens: contextSectionHeaderTokenCost(name),
		}
		omitted := false
		for _, record := range candidates {
			if record.ID == "" || selected[record.ID] {
				continue
			}
			tokens := contextRecordTokens(record)
			left := allowance - section.Tokens - reserved
			if tokens > left {
				code := ContextExcludeOverBudget
				detail := fmt.Sprintf("does not fit the %s layer: needs %d tokens, %d left", name, tokens, left)
				if omitted {
					// The first record that missed the cut ran out of room;
					// every later one is simply ranked below it.
					code = ContextExcludeLowerRank
					detail = fmt.Sprintf("ranked below the %s cut: needs %d tokens, %d left", name, tokens, left)
				} else if left <= 0 {
					detail = fmt.Sprintf("budget exhausted before the %s layer; needs %d tokens", name, tokens)
				}
				pack.Truncated = append(pack.Truncated, ContextTruncation{
					ID: record.ID, Kind: record.Kind, Section: name, Code: code,
					Reason: code + ": " + detail, Score: record.Score,
				})
				omitted = true
				continue
			}
			section.Records = append(section.Records, contextSectionRecord(record))
			section.Tokens += tokens
			selected[record.ID] = true
			selectedOrder = append(selectedOrder, record.ID)
		}
		marker := contextDroppedText(exclusionsFromTruncations(pack.Truncated), name)
		if len(section.Records) == 0 {
			dropped := contextDroppedCount(pack.Truncated, name)
			switch {
			case always:
				// The query headings and their "none" lines are printed by
				// every answer, even when nothing matched, so they cost budget.
				section.Tokens = contextQueryEmptyTokens()
			case dropped == 0:
				return
			default:
				// Nothing fit, but the layer must still report how many
				// records the budget left out.
				section.Tokens = contextSectionHeaderTokenCost(name)
			}
		}
		section.Tokens += textbudget.Tokens(marker)
		pack.Sections = append(pack.Sections, section)
		remaining -= section.Tokens
		if remaining < 0 {
			remaining = 0
		}
		borrowed = contextBorrowLeft(borrowed, share, section.Tokens)
	}

	addLayer(ContextLayerQuery, in.Query, in.QueryAlways)
	addLayer(ContextLayerDecisions, in.Decisions, false)
	addLayer(ContextLayerAttempts, in.Attempts, false)

	// The linked layer exists only when the earlier layers left room. Its
	// lookup is skipped entirely when there is none, so a tight budget costs
	// no extra store reads.
	if remaining > 0 && in.Linked != nil {
		linked, dropped, err := in.Linked(selectedOrder)
		if err != nil {
			return ContextPack{}, err
		}
		pack.Truncated = append(pack.Truncated, dropped...)
		addLayer(ContextLayerLinked, linked, false)
	}

	for _, section := range pack.Sections {
		pack.TotalTokens += section.Tokens
	}
	return pack, nil
}

// contextLayerCap is the proportional cap of one layer.
func contextLayerCap(name string, budget int) int {
	for _, layer := range contextLayerOrder {
		if layer.name != name {
			continue
		}
		capTokens := int(math.Round(layer.share * float64(budget)))
		if capTokens < 1 {
			capTokens = 1
		}
		return capTokens
	}
	return budget
}

// contextBorrowLeft tracks the budget earlier layers left unused. A layer that
// spends less than its share adds to the pool the later layers may borrow; a
// layer that spends more pays it back, never below zero.
func contextBorrowLeft(borrowed, share, spent int) int {
	if spent < share {
		return borrowed + (share - spent)
	}
	borrowed -= spent - share
	if borrowed < 0 {
		return 0
	}
	return borrowed
}

// contextDroppedMarkerTokens reserves room for the per-layer lines that report
// the records the layer left out. It is the worst case for the count that is
// known before packing; the exact text is accounted after the layer is filled.
func contextDroppedMarkerTokens(layer string, dropped int) int {
	text := fmt.Sprintf("- … %d more dropped (%s)\n", dropped, ContextExcludeOverBudget)
	if layer == ContextLayerQuery && dropped > 0 {
		text += fmt.Sprintf("- … %d lower-ranked omitted\n", minInt(dropped, contextNearMissLimit))
	}
	return textbudget.Tokens(text)
}

// contextDroppedText is the Markdown a layer prints for the records that are
// not in it: one line with the count and the distinct reasons, plus the
// near-miss line for the records that matched but ranked below the cut. The
// packer and the renderer share it, so the budget accounts for what is printed.
func contextDroppedText(excluded []ContextExclusion, layer string) string {
	count := 0
	reasons := make(map[string]bool, len(contextExcludeReasonOrder))
	near := 0
	for _, exclusion := range excluded {
		if exclusion.Section != layer {
			continue
		}
		count++
		reasons[exclusion.Reason] = true
		if layer == ContextLayerQuery && contextNearMissReason(exclusion.Reason) {
			near++
		}
	}
	if count == 0 {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "- … %d more dropped (%s)\n", count, strings.Join(contextOrderedReasons(reasons), ", "))
	if near > 0 {
		fmt.Fprintf(&out, "- … %d lower-ranked omitted\n", minInt(near, contextNearMissLimit))
	}
	return out.String()
}

// contextOrderedReasons sorts the distinct reasons of one layer into the fixed
// vocabulary order, with an unknown code last.
func contextOrderedReasons(found map[string]bool) []string {
	out := make([]string, 0, len(found))
	for _, reason := range contextExcludeReasonOrder {
		if found[reason] {
			out = append(out, reason)
		}
	}
	return out
}

// contextNearMissReason reports whether a drop is a ranked record that simply
// did not fit, which is what the near-miss bucket keeps.
func contextNearMissReason(reason string) bool {
	return reason == ContextExcludeOverBudget || reason == ContextExcludeLowerRank
}

// exclusionsFromTruncations projects the truncation bookkeeping into the
// record-level exclusion view. It skips the core and summary blocks and keeps
// the first reason per id, so one record is never listed twice.
func exclusionsFromTruncations(truncations []ContextTruncation) []ContextExclusion {
	seen := make(map[string]bool, len(truncations))
	out := make([]ContextExclusion, 0, len(truncations))
	for _, truncation := range truncations {
		if truncation.ID == "" || truncation.ID == "core" || truncation.ID == "summary" {
			continue
		}
		if seen[truncation.ID] {
			continue
		}
		seen[truncation.ID] = true
		out = append(out, ContextExclusion{
			ID: truncation.ID, Kind: truncation.Kind, Section: truncation.Section,
			Reason: contextExclusionCode(truncation.Code, truncation.Reason), Score: truncation.Score,
		})
	}
	return out
}

// contextExclusionCode prefers the explicit code and falls back to the prefix
// of the detail line, so a truncation written before the codes existed still
// yields a known reason.
func contextExclusionCode(code, detail string) string {
	if slices.Contains(contextExcludeReasonOrder, code) {
		return code
	}
	if prefix, _, ok := strings.Cut(detail, ": "); ok && slices.Contains(contextExcludeReasonOrder, prefix) {
		return prefix
	}
	return ContextExcludeOther
}

// contextDroppedCount is how many records of one layer the answer left out,
// whether a filter or the budget dropped them.
func contextDroppedCount(truncations []ContextTruncation, layer string) int {
	count := 0
	for _, truncation := range truncations {
		if truncation.Section == layer && truncation.ID != "core" && truncation.ID != "summary" {
			count++
		}
	}
	return count
}

// contextQueryEmptyTokens counts the headings and the two "none" lines the
// query layer prints when nothing matched.
func contextQueryEmptyTokens() int {
	return textbudget.Tokens("\n## Relevant records\n- none matched the task\n" +
		"\n## Failed attempts matching the task\n- none beyond the relevant records\n")
}

// contextSectionHeaderTokenCost counts the Markdown heading(s) a layer adds.
func contextSectionHeaderTokenCost(name string) int {
	switch name {
	case ContextLayerDecisions:
		return textbudget.Tokens("\n## Current decisions\n")
	case ContextLayerAttempts:
		return textbudget.Tokens("\n## Recent attempts\n")
	case ContextLayerQuery:
		return textbudget.Tokens("\n## Relevant records\n\n## Failed attempts matching the task\n")
	case ContextLayerLinked:
		return textbudget.Tokens("\n## Linked records\n")
	}
	return 0
}

// RankingPathsEnv is the environment switch of the ranking.paths feature. The
// feature is off by default: with no switch no git command runs and no score
// changes. The config key of the same feature is `ranking.paths`.
const RankingPathsEnv = "BRAIN_RANKING_PATHS"

// ContextPathSignalCap bounds the whole changed-file bonus. A record that only
// earns the signal can never beat a record with the same base score by more
// than this value, which is what keeps the signal additive and honest.
const ContextPathSignalCap = 0.05

// Tier shares of the cap. An exact changed file is stronger than a changed
// directory, which is stronger than a token shared with the branch name.
const (
	contextPathExactShare  = 1.0
	contextPathDirShare    = 0.6
	contextPathBranchShare = 0.25
)

// contextPathSignalTimeout bounds both git reads together, so a hung checkout
// can delay one context call by a couple of seconds, never longer.
const contextPathSignalTimeout = 2 * time.Second

// contextPathSignalMaxFiles bounds the changed-file set. A generated tree can
// list hundreds of thousands of untracked paths; the signal keeps a sample and
// never grows the context answer with it.
const contextPathSignalMaxFiles = 2000

// contextPathSignal is the read-only view of the checkout a context call ranks
// against: changed files, the directories that contain them, and the tokens of
// the current branch name.
type contextPathSignal struct {
	files        map[string]bool
	dirs         map[string]bool
	branchTokens map[string]bool
	branch       string
	changedCount int
}

// rankingPathsEnabled reports the ranking.paths switch. The environment value
// accepts the usual truthy spellings; anything else keeps the feature off.
func rankingPathsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RankingPathsEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// repoChangeSignal reads the changed files and the branch of one checkout. It
// is deliberately read-only and best-effort: a missing git, a directory that
// is not a repository, an unborn HEAD or a timeout all yield whatever partial
// information is available, never an error. Paths are normalized to the
// project checkout, not to the repository root, so they compare with the
// repo-relative paths a decision records.
func repoChangeSignal(ctx context.Context, dir string) ([]string, string) {
	runCtx, cancel := context.WithTimeout(ctx, contextPathSignalTimeout)
	defer cancel()
	run := func(args ...string) (string, bool) {
		cmd := exec.CommandContext(runCtx, "git", append([]string{"-C", dir}, args...)...)
		winproc.Hide(cmd)
		out, err := cmd.Output()
		if err != nil {
			return "", false
		}
		return string(out), true
	}
	prefix := ""
	if out, ok := run("rev-parse", "--show-prefix"); ok {
		prefix = strings.TrimSuffix(strings.TrimSpace(out), "/")
	}
	changed := make([]string, 0, 16)
	add := func(out string) {
		for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
			if len(changed) >= contextPathSignalMaxFiles {
				return
			}
			if path := contextRepoPath(line, prefix); path != "" {
				changed = append(changed, path)
			}
		}
	}
	if out, ok := run("diff", "--name-only", "HEAD"); ok {
		add(out)
	}
	if len(changed) < contextPathSignalMaxFiles {
		if out, ok := run("ls-files", "--others", "--exclude-standard"); ok {
			add(out)
		}
	}
	branch, _ := run("rev-parse", "--abbrev-ref", "HEAD")
	return changed, strings.TrimSpace(branch)
}

// contextRepoPath normalizes one git path to the project checkout and to the
// lowercase slash form the matcher compares. A path outside the checkout
// prefix (git reports repository-root paths) is dropped instead of guessed at.
func contextRepoPath(raw, prefix string) string {
	path := strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/")
	path = strings.TrimPrefix(path, "./")
	path = strings.Trim(path, "/")
	if path == "" || path == "." || path == ".." || strings.HasPrefix(path, "../") {
		return ""
	}
	prefix = strings.Trim(strings.ReplaceAll(strings.TrimSpace(prefix), "\\", "/"), "/")
	if prefix != "" {
		if path == prefix || !strings.HasPrefix(path, prefix+"/") {
			return ""
		}
		path = strings.TrimPrefix(path, prefix+"/")
	}
	return strings.ToLower(path)
}

// newContextPathSignal builds the matcher. nil means the feature has nothing
// to say: no changed files and no usable branch tokens.
func newContextPathSignal(changed []string, branch string) *contextPathSignal {
	files := make(map[string]bool, len(changed))
	dirs := make(map[string]bool, len(changed)*2)
	for _, raw := range changed {
		path := contextRepoPath(raw, "")
		if path == "" {
			continue
		}
		files[path] = true
		for dir := path; ; {
			index := strings.LastIndexByte(dir, '/')
			if index < 0 {
				break
			}
			dir = dir[:index]
			if dirs[dir] {
				break
			}
			dirs[dir] = true
		}
	}
	signal := &contextPathSignal{
		files: files, dirs: dirs, branch: strings.TrimSpace(branch), changedCount: len(files),
	}
	signal.branchTokens = contextBranchTokens(signal.branch)
	if len(files) == 0 && len(signal.branchTokens) == 0 {
		return nil
	}
	return signal
}

// contextBranchTokens splits a feature branch into comparable words. A default
// branch, a detached HEAD or an empty name contributes no token: those names
// say nothing about the task and would only add noise.
func contextBranchTokens(branch string) map[string]bool {
	branch = strings.ToLower(strings.TrimSpace(branch))
	switch branch {
	case "", "head", "main", "master", "trunk", "develop":
		return nil
	}
	tokens := make(map[string]bool, 4)
	for _, token := range strings.FieldsFunc(branch, contextPathTokenSep) {
		if len(token) >= 3 {
			tokens[token] = true
		}
	}
	if len(tokens) == 0 {
		return nil
	}
	return tokens
}

// contextPathTokenSep is the separator set of path and branch words: anything
// that is not a letter or a digit, so "internal/app/context.go" yields four
// words and "feat/ret-5" yields three.
func contextPathTokenSep(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// signal returns the capped bonus of one decision's paths and the tier that
// earned it. Tiers are exclusive in strength order: exact file, directory
// prefix, branch-name token. The caller adds the value to the base score; the
// value never exceeds ContextPathSignalCap.
func (s *contextPathSignal) signal(paths []string) (float64, string) {
	if s == nil || len(paths) == 0 {
		return 0, ""
	}
	best := 0.0
	bestMatch := ""
	for _, raw := range paths {
		path := contextRepoPath(raw, "")
		if path == "" {
			continue
		}
		switch {
		case s.files[path]:
			return roundContextScore(ContextPathSignalCap * contextPathExactShare), "exact " + path
		case s.dirs[path]:
			if bonus := roundContextScore(ContextPathSignalCap * contextPathDirShare); bonus > best {
				best, bestMatch = bonus, "dir "+path
			}
		case s.branchMatch(path):
			if bonus := roundContextScore(ContextPathSignalCap * contextPathBranchShare); bonus > best {
				best, bestMatch = bonus, "branch "+s.branch
			}
		}
	}
	return best, bestMatch
}

// branchMatch reports whether a decision path shares a word with the branch
// name, which is the weakest and last-resort tier of the signal.
func (s *contextPathSignal) branchMatch(path string) bool {
	if len(s.branchTokens) == 0 {
		return false
	}
	for _, token := range strings.FieldsFunc(path, contextPathTokenSep) {
		if s.branchTokens[token] {
			return true
		}
	}
	return false
}

// sortContextCandidates orders a layer by score, then update time, then id.
func sortContextCandidates(records []ContextRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Score != records[j].Score {
			return records[i].Score > records[j].Score
		}
		if !records[i].UpdatedAt.Equal(records[j].UpdatedAt) {
			return records[i].UpdatedAt.After(records[j].UpdatedAt)
		}
		return records[i].ID < records[j].ID
	})
}

func contextSectionRecord(record ContextRecord) ContextSectionRecord {
	return ContextSectionRecord{
		ID: record.ID, Kind: record.Kind, Title: record.Title, Status: record.Status,
		Path: record.Path, Detail: record.Detail, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		Quarantined: record.Quarantined, Tokens: contextRecordTokens(record), Score: record.Score,
		Paths: record.Paths, PathSignal: record.PathSignal, PathSignalMatch: record.PathSignalMatch,
	}
}

// contextRecordPath is the logical data-directory path of a record. It uses the
// record id as the file stem because the physical name also carries a slug;
// the id stays the stable, human-readable address.
func contextRecordPath(projectID, kind, id string) string {
	return recordDataPath(projectID, kind, id+".md")
}

// contextRecordTokens counts the Markdown line(s) one record adds. It is the
// same text writeContextRecords emits, so the budget and the answer agree.
func contextRecordTokens(record ContextRecord) int {
	return textbudget.Tokens(contextRecordText(record))
}

// sectionRecords converts a packed layer back to the record shape the renderer
// and the compatibility fields use.
func sectionRecords(pack ContextPack, name string) []ContextRecord {
	section, ok := contextSection(pack, name)
	if !ok {
		return nil
	}
	out := make([]ContextRecord, 0, len(section.Records))
	for _, record := range section.Records {
		out = append(out, ContextRecord{
			ID: record.ID, Kind: record.Kind, Title: record.Title, Status: record.Status,
			Detail: record.Detail, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
			Score: record.Score, Quarantined: record.Quarantined, Path: record.Path,
			Paths: record.Paths, PathSignal: record.PathSignal, PathSignalMatch: record.PathSignalMatch,
		})
	}
	return out
}

func contextSection(pack ContextPack, name string) (ContextSection, bool) {
	for _, section := range pack.Sections {
		if section.Name == name {
			return section, true
		}
	}
	return ContextSection{}, false
}

// ExplainLines is the human-readable budget table printed by
// `brain context --explain` and returned by brain_context with explain=true.
func (p ContextPack) ExplainLines() []string {
	lines := make([]string, 0, len(contextLayerOrder)+2)
	for _, layer := range contextLayerOrder {
		section, ok := contextSection(p, layer.name)
		tokens, records := 0, 0
		capTokens := contextLayerCap(layer.name, p.Budget)
		if ok {
			tokens, records = section.Tokens, len(section.Records)
			capTokens = section.Cap
		}
		lines = append(lines, fmt.Sprintf("%s: %d/%d tokens, %d records", layer.name, tokens, capTokens, records))
		if layer.name == ContextLayerCore {
			if ideas, ok := contextSection(p, ContextLayerIdeas); ok {
				lines = append(lines, fmt.Sprintf("%s: %d/%d tokens, prose", ContextLayerIdeas, ideas.Tokens, ideas.Cap))
			}
		}
	}
	lines = append(lines, fmt.Sprintf("total: %d/%d tokens, %d dropped", p.TotalTokens, p.Budget, len(p.Truncated)))
	if p.PathSignal {
		branch := p.PathBranch
		if branch == "" {
			branch = "unknown"
		}
		lines = append(lines, fmt.Sprintf("paths: on, %d changed files, branch %s", p.PathChangedFiles, branch))
		for _, section := range p.Sections {
			for _, record := range section.Records {
				if record.PathSignal <= 0 {
					continue
				}
				lines = append(lines, fmt.Sprintf("paths: %s +%.3f %s", record.ID, record.PathSignal, record.PathSignalMatch))
			}
		}
	}
	return lines
}

// ExplainText renders ExplainLines as one block.
func (p ContextPack) ExplainText() string {
	return strings.Join(p.ExplainLines(), "\n")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// contextTokenBudget resolves the token budget of one context call: an
// explicit value, then the configuration, then the documented default.
func (a *App) contextTokenBudget(requested int) (int, error) {
	if requested < 0 {
		return 0, domain.NewValidationError("context.budget", "must not be negative")
	}
	budget := requested
	if budget == 0 {
		budget = a.cfg.EffectiveContextBudget()
	}
	if budget <= 0 {
		budget = domain.DefaultContextBudgetTokens
	}
	if budget > domain.MaxContextBudgetTokens {
		return 0, domain.NewValidationError("context.budget",
			fmt.Sprintf("must be at most %d tokens", domain.MaxContextBudgetTokens))
	}
	return budget, nil
}

// packContext fills the layers for one task context. A failure in the optional
// linked lookup retries without that layer and returns the reason, so a link
// read problem never costs the curated answer.
func (a *App) packContext(
	ctx context.Context, result ContextResult, ranking *contextRanking,
	kinds []string, tokenBudget int, now time.Time,
) (ContextPack, error) {
	header := renderContextHeader(result, a.revision(ctx))
	coreText, coreRecords := renderContextCore(result, now)
	query := append(append([]ContextRecord{}, result.Records...), result.FailedAttempts...)
	exclude := recordIDs(query)

	var decisions, attempts []ContextRecord
	if slices.Contains(kinds, ContextKindDecision) {
		decisions = ranking.background(ContextKindDecision, exclude)
	}
	if slices.Contains(kinds, ContextKindAttempt) {
		attempts = ranking.background(ContextKindAttempt, exclude)
	}
	input := contextPackInput{
		Budget: tokenBudget, Header: header, CoreText: coreText, CoreRecords: coreRecords,
		Decisions: decisions, Attempts: attempts, Query: query, QueryAlways: true,
		Quarantined: len(result.Quarantined), Dropped: slices.Clone(ranking.dropped),
		Linked: func(selected []string) ([]ContextRecord, []ContextTruncation, error) {
			return a.contextLinkedCandidates(ctx, ranking, selected, kinds)
		},
	}
	pack, err := buildContextPack(input)
	if err == nil {
		return pack, nil
	}
	input.Linked = nil
	fallback, fallbackErr := buildContextPack(input)
	if fallbackErr != nil {
		return ContextPack{}, fallbackErr
	}
	return fallback, err
}

// background lists the current records of one kind for its curated layer.
// Decisions keep accepted ones first; attempts are ordered by update time by
// the packer. Records are read in id order so the exclusion bookkeeping is
// deterministic even though the catalog is a map.
func (r *contextRanking) background(kind string, exclude map[string]bool) []ContextRecord {
	candidates := make([]ContextRecord, 0, len(r.catalog.records))
	for _, record := range r.catalog.records {
		if record.Kind != kind || exclude[record.ID] {
			continue
		}
		candidates = append(candidates, record)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	layer := ContextLayerDecisions
	if kind == ContextKindAttempt {
		layer = ContextLayerAttempts
	}
	out := make([]ContextRecord, 0, len(candidates))
	for _, record := range candidates {
		if code, detail := r.admit(record); code != "" {
			r.drop(record, layer, code, detail)
			continue
		}
		record.Path = contextRecordPath(r.projectID, record.Kind, record.ID)
		if kind == ContextKindDecision {
			record.Score = decisionContextScore(record.Status, record.Pinned)
		}
		out = append(out, record)
	}
	return out
}

// contextLinkedCandidates returns the 1-hop neighbours of the selected records
// and the neighbour candidates it left out. Only references inside the same
// project resolve: a cross-project link would widen the answer beyond the
// curated project and its owner, so it is reported as other-owner. The target
// metadata comes from the catalog already loaded for this call, and the
// selected records themselves are read in one batch listing: one listing per
// selected record would make the layer cost N full parses of the project.
func (a *App) contextLinkedCandidates(
	ctx context.Context, ranking *contextRanking, selected []string, kinds []string,
) ([]ContextRecord, []ContextTruncation, error) {
	if len(selected) == 0 || len(ranking.catalog.records) == 0 {
		return nil, nil, nil
	}
	start := len(ranking.dropped)
	allowed := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = true
	}
	chosen := make(map[string]bool, len(selected))
	for _, id := range selected {
		chosen[id] = true
	}
	sources, err := a.RecordsByIDs(ctx, ranking.projectID, selected)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, nil, err
	}
	references := map[string]int{}
	for _, id := range selected {
		record, ok := sources[id]
		if !ok {
			// A record can be removed between the listing and the read; a
			// missing neighbour is not a reason to fail the context.
			continue
		}
		for _, raw := range recordRawLinks(record) {
			if raw.projectID != ranking.projectID {
				ranking.drop(ContextRecord{ID: raw.link.ID}, ContextLayerLinked, ContextExcludeOtherOwner,
					"the link points into another project, whose records this answer does not own")
				continue
			}
			if chosen[raw.link.ID] {
				continue
			}
			references[raw.link.ID]++
		}
	}
	ids := make([]string, 0, len(references))
	for id := range references {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ContextRecord, 0, len(ids))
	for _, id := range ids {
		count := references[id]
		record, ok := ranking.catalog.records[id]
		if !ok || !allowed[record.Kind] {
			continue
		}
		if code, detail := ranking.admit(record); code != "" {
			ranking.drop(record, ContextLayerLinked, code, detail)
			continue
		}
		record.Score = float64(count)
		out = append(out, record)
	}
	return out, slices.Clone(ranking.dropped[start:]), nil
}

// contextRecordFromRecord converts a loaded record to the context shape.
func contextRecordFromRecord(record Record) ContextRecord {
	summary := record.Summary()
	out := ContextRecord{
		ID: summary.ID, Kind: summary.Kind, Title: summary.Title, Status: summary.Status,
		CreatedAt: summary.CreatedAt, UpdatedAt: summary.UpdatedAt, ContentUpdatedAt: summary.ContentUpdatedAt,
		Validity: record.Validity(), Quarantined: recordQuarantined(record),
		Pinned: record.Pinned(),
	}
	out.Path = contextRecordPath(summary.ProjectID, summary.Kind, summary.ID)
	switch {
	case record.Note != nil:
		out.Detail = SummarizeLine(record.Note.Body, promptLineRunes)
	case record.Decision != nil:
		out.Detail = SummarizeLine(record.Decision.Context, promptLineRunes)
	case record.Attempt != nil:
		out.Detail = attemptDetail(*record.Attempt)
	case record.Handoff != nil:
		out.Detail = SummarizeLine(record.Handoff.Summary, promptLineRunes)
	}
	return out
}

func recordQuarantined(record Record) bool {
	switch {
	case record.Note != nil:
		return record.Note.Quarantined()
	case record.Decision != nil:
		return record.Decision.Quarantined()
	case record.Attempt != nil:
		return record.Attempt.Quarantined()
	case record.Handoff != nil:
		return record.Handoff.Quarantined()
	}
	return false
}

// decisionContextScore ranks current decisions: an accepted decision is
// pinned, a proposal is current but weaker, and the rest sit behind them.
// An explicitly pinned record ranks above its unpinned peers.
func decisionContextScore(status string, pinned bool) float64 {
	base := 0.25
	switch status {
	case string(domain.DecisionAccepted):
		base = 1
	case string(domain.DecisionProposed):
		base = 0.5
	case string(domain.DecisionSuperseded):
		base = 0.1
	}
	if pinned {
		base += 2
	}
	return base
}

// splitQuerySection splits the packed query layer back into ranked records and
// failed attempts, so the compatibility fields keep their meaning.
func splitQuerySection(pack ContextPack, failed map[string]bool) ([]ContextRecord, []ContextRecord) {
	packed := sectionRecords(pack, ContextLayerQuery)
	records := make([]ContextRecord, 0, len(packed))
	attempts := make([]ContextRecord, 0, len(packed))
	for _, record := range packed {
		if failed[record.ID] {
			attempts = append(attempts, record)
			continue
		}
		records = append(records, record)
	}
	return records, attempts
}

// fitContextPack renders the packed answer and drops records until the rendered
// text fits the token budget. The packer is conservative, but only the rendered
// text is authoritative: a dropped-record line, an empty-section placeholder or
// a rounding step can still push the estimate over. Dropping the lowest
// priority packed record keeps "never exceed the budget" true without lying
// about the sections.
func fitContextPack(result *ContextResult, revision string, now time.Time, failed map[string]bool) string {
	budget := result.Pack.Budget
	limit := fitAttempts(result.Pack) + 1
	for attempt := 0; attempt < limit; attempt++ {
		applyContextExclusions(result)
		summary := renderContext(*result, revision, now)
		if budget <= 0 || textbudget.Tokens(summary) <= budget {
			return summary
		}
		if !dropLowestPriorityRecord(&result.Pack) {
			// Even the fixed header does not fit; shorten the text itself so a
			// tiny budget is still honoured.
			applyContextExclusions(result)
			cut, truncated := textbudget.Truncate(summary, budget)
			if truncated {
				result.Pack.Truncated = append(result.Pack.Truncated, ContextTruncation{
					ID: "summary", Section: ContextLayerCore,
					Reason: "the fixed header alone exceeds the budget; the answer was cut",
				})
				result.Pack.TotalTokens = textbudget.Tokens(cut)
			}
			return cut
		}
		result.Records, result.FailedAttempts = splitQuerySection(result.Pack, failed)
	}
	applyContextExclusions(result)
	return renderContext(*result, revision, now)
}

// fitAttempts bounds the fitting loop by the number of packed records: each
// step drops exactly one.
func fitAttempts(pack ContextPack) int {
	total := 0
	for _, section := range pack.Sections {
		total += len(section.Records)
	}
	return total
}

// dropLowestPriorityRecord removes the last record of the least important
// packed layer and records why. It reports false when nothing is left to drop.
func dropLowestPriorityRecord(pack *ContextPack) bool {
	for _, name := range []string{ContextLayerLinked, ContextLayerAttempts, ContextLayerDecisions, ContextLayerQuery} {
		for index := range pack.Sections {
			section := &pack.Sections[index]
			if section.Name != name || len(section.Records) == 0 {
				continue
			}
			last := section.Records[len(section.Records)-1]
			section.Records = section.Records[:len(section.Records)-1]
			section.Tokens -= last.Tokens
			pack.TotalTokens -= last.Tokens
			if pack.TotalTokens < 0 {
				pack.TotalTokens = 0
			}
			pack.Truncated = append(pack.Truncated, ContextTruncation{
				ID: last.ID, Kind: last.Kind, Section: name, Code: ContextExcludeOverBudget,
				Reason: ContextExcludeOverBudget + ": dropped after rendering: the answer would have exceeded the token budget",
				Score:  last.Score,
			})
			return true
		}
	}
	return false
}
