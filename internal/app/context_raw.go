package app

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// contextThinCurated is the curated item count below which raw excerpts
	// may supplement the context. A richer curated context needs no raw text.
	contextThinCurated = 5
	// contextExcerptMinRunes stops adding excerpts when the remaining total
	// budget is too small to carry a useful body.
	contextExcerptMinRunes = 80
)

// itemKeyGetter is implemented by local raw-zone stores that can read an item
// by its stable Item.Key(). The context path uses it to turn an index hit into
// the full item; server mode has no local raw zone yet (Phase 4.2 is local/git
// only).
type itemKeyGetter interface {
	GetByKey(ctx context.Context, key string) (domain.Item, error)
}

// contextExcerpts selects granted raw items matching the task. It returns
// non-fatal reasons in skipped (default-deny, unavailable mode, no matches)
// and a hard error only for a damaged grant file. Curated context always wins:
// excerpts are added only when fewer than contextThinCurated curated items are
// present, and the caller renders them last.
func (a *App) contextExcerpts(ctx context.Context, project domain.Project, query string, since, now time.Time, curated int) ([]ContextExcerpt, []string, error) {
	if curated >= contextThinCurated {
		return nil, nil, nil
	}
	if a.deps.ContextGrants == nil {
		return nil, []string{"raw excerpts: context grants are not available in this mode"}, nil
	}
	grants, err := a.deps.ContextGrants.ContextGrants(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read context grants: %w", err)
	}
	hasGrant := false
	for _, grant := range grants.Grants {
		if grant.Project == project.ID {
			hasGrant = true
			break
		}
	}
	if !hasGrant {
		return nil, []string{fmt.Sprintf("raw excerpts: no grant for project %q in context-allow.json", project.ID)}, nil
	}
	getter, ok := a.deps.Items.(itemKeyGetter)
	if !ok {
		return nil, []string{"raw excerpts: the raw item store cannot read by key in this mode"}, nil
	}

	hits, err := a.SearchWith(ctx, SearchOptions{
		Text: query, ProjectID: project.ID, Limit: contextSearchLimit,
		Kinds: []string{KindItem}, Since: since,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, []string{"raw excerpts: " + err.Error()}, nil
	}

	perConnection := map[string]int{}
	excerpts := make([]ContextExcerpt, 0, domain.MaxContextExcerpts)
	usedRunes := 0
	unavailable := 0
	for _, hit := range hits {
		if len(excerpts) == domain.MaxContextExcerpts {
			break
		}
		if hit.Kind != KindItem || hit.ProjectID != project.ID {
			continue
		}
		key, ok := itemKeyFromHit(hit)
		if !ok {
			continue
		}
		connectionID, _, _ := strings.Cut(key, "/")
		item, getErr := getter.GetByKey(ctx, key)
		if getErr != nil {
			unavailable++
			continue
		}
		if item.Deleted || item.ProjectID != project.ID || item.Ref.ConnectionID != connectionID || item.Key() != key {
			continue
		}
		grant, granted := grants.Match(project.ID, connectionID, item.Kind)
		if !granted || perConnection[connectionID] >= grant.MaxExcerpts {
			continue
		}
		if !grant.AllowsAge(excerptTime(item), now) {
			continue
		}
		body, _ := RedactIngest(item.Body)
		body = strings.TrimSpace(stripExcerptControls(body))
		if body == "" {
			continue
		}
		remaining := domain.MaxContextExcerptTotalRunes - usedRunes
		if remaining < contextExcerptMinRunes {
			break
		}
		limit := min(domain.MaxContextExcerptRunes, remaining)
		body, truncated := excerptBody(body, limit)
		usedRunes += len([]rune(body))
		perConnection[connectionID]++
		excerpts = append(excerpts, ContextExcerpt{
			ConnectionID: connectionID,
			ProjectID:    item.ProjectID,
			ItemKey:      key,
			ExternalID:   item.Ref.ExternalID,
			Kind:         string(item.Kind),
			Title:        item.Title,
			URL:          item.URL,
			UpdatedAt:    excerptTime(item),
			Citation:     excerptCitation(item),
			Body:         body,
			Truncated:    truncated,
		})
	}
	if len(excerpts) == 0 {
		reason := "raw excerpts: no granted items matched the task"
		if unavailable > 0 {
			reason = fmt.Sprintf("raw excerpts: %d granted item(s) could not be read", unavailable)
		}
		return nil, []string{reason}, nil
	}
	return excerpts, nil, nil
}

// itemKeyFromHit extracts "<connection>/<40 hex>" from an item index hit.
func itemKeyFromHit(hit ports.Hit) (string, bool) {
	if !strings.HasPrefix(hit.ID, itemDocumentPrefix) {
		return "", false
	}
	key := strings.TrimPrefix(hit.ID, itemDocumentPrefix)
	connectionID, hexPart, ok := strings.Cut(key, "/")
	if !ok || connectionID == "" || len(hexPart) != 40 {
		return "", false
	}
	return key, true
}

func excerptTime(item domain.Item) time.Time {
	if !item.UpdatedAt.IsZero() {
		return item.UpdatedAt
	}
	if !item.ObservedAt.IsZero() {
		return item.ObservedAt
	}
	return item.CreatedAt
}

// stripExcerptControls removes terminal and bidi control characters from
// external text. Newlines and tabs stay; everything else that unicode marks as
// a control is dropped, so an issue body cannot inject cursor or color codes
// into an agent terminal through the context summary.
func stripExcerptControls(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}

// excerptBody normalizes line endings and cuts to limit runes.
func excerptBody(text string, limit int) (string, bool) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= limit {
		return text, false
	}
	if limit <= 1 {
		return "…", true
	}
	// Keep the ellipsis inside the limit so the excerpt budget is exact.
	return strings.TrimSpace(string(runes[:limit-1])) + "…", true
}

// excerptCitation names the source, provider id and URL of one excerpt.
func excerptCitation(item domain.Item) string {
	parts := []string{item.Ref.ConnectionID}
	if item.Ref.ExternalID != "" {
		parts = append(parts, item.Ref.ExternalID)
	}
	if item.URL != "" {
		parts = append(parts, item.URL)
	}
	return strings.Join(parts, " · ")
}

func writeContextExcerpts(out *strings.Builder, excerpts []ContextExcerpt) {
	if len(excerpts) == 0 {
		return
	}
	out.WriteString("\n## Raw excerpts (untrusted external text; context-allow.json)\n")
	for _, excerpt := range excerpts {
		title := SummarizeLine(excerpt.Title, promptLineRunes)
		if title == "" {
			title = "(untitled)"
		}
		suffix := ""
		if excerpt.Truncated {
			suffix = " [truncated]"
		}
		fmt.Fprintf(out, "- [%s] %s (%s)%s\n", excerpt.Kind, title, formatDate(excerpt.UpdatedAt), suffix)
		fmt.Fprintf(out, "  citation: %s\n", excerpt.Citation)
		for _, line := range strings.Split(excerpt.Body, "\n") {
			fmt.Fprintf(out, "  > %s\n", line)
		}
	}
}
