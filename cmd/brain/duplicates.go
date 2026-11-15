package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
)

// runDuplicates reports exact and near-duplicate records. It never merges or
// trashes anything: the ids are what a person needs to choose a survivor.
func runDuplicates(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("duplicates", stderr)
	project := fs.String("project", "", "project id or alias")
	scan := fs.Bool("scan", false, "scan every registered project instead of one")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if *scan && (len(positional) > 0 || strings.TrimSpace(*project) != "") {
		fmt.Fprintln(stderr, "brain duplicates: --scan inspects every registered project; drop the project argument")
		return 2
	}
	ctx := context.Background()
	var runtime *bootstrap.Runtime
	var projectID string
	if *scan {
		cfg, err := loadRequiredConfig(*cfgPath)
		if err != nil {
			return reportError(stderr, "duplicates", err)
		}
		runtime, err = bootstrap.Build(cfg)
		if err != nil {
			return reportError(stderr, "duplicates", err)
		}
	} else {
		var target app.Resolved
		var code int
		runtime, target, code = stateTarget(ctx, *cfgPath, *project, positional, stderr, "duplicates")
		if code != 0 {
			return code
		}
		projectID = target.Project.ID
	}
	defer func() { _ = runtime.Close() }()
	report, err := runtime.App.Duplicates(ctx, app.DuplicateOptions{ProjectID: projectID, Scan: *scan})
	if err != nil {
		return reportError(stderr, "duplicates", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "duplicates", toJSONDuplicates(report, *scan))
	}
	printDuplicates(stdout, report, *scan)
	return 0
}

// printDuplicates renders the clusters, exact groups first within each kind.
func printDuplicates(w io.Writer, report app.DuplicateReport, scan bool) {
	scope := "every project"
	if !scan {
		scope = "one project"
	}
	fmt.Fprintf(w, "# Duplicates — %s\n", scope)
	fmt.Fprintf(w, "projects: %d, exact clusters: %d, near clusters: %d\n", report.Projects, report.Exact, report.Near)
	if len(report.Clusters) == 0 {
		fmt.Fprintln(w, "no exact or near-duplicate records")
		return
	}
	for _, cluster := range report.Clusters {
		ids := make([]string, 0, len(cluster.Members))
		titles := make([]string, 0, len(cluster.Members))
		for _, member := range cluster.Members {
			ids = append(ids, member.ID)
			if member.Title != "" {
				titles = append(titles, member.Title)
			}
		}
		detail := ""
		if len(titles) > 0 {
			detail = ": " + app.SummarizeLine(titles[0], 80)
		}
		if cluster.Reason == app.DuplicateReasonExact {
			fmt.Fprintf(w, "- [exact] %s %s (%s)%s\n", cluster.ProjectID, cluster.Kind, strings.Join(ids, ", "), detail)
			continue
		}
		fmt.Fprintf(w, "- [near %d%%] %s %s (%s)%s\n", int(cluster.Similarity*100), cluster.ProjectID, cluster.Kind,
			strings.Join(ids, ", "), detail)
	}
}
