package httpclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// searchResponse is the GET /v1/search envelope. The server encodes each hit
// as a ports.Hit without JSON tags, so the field names match the Go fields.
type searchResponse struct {
	Items []ports.Hit `json:"items"`
}

// Search runs a full-text search on the server through GET /v1/search. The
// server reads q, projectId, and limit; empty optional parameters are omitted
// so the server applies its own defaults.
func (c *Client) Search(ctx context.Context, query ports.Query) ([]ports.Hit, error) {
	text := strings.TrimSpace(query.Text)
	if err := requireValue("search.query", text); err != nil {
		return nil, err
	}
	params := url.Values{}
	params.Set("q", text)
	if projectID := strings.TrimSpace(query.ProjectID); projectID != "" {
		params.Set("projectId", projectID)
	}
	if query.Limit > 0 {
		params.Set("limit", strconv.Itoa(query.Limit))
	}
	var payload searchResponse
	if err := c.do(ctx, http.MethodGet, "/v1/search", params, nil, &payload); err != nil {
		return nil, err
	}
	if payload.Items == nil {
		return []ports.Hit{}, nil
	}
	return payload.Items, nil
}
