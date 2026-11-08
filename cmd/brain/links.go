package main

import (
	"context"
	"fmt"
	"io"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/mcpserver"
)

// runLinks implements `brain links`: the [[id]] links and backlinks of one
// record, or every broken link of a project.
func runLinks(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("links", stderr)
	project := fs.String("project", "", "project id or alias")
	broken := fs.Bool("broken", false, "list links to records or projects that do not exist")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage:
  brain links <id> [--project p] [--json]
  brain links --broken [--project p] [--json]

Records reference each other with [[<id>]], [[<id>|text]] or, across
projects, [[<project>/<id>]]. Backlinks come from the same project.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	want, usage := 1, "<id>"
	if *broken {
		want, usage = 0, "no arguments with --broken"
	}
	if !expectArgs("links", positional, want, usage, stderr) {
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "links")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	if *broken {
		return printBrokenLinks(ctx, runtime.App, target.Project.ID, *asJSON, stdout, stderr)
	}
	links, err := runtime.App.RecordLinks(ctx, target.Project.ID, positional[0])
	if err != nil {
		return reportError(stderr, "links", err)
	}
	summary := toRecordSummary(links.Record.Summary())
	out := jsonLinks{Record: summary, Links: toJSONLinks(links.Links), Backlinks: toJSONBacklinks(links.Backlinks)}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "links", out)
	}
	fmt.Fprintln(stdout, recordListLine(summary))
	printLinkSections(stdout, out.Links, out.Backlinks)
	return 0
}

func printBrokenLinks(ctx context.Context, uc *app.App, projectID string, asJSON bool, stdout, stderr io.Writer) int {
	broken, err := uc.BrokenLinks(ctx, projectID)
	if err != nil {
		return reportError(stderr, "links", err)
	}
	out := make([]jsonBrokenLink, 0, len(broken))
	for _, item := range broken {
		out = append(out, jsonBrokenLink{Source: toRecordSummary(item.Source), Link: toJSONLink(item.Link)})
	}
	if asJSON {
		return writeJSONReport(stdout, stderr, "links", jsonBrokenLinks{ProjectID: projectID, Broken: out})
	}
	if len(out) == 0 {
		fmt.Fprintf(stdout, "no broken links in %s\n", projectID)
		return 0
	}
	for _, item := range out {
		fmt.Fprintf(stdout, "%s %s -> [[%s]] in %s: %s\n", item.Source.Kind, item.Source.ID, item.Link.Ref, item.Link.Field, item.Link.Detail)
	}
	fmt.Fprintf(stdout, "%d broken links in %s\n", len(out), projectID)
	return 0
}

// printLinkSections renders the Links and Backlinks sections of a record.
func printLinkSections(w io.Writer, links []mcpserver.RecordLink, backlinks []mcpserver.RecordBacklink) {
	fmt.Fprintln(w, "Links:")
	if len(links) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, link := range links {
		fmt.Fprintf(w, "  - %s (in %s)\n", mcpserver.LinkLine(link), link.Field)
	}
	fmt.Fprintln(w, "Backlinks:")
	if len(backlinks) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, backlink := range backlinks {
		fmt.Fprintf(w, "  - %s\n", mcpserver.BacklinkLine(backlink))
	}
}

// jsonLinks is `brain links <id> --json`.
type jsonLinks struct {
	Record    mcpserver.RecordSummary    `json:"record"`
	Links     []mcpserver.RecordLink     `json:"links"`
	Backlinks []mcpserver.RecordBacklink `json:"backlinks"`
}

// jsonBrokenLinks is `brain links --broken --json`.
type jsonBrokenLinks struct {
	ProjectID string           `json:"project_id"`
	Broken    []jsonBrokenLink `json:"broken"`
}

type jsonBrokenLink struct {
	Source mcpserver.RecordSummary `json:"source"`
	Link   mcpserver.RecordLink    `json:"link"`
}

// toRecordDetailWithLinks is the full record with its links, as shown by
// `brain record show` and the brain_record MCP tool.
func toRecordDetailWithLinks(links app.RecordLinks) mcpserver.RecordDetail {
	detail := toRecordDetail(links.Record)
	detail.Links = toJSONLinks(links.Links)
	detail.Backlinks = toJSONBacklinks(links.Backlinks)
	return detail
}

// toJSONLinks keeps an empty list as [] rather than null.
func toJSONLinks(links []app.ResolvedLink) []mcpserver.RecordLink {
	out := make([]mcpserver.RecordLink, 0, len(links))
	for _, link := range links {
		out = append(out, toJSONLink(link))
	}
	return out
}

func toJSONLink(link app.ResolvedLink) mcpserver.RecordLink {
	return mcpserver.RecordLink{
		Field:     link.Field,
		Ref:       link.Ref,
		Text:      link.Text,
		ProjectID: link.ProjectID,
		ID:        link.ID,
		State:     link.State,
		Detail:    link.Detail,
		Kind:      link.Target.Kind,
		Title:     link.Target.Title,
		Status:    link.Target.Status,
	}
}

func toJSONBacklinks(backlinks []app.Backlink) []mcpserver.RecordBacklink {
	out := make([]mcpserver.RecordBacklink, 0, len(backlinks))
	for _, backlink := range backlinks {
		out = append(out, mcpserver.RecordBacklink{
			Field:     backlink.Field,
			Kind:      backlink.Source.Kind,
			ID:        backlink.Source.ID,
			ProjectID: backlink.Source.ProjectID,
			Title:     backlink.Source.Title,
			Status:    backlink.Source.Status,
		})
	}
	return out
}
