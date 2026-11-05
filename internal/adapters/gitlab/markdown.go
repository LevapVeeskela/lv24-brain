package gitlab

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
)

func renderDump(dump projectDump, generatedAt time.Time) []byte {
	item := dump.project
	name := item.PathWithNamespace
	if name == "" {
		name = item.Name
	}
	if name == "" {
		name = "unknown project"
	}
	fields := map[string]string{
		"kind":           "connector_dump",
		"source":         Name,
		"project_id":     "gitlab:" + name,
		"schema_version": "1",
		"updated_at":     connectors.FormatTime(generatedAt),
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", connectors.EscapeInline(name))
	fmt.Fprintf(&b, "- URL: %s\n", valueOr(item.WebURL, "unknown"))
	fmt.Fprintf(&b, "- Description: %s\n", valueOr(connectors.OneLine(item.Description), "none"))
	fmt.Fprintf(&b, "- Default branch: %s\n", valueOr(item.DefaultBranch, "unknown"))
	fmt.Fprintf(&b, "- Visibility: %s\n", valueOr(item.Visibility, "unknown"))
	fmt.Fprintf(&b, "- Archived: %t\n", item.Archived)
	fmt.Fprintf(&b, "- Stars: %d\n", item.StarCount)
	fmt.Fprintf(&b, "- Forks: %d\n", item.ForksCount)
	fmt.Fprintf(&b, "- Open issues: %d\n", item.OpenIssuesCount)
	fmt.Fprintf(&b, "- Last activity: %s\n", connectors.DisplayTime(item.LastActivityAt, "unknown"))

	renderMergeRequests(&b, dump.mergeRequests)
	renderIssues(&b, dump.issues)
	renderCommits(&b, dump.commits)

	return connectors.Document(fields, b.String())
}

func renderMergeRequests(b *strings.Builder, items []mergeRequest) {
	fmt.Fprintf(b, "\n## Merge requests (%d)\n\n", len(items))
	if len(items) == 0 {
		b.WriteString("None.\n")
		return
	}
	for _, item := range items {
		fmt.Fprintf(b, "### !%d %s\n\n", item.IID, connectors.EscapeInline(connectors.Truncate(item.Title, 300)))
		fmt.Fprintf(b, "- State: %s\n", valueOr(item.State, "unknown"))
		fmt.Fprintf(b, "- Author: %s\n", valueOr(item.Author.Username, "unknown"))
		fmt.Fprintf(b, "- Draft: %t\n", item.Draft || item.WorkInProgress)
		fmt.Fprintf(b, "- Created: %s\n", connectors.DisplayTime(item.CreatedAt, "unknown"))
		fmt.Fprintf(b, "- Updated: %s\n", connectors.DisplayTime(item.UpdatedAt, "unknown"))
		if item.MergedAt != nil {
			fmt.Fprintf(b, "- Merged: %s\n", connectors.DisplayTime(*item.MergedAt, "unknown"))
		}
		if item.ClosedAt != nil {
			fmt.Fprintf(b, "- Closed: %s\n", connectors.DisplayTime(*item.ClosedAt, "unknown"))
		}
		if item.SourceBranch != "" || item.TargetBranch != "" {
			fmt.Fprintf(b, "- Branches: %s -> %s\n", valueOr(item.SourceBranch, "?"), valueOr(item.TargetBranch, "?"))
		}
		renderLabels(b, item.Labels)
		fmt.Fprintf(b, "- URL: %s\n\n", valueOr(item.WebURL, "unknown"))
	}
}

func renderIssues(b *strings.Builder, items []issue) {
	fmt.Fprintf(b, "\n## Issues (%d)\n\n", len(items))
	if len(items) == 0 {
		b.WriteString("None.\n")
		return
	}
	for _, item := range items {
		fmt.Fprintf(b, "### #%d %s\n\n", item.IID, connectors.EscapeInline(connectors.Truncate(item.Title, 300)))
		fmt.Fprintf(b, "- State: %s\n", valueOr(item.State, "unknown"))
		fmt.Fprintf(b, "- Author: %s\n", valueOr(item.Author.Username, "unknown"))
		fmt.Fprintf(b, "- Comments: %d\n", item.UserNotesCount)
		fmt.Fprintf(b, "- Created: %s\n", connectors.DisplayTime(item.CreatedAt, "unknown"))
		fmt.Fprintf(b, "- Updated: %s\n", connectors.DisplayTime(item.UpdatedAt, "unknown"))
		if item.ClosedAt != nil {
			fmt.Fprintf(b, "- Closed: %s\n", connectors.DisplayTime(*item.ClosedAt, "unknown"))
		}
		renderLabels(b, item.Labels)
		fmt.Fprintf(b, "- URL: %s\n\n", valueOr(item.WebURL, "unknown"))
	}
}

func renderCommits(b *strings.Builder, items []commit) {
	fmt.Fprintf(b, "\n## Commits (%d)\n\n", len(items))
	if len(items) == 0 {
		b.WriteString("None.\n")
		return
	}
	for _, item := range items {
		short := item.ShortID
		if short == "" && len(item.ID) > 7 {
			short = item.ID[:7]
		}
		if short == "" {
			short = "unknown"
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = firstLine(item.Message)
		}
		fmt.Fprintf(b, "### %s %s\n\n", short, connectors.EscapeInline(connectors.Truncate(title, 300)))
		author := item.AuthorName
		if item.AuthorEmail != "" {
			author += " <" + item.AuthorEmail + ">"
		}
		fmt.Fprintf(b, "- Author: %s\n", valueOr(author, "unknown"))
		fmt.Fprintf(b, "- Date: %s\n", connectors.DisplayTime(item.AuthoredDate, "unknown"))
		fmt.Fprintf(b, "- URL: %s\n\n", valueOr(item.WebURL, "unknown"))
	}
}

func renderLabels(b *strings.Builder, labels []string) {
	if len(labels) == 0 {
		return
	}
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		if name := strings.TrimSpace(label); name != "" {
			names = append(names, connectors.EscapeInline(name))
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	fmt.Fprintf(b, "- Labels: %s\n", strings.Join(names, ", "))
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(value), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return "(no message)"
	}
	return line
}

func valueOr(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
