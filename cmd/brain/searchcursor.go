package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// The CLI search cursor mirrors the MCP pagination contract (page, page_size,
// has_more, next_cursor) so an agent that learned one surface can use the
// other. A cursor is an opaque, query-bound offset: it is refused when it is
// replayed on another query, which is what keeps paging from skipping or
// repeating a hit.
const (
	// searchMaxWindow is the largest result offset the index can serve,
	// mirroring the SQLite index hit cap.
	searchMaxWindow = 200
	// searchCursorVersion is bumped when the cursor payload changes shape.
	searchCursorVersion = 1
	// searchCursorTool names the cursor family.
	searchCursorTool = "brain_search"
)

// errInvalidSearchCursor marks a cursor the caller did not receive from this
// command. It is a usage error, not a runtime failure.
var errInvalidSearchCursor = errors.New("invalid search cursor")

// searchCursorPayload is the decoded form of a page cursor.
type searchCursorPayload struct {
	Version int    `json:"v"`
	Tool    string `json:"tool"`
	Offset  int    `json:"offset"`
	Filter  string `json:"filter"`
}

// searchPage is the pagination envelope of `brain search`.
type searchPage struct {
	Page       int
	PageSize   int
	HasMore    bool
	NextCursor string
}

// searchFilterFingerprint binds a cursor to the query it was issued for.
func searchFilterFingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:8])
}

// encodeSearchCursor renders the cursor of the page that starts at offset.
func encodeSearchCursor(offset int, filter string) string {
	payload, err := json.Marshal(searchCursorPayload{
		Version: searchCursorVersion, Tool: searchCursorTool, Offset: offset, Filter: filter,
	})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// decodeSearchCursor parses a cursor and checks it belongs to this query. An
// empty cursor decodes to offset zero.
func decodeSearchCursor(cursor, filter string) (int, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errInvalidSearchCursor, err)
	}
	var payload searchCursorPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, fmt.Errorf("%w: %v", errInvalidSearchCursor, err)
	}
	if payload.Version != searchCursorVersion || payload.Tool != searchCursorTool {
		return 0, fmt.Errorf("%w: not issued by brain search", errInvalidSearchCursor)
	}
	if payload.Filter != filter {
		return 0, fmt.Errorf("%w: it belongs to another query", errInvalidSearchCursor)
	}
	if payload.Offset < 0 || payload.Offset >= searchMaxWindow {
		return 0, fmt.Errorf("%w: it starts past the %d-result window; narrow the query and search again", errInvalidSearchCursor, searchMaxWindow)
	}
	return payload.Offset, nil
}

// searchFetchLimit is how many rows to request for one page: the rows before
// it, the page itself and one look-ahead row that proves another page exists.
// The index hit cap clamps it.
func searchFetchLimit(offset, size int) int {
	want := offset + size + 1
	if want > searchMaxWindow {
		return searchMaxWindow
	}
	return want
}

// pageSearchHits slices one fetched window into a page and builds its envelope.
// hits must be the first requested rows of the ranked result set.
func pageSearchHits(hits []ports.Hit, offset, size, requested int, filter string) ([]ports.Hit, searchPage) {
	returned := len(hits)
	page := searchPage{Page: offset/size + 1, PageSize: size}
	var items []ports.Hit
	if offset < returned {
		end := offset + size
		if end > returned {
			end = returned
		}
		items = hits[offset:end]
	}
	switch {
	case offset+len(items) < returned:
		// A row beyond this page is present, so another page exists.
		page.HasMore = true
	case returned == requested && offset+size >= requested:
		// The backend filled the whole requested window. When that window was
		// clamped at the result window, another page may exist; its cursor
		// either serves it or reports that the window is exhausted.
		page.HasMore = true
	}
	if page.HasMore {
		page.NextCursor = encodeSearchCursor(offset+len(items), filter)
		if page.NextCursor == "" {
			page.HasMore = false
		}
	}
	return items, page
}
