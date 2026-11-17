package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/exchange"
)

func runExport(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("export", stderr)
	out := fs.String("out", "", "output directory or .zip")
	project := fs.String("project", "", "project id")
	since := fs.String("since", "", "only records updated since")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*out) == "" {
		fmt.Fprintln(stderr, "brain export: --out is required")
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "export", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "export", err)
	}
	defer func() { _ = runtime.Close() }()
	src, ok := runtime.App.Store().(exchange.Source)
	if !ok {
		fmt.Fprintln(stderr, "brain export: store does not support exchange")
		return 1
	}
	var sinceTime time.Time
	if strings.TrimSpace(*since) != "" {
		// reuse parseRecordSince? simple RFC3339 or duration not; try parse
		if t, err := time.Parse(time.RFC3339, *since); err == nil {
			sinceTime = t.UTC()
		}
	}
	ctx := context.Background()
	manifest, err := exchange.Export(ctx, src, exchange.ExportOptions{
		Out:     *out,
		Project: *project,
		Since:   sinceTime,
		JSON:    *asJSON,
	})
	if err != nil {
		return reportError(stderr, "export", err)
	}
	if *asJSON {
		fmt.Fprintf(stdout, "{\"created\":true,\"records\":%d}\n", manifest.Counts.Records)
	} else {
		fmt.Fprintf(stdout, "exported %d records, %d projects to %s\n", manifest.Counts.Records, manifest.Counts.Projects, *out)
	}
	return 0
}

func runImport(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("import", stderr)
	from := fs.String("from", "", "input directory or .zip")
	dryRun := fs.Bool("dry-run", false, "report without writing")
	asJSON := fs.Bool("json", false, "print JSON")
	createProjects := fs.Bool("create-projects", false, "create missing projects")
	rollback := fs.String("rollback", "", "rollback manifest")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*rollback) != "" {
		// basic: not implemented; delegate to message
		fmt.Fprintln(stderr, "brain import: --rollback is not supported in this build")
		return 2
	}
	if strings.TrimSpace(*from) == "" {
		fmt.Fprintln(stderr, "brain import: --from is required")
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "import", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "import", err)
	}
	defer func() { _ = runtime.Close() }()
	dst, ok := runtime.App.Store().(exchange.Source)
	if !ok {
		fmt.Fprintln(stderr, "brain import: store does not support exchange")
		return 1
	}
	ctx := context.Background()
	report, err := exchange.Import(ctx, dst, exchange.ImportOptions{
		From:           *from,
		DryRun:         *dryRun,
		JSON:           *asJSON,
		CreateProjects: *createProjects,
	})
	if err != nil {
		return reportError(stderr, "import", err)
	}
	if *asJSON {
		b, _ := json.Marshal(report)
		fmt.Fprintln(stdout, string(b))
	} else {
		fmt.Fprintf(stdout, "created %d\nunchanged %d\nconflicts %d\nquarantined %d\nskipped %d\n", report.Created, report.Unchanged, report.Conflicts, report.Quarantined, report.Skipped)
	}
	return 0
}
