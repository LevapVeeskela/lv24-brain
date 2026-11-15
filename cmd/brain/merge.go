package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// runRecordMerge implements the two-phase merge:
//
//	brain record merge <survivor> <duplicate>... --dry-run|--apply [--json]
//	brain record merge --rollback <manifest> [--json]
//
// Without --apply the command only plans, so an accidental run cannot edit
// records. --dry-run makes that explicit. --rollback restores a manifest byte
// for byte.
func runRecordMerge(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record merge", stderr)
	project := fs.String("project", "", "project id or alias")
	dryRun := fs.Bool("dry-run", false, "print the plan and write nothing")
	apply := fs.Bool("apply", false, "apply the plan")
	rollback := fs.String("rollback", "", "restore the state recorded in this merge manifest")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if *dryRun && *apply {
		fmt.Fprintln(stderr, "brain record merge: pass either --dry-run or --apply, not both")
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "record merge")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	if manifest := strings.TrimSpace(*rollback); manifest != "" {
		if len(positional) > 0 {
			fmt.Fprintln(stderr, "brain record merge: --rollback takes no record ids")
			return 2
		}
		return runRecordMergeRollback(ctx, runtime.App, target.Project.ID, manifest, *asJSON, stdout, stderr)
	}
	if len(positional) < 2 {
		fmt.Fprintln(stderr, "brain record merge: expected <survivor> <duplicate>... [--dry-run|--apply], or --rollback <manifest>")
		return 2
	}
	options := app.MergeOptions{
		ProjectID:  target.Project.ID,
		Survivor:   positional[0],
		Duplicates: positional[1:],
		DryRun:     *dryRun,
		Apply:      *apply,
	}
	result, err := runtime.App.Merge(ctx, options)
	if err != nil {
		if len(result.Plan.Conflicts) > 0 {
			printMergeConflicts(stderr, result.Plan.Conflicts)
		}
		return reportError(stderr, "record merge", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "record merge", toJSONMergeResult(result))
	}
	printMergePlan(stdout, result)
	if !result.Applied {
		fmt.Fprintln(stdout, "nothing written; re-run with --apply to merge")
	}
	return 0
}

func runRecordMergeRollback(ctx context.Context, a *app.App, projectID, manifest string, asJSON bool, stdout, stderr io.Writer) int {
	result, err := a.RollbackMerge(ctx, manifest)
	if err != nil {
		if len(result.Conflicts) > 0 {
			printMergeConflicts(stderr, result.Conflicts)
		}
		return reportError(stderr, "record merge --rollback", err)
	}
	if asJSON {
		return writeJSONReport(stdout, stderr, "record merge --rollback", toJSONMergeRollback(result))
	}
	if result.AlreadyRolledBack {
		fmt.Fprintf(stdout, "%s was already rolled back\n", result.Manifest)
		return 0
	}
	fmt.Fprintf(stdout, "rolled back %s (%d files restored", result.Manifest, len(result.Files))
	if result.Reindexed {
		fmt.Fprint(stdout, ", index rebuilt")
	}
	fmt.Fprintln(stdout, ")")
	return 0
}

func printMergeConflicts(w io.Writer, conflicts []string) {
	fmt.Fprintln(w, "conflicts:")
	for _, conflict := range conflicts {
		fmt.Fprintf(w, "  - %s\n", conflict)
	}
}

func printMergePlan(w io.Writer, result app.MergeResult) {
	plan := result.Plan
	fmt.Fprintf(w, "merge: %s %s absorbs %d\n", plan.Survivor.Kind, plan.Survivor.ID, len(plan.Duplicates))
	for _, duplicate := range plan.Duplicates {
		fmt.Fprintf(w, "  <- %s %s: %s\n", duplicate.Kind, duplicate.ID, duplicate.Title)
	}
	for _, already := range plan.AlreadyMerged {
		fmt.Fprintf(w, "  = %s %s: already merged\n", already.Kind, already.ID)
	}
	if len(plan.MergedFrom) > 0 {
		fmt.Fprintf(w, "merged_from: %s\n", strings.Join(plan.MergedFrom, ", "))
	}
	if len(plan.Links) > 0 {
		fmt.Fprintln(w, "rewrites:")
		for _, link := range plan.Links {
			fmt.Fprintf(w, "  - %s %s (%s): %s -> %s\n", link.SourceKind, link.SourceID, link.Field, link.Ref, link.NewID)
		}
	}
	if len(plan.Conflicts) > 0 {
		printMergeConflicts(w, plan.Conflicts)
	}
	if result.Applied {
		fmt.Fprintf(w, "applied; manifest: %s (%d files)\n", result.Manifest, len(result.Files))
	}
}

