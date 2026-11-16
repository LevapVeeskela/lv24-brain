package httpclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var _ ports.RecordReviewer = (*Client)(nil)

// ReviewRecord marks one server-side record reviewed and optionally sets its
// trust level. It reports whether the review changed anything.
func (c *Client) ReviewRecord(ctx context.Context, projectID, kind, id string, trust domain.Trust) (bool, error) {
	if err := requireValue("record.id", id); err != nil {
		return false, err
	}
	var path string
	if strings.TrimSpace(projectID) != "" {
		path = "/v1/projects/" + url.PathEscape(projectID) + "/records/" + url.PathEscape(kind) + "/" + url.PathEscape(id) + "/review"
	} else {
		path = "/v1/records/" + url.PathEscape(kind) + "/" + url.PathEscape(id) + "/review"
	}
	body := map[string]any{"reviewed": true}
	if trust != "" {
		body["trust"] = trust
	}
	switch kind {
	case ports.RecordNote:
		var resp struct {
			noteRecord
			Changed bool `json:"changed"`
		}
		if err := c.do(ctx, http.MethodPost, path, nil, body, &resp); err != nil {
			return false, err
		}
		return resp.Changed, nil
	case ports.RecordDecision:
		var resp struct {
			decisionRecord
			Changed bool `json:"changed"`
		}
		if err := c.do(ctx, http.MethodPost, path, nil, body, &resp); err != nil {
			return false, err
		}
		return resp.Changed, nil
	case ports.RecordAttempt:
		var resp struct {
			attemptRecord
			Changed bool `json:"changed"`
		}
		if err := c.do(ctx, http.MethodPost, path, nil, body, &resp); err != nil {
			return false, err
		}
		return resp.Changed, nil
	case ports.RecordHandoff:
		var resp struct {
			handoffRecord
			Changed bool `json:"changed"`
		}
		if err := c.do(ctx, http.MethodPost, path, nil, body, &resp); err != nil {
			return false, err
		}
		return resp.Changed, nil
	default:
		return false, domain.NewValidationError("kind", "unknown record kind")
	}
}
