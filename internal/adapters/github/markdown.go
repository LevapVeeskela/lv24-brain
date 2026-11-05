package github

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
)

func renderDump(dump repoDump, generatedAt time.Time) []byte {
	repo := dump.repo
	fullName := repo.FullName
	if fullName == "" {
		fullName = "unknown repository"
	}
	fields := map[string]string{
		"kind":           "connector_dump",
		"source":         Name,
		"project_id":     "github:" + fullName,
		"schema_version": "1",
		"updated_at":     connectors.FormatTime(generatedAt),
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", connectors.EscapeInline(fullName))
	fmt.Fprintf(&b, "- URL: %s\n", valueOr(repo.HTMLURL, "unknown"))
	fmt.Fprintf(&b, "- Description: %s\n", valueOr(connectors.OneLine(repo.Description), "none"))
	fmt.Fprintf(&b, "- Default branch: %s\n", valueOr(repo.DefaultBranch, "unknown"))
	fmt.Fprintf(&b, "- Visibility: %s\n", visibility(repo.Private))
	fmt.Fprintf(&b, "- Archived: %t\n", repo.Archived)
	fmt.Fprintf(&b, "- Fork: %t\n", repo.Fork)
	fmt.Fprintf(&b, "- Stars: %d\n", repo.StargazersCount)
	fmt.Fprintf(&b, "- Forks: %d\n", repo.ForksCount)
	fmt.Fprintf(&b, "- Open issues: %d\n", repo.OpenIssuesCount)
	fmt.Fprintf(&b, "- Last updated: %s\n", connectors.DisplayTime(repo.UpdatedAt, "unknown"))

	renderPullRequests(&b, dump.pulls)
	renderIssues(&b, dump.issues)
	renderCommits(&b, dump.commits)

	return connectors.Document(fields, b.String())
}

func renderPullRequests(b *strings.Builder, pulls []pullRequest) {
	fmt.Fprintf(b, "\n## Pull requests (%d)\n\n", len(pulls))
	if len(pulls) == 0 {
		b.WriteString("None.\n")
		return
	}
	for _, item := range pulls {
		fmt.Fprintf(b, "### #%d %s\n\n", item.Number, connectors.EscapeInline(connectors.Truncate(item.Title, 300)))
		fmt.Fprintf(b, "- State: %s\n", valueOr(item.State, "unknown"))
		fmt.Fprintf(b, "- Author: %s\n", valueOr(item.User.Login, "unknown"))
		fmt.Fprintf(b, "- Draft: %t\n", item.Draft)
		fmt.Fprintf(b, "- Created: %s\n", connectors.DisplayTime(item.CreatedAt, "unknown"))
		fmt.Fprintf(b, "- Updated: %s\n", connectors.DisplayTime(item.UpdatedAt, "unknown"))
		if item.MergedAt != nil {
			fmt.Fprintf(b, "- Merged: %s\n", connectors.DisplayTime(*item.MergedAt, "unknown"))
		}
		if item.Head.Ref != "" || item.Base.Ref != "" {
			fmt.Fprintf(b, "- Branches: %s -> %s\n", valueOr(item.Head.Ref, "?"), valueOr(item.Base.Ref, "?"))
		}
		renderLabels(b, item.Labels)
		fmt.Fprintf(b, "- URL: %s\n\n", valueOr(item.HTMLURL, "unknown"))
	}
}

func renderIssues(b *strings.Builder, issues []issue) {
	fmt.Fprintf(b, "\n## Issues (%d)\n\n", len(issues))
	if len(issues) == 0 {
		b.WriteString("None.\n")
		return
	}
	for _, item := range issues {
		fmt.Fprintf(b, "### #%d %s\n\n", item.Number, connectors.EscapeInline(connectors.Truncate(item.Title, 300)))
		fmt.Fprintf(b, "- State: %s\n", valueOr(item.State, "unknown"))
		fmt.Fprintf(b, "- Author: %s\n", valueOr(item.User.Login, "unknown"))
		fmt.Fprintf(b, "- Comments: %d\n", item.Comments)
		fmt.Fprintf(b, "- Created: %s\n", connectors.DisplayTime(item.CreatedAt, "unknown"))
		fmt.Fprintf(b, "- Updated: %s\n", connectors.DisplayTime(item.UpdatedAt, "unknown"))
		if item.ClosedAt != nil {
			fmt.Fprintf(b, "- Closed: %s\n", connectors.DisplayTime(*item.ClosedAt, "unknown"))
		}
		renderLabels(b, item.Labels)
		fmt.Fprintf(b, "- URL: %s\n\n", valueOr(item.HTMLURL, "unknown"))
	}
}

func renderCommits(b *strings.Builder, commits []commit) {
	fmt.Fprintf(b, "\n## Commits (%d)\n\n", len(commits))
	if len(commits) == 0 {
		b.WriteString("None.\n")
		return
	}
	for _, item := range commits {
		title := firstLine(item.Commit.Message)
		shortSHA := item.SHA
		if len(shortSHA) > 7 {
			shortSHA = shortSHA[:7]
		}
		fmt.Fprintf(b, "### %s %s\n\n", shortSHA, connectors.EscapeInline(connectors.Truncate(title, 300)))
		author := item.Commit.Author.Name
		if item.Commit.Author.Email != "" {
			author += " <" + item.Commit.Author.Email + ">"
		}
		fmt.Fprintf(b, "- Author: %s\n", valueOr(author, "unknown"))
		if item.Author != nil && item.Author.Login != "" {
			fmt.Fprintf(b, "- GitHub user: %s\n", item.Author.Login)
		}
		fmt.Fprintf(b, "- Date: %s\n", connectors.DisplayTime(item.Commit.Author.Date, "unknown"))
		fmt.Fprintf(b, "- URL: %s\n\n", valueOr(item.HTMLURL, "unknown"))
	}
}

func renderLabels(b *strings.Builder, labels []label) {
	if len(labels) == 0 {
		return
	}
	names := make([]string, 0, len(labels))
	for _, item := range labels {
		if name := strings.TrimSpace(item.Name); name != "" {
			names = append(names, connectors.EscapeInline(name))
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	fmt.Fprintf(b, "- Labels: %s\n", strings.Join(names, ", "))
}

func visibility(private bool) string {
	if private {
		return "private"
	}
	return "public"
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