// jsonMergeRecord is one record of a merge plan.
type jsonMergeRecord struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
}

// jsonMergeLink is one rewritten reference.
type jsonMergeLink struct {
	SourceKind string `json:"source_kind"`
	SourceID   string `json:"source_id"`
	Field      string `json:"field"`
	Ref        string `json:"ref"`
	OldID      string `json:"old_id"`
	NewID      string `json:"new_id"`
}

// jsonMergeResult is `brain record merge --json`.
type jsonMergeResult struct {
	ProjectID     string            `json:"project_id"`
	Kind          string            `json:"kind"`
	Survivor      jsonMergeRecord   `json:"survivor"`
	Duplicates    []jsonMergeRecord `json:"duplicates"`
	AlreadyMerged []jsonMergeRecord `json:"already_merged"`
	Links         []jsonMergeLink   `json:"links"`
	MergedFrom    []string          `json:"merged_from"`
	Conflicts     []string          `json:"conflicts"`
	Fingerprint   string            `json:"fingerprint"`
	Changed       bool              `json:"changed"`
	Applied       bool              `json:"applied"`
	Manifest      string            `json:"manifest,omitempty"`
	Files         []jsonMergeFile   `json:"files"`
}

// jsonMergeFile is one file a merge touched.
type jsonMergeFile struct {
	Path   string `json:"path"`
	Change string `json:"change"`
}

func toJSONMergeResult(result app.MergeResult) jsonMergeResult {
	plan := result.Plan
	out := jsonMergeResult{
		ProjectID: plan.ProjectID, Kind: plan.Kind, Survivor: toJSONMergeRecord(plan.Survivor),
		Duplicates: toJSONMergeRecords(plan.Duplicates), AlreadyMerged: toJSONMergeRecords(plan.AlreadyMerged),
		Links: toJSONMergeLinks(plan.Links), MergedFrom: nonNilStrings(plan.MergedFrom),
		Conflicts: nonNilStrings(plan.Conflicts), Fingerprint: plan.Fingerprint,
		Changed: plan.Changed, Applied: result.Applied, Manifest: result.Manifest,
		Files: toJSONMergeFiles(result.Files),
	}
	return out
}

func toJSONMergeRecord(record ports.MergeRecord) jsonMergeRecord {
	return jsonMergeRecord{Kind: record.Kind, ID: record.ID, Title: record.Title, State: record.State}
}

func toJSONMergeRecords(records []ports.MergeRecord) []jsonMergeRecord {
	out := make([]jsonMergeRecord, 0, len(records))
	for _, record := range records {
		out = append(out, toJSONMergeRecord(record))
	}
	return out
}

func toJSONMergeLinks(links []ports.MergeLinkRewrite) []jsonMergeLink {
	out := make([]jsonMergeLink, 0, len(links))
	for _, link := range links {
		out = append(out, jsonMergeLink{
			SourceKind: link.SourceKind, SourceID: link.SourceID, Field: link.Field,
			Ref: link.Ref, OldID: link.OldID, NewID: link.NewID,
		})
	}
	return out
}

func toJSONMergeFiles(files []ports.MergeFile) []jsonMergeFile {
	out := make([]jsonMergeFile, 0, len(files))
	for _, file := range files {
		out = append(out, jsonMergeFile{Path: file.Path, Change: file.Change})
	}
	return out
}

// jsonMergeRollback is `brain record merge --rollback --json`.
type jsonMergeRollback struct {
	Manifest          string          `json:"manifest"`
	Files             []jsonMergeFile `json:"files"`
	Conflicts         []string        `json:"conflicts"`
	AlreadyRolledBack bool            `json:"already_rolled_back"`
	Reindexed         bool            `json:"reindexed"`
}

func toJSONMergeRollback(result app.MergeRollbackResult) jsonMergeRollback {
	return jsonMergeRollback{
		Manifest: result.Manifest, Files: toJSONMergeFiles(result.Files),
		Conflicts: nonNilStrings(result.Conflicts), AlreadyRolledBack: result.AlreadyRolledBack,
		Reindexed: result.Reindexed,
	}
}
