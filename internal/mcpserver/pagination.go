package mcpserver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Page is the pagination envelope of the list-like tools. It is embedded in
// their structured result, so page/page_size/has_more/next_cursor are top-level
// fields of the tool output.
type Page struct {
	// Page is the 1-based number of the returned page.
	Page int `json:"page"`
	// PageSize is the number of items this page may hold.
	PageSize int `json:"page_size"`
	// HasMore reports whether another page exists. When it is true, NextCursor
	// is set.
	HasMore bool `json:"has_more"`
	// NextCursor is the opaque cursor of the next page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// pageLimits bounds one tool's pages. window is the largest offset+size the
// backend can serve; 0 means the backend is unbounded for that tool.
type pageLimits struct {
	def    int
	max    int
	window int
}

// cursorVersion is bumped when the cursor payload changes shape.
const cursorVersion = 1

// cursorPayload is the decoded form of a page cursor. Filter binds the cursor
// to the query it was issued for, so a cursor from another query is refused
// instead of silently skipping or repeating rows.
type cursorPayload struct {
	Version int    `json:"v"`
	Tool    string `json:"tool"`
	Offset  int    `json:"offset"`
	Filter  string `json:"filter"`
}

// filterFingerprint builds a short stable fingerprint of a query. The order of
// the parts is the caller's, so two calls with the same arguments always
// produce the same fingerprint.
func filterFingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:8])
}

// encodeCursor renders the cursor of the page that starts at offset.
func encodeCursor(tool string, offset int, filter string) string {
	payload, err := json.Marshal(cursorPayload{Version: cursorVersion, Tool: tool, Offset: offset, Filter: filter})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// decodeCursor parses a cursor and checks it belongs to this tool and query.
// An empty cursor decodes to offset zero.
func decodeCursor(tool, cursor, filter string) (offset int, err error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, nil
	}
	raw, decodeErr := base64.RawURLEncoding.DecodeString(cursor)
	if decodeErr != nil {
		return 0, refusalInvalid("cursor is not a cursor issued by this server")
	}
	var payload cursorPayload
	if unmarshalErr := json.Unmarshal(raw, &payload); unmarshalErr != nil {
		return 0, refusalInvalid("cursor is not a cursor issued by this server")
	}
	if payload.Version != cursorVersion || payload.Tool != tool || payload.Filter != filter || payload.Offset < 0 {
		return 0, refusalInvalidf("cursor does not belong to this query of %s", tool)
	}
	return payload.Offset, nil
}

// pageRequest resolves the cursor and page size of one list call.
func pageRequest(tool string, limits pageLimits, limit int, cursor, filter string) (offset, size int, err error) {
	if limit < 0 {
		return 0, 0, refusalInvalid("limit must not be negative")
	}
	size = limit
	if size == 0 {
		size = limits.def
	}
	if limits.max > 0 && size > limits.max {
		size = limits.max
	}
	offset, err = decodeCursor(tool, cursor, filter)
	if err != nil {
		return 0, 0, err
	}
	if limits.window > 0 && offset >= limits.window {
		return 0, 0, refusalUnsupported(
			fmt.Sprintf("%s serves at most %d results per query; this cursor starts at %d", tool, limits.window, offset),
			"narrow the query (project, since, kinds) and start a new search",
		)
	}
	return offset, size, nil
}

// fetchLimit is the number of rows to request from the backend for one page: the
// rows before the page, the page itself and one look-ahead row that proves
// whether another page exists. A bounded window clamps it.
func fetchLimit(offset, size int, limits pageLimits) int {
	want := offset + size + 1
	if limits.window > 0 && want > limits.window {
		return limits.window
	}
	return want
}

// paginate slices one fetched window into a page and builds its envelope. rows
// must be the first requested rows of the ordered result set; requested is the
// limit that was asked of the backend.
func paginate[T any](rows []T, offset, size, requested int, tool, filter string) ([]T, Page) {
	returned := len(rows)
	page := Page{Page: offset/size + 1, PageSize: size}
	var items []T
	if offset < returned {
		items = rows[offset:min(offset+size, returned)]
	}
	switch {
	case offset+len(items) < returned:
		// A row beyond this page is present, so another page exists.
		page.HasMore = true
	case returned == requested && offset+size >= requested:
		// The backend filled the whole requested window. When that window was
		// clamped at the tool's limit another page may exist; its cursor either
		// serves it or reports that the window is exhausted.
		page.HasMore = true
	}
	if page.HasMore {
		page.NextCursor = encodeCursor(tool, offset+len(items), filter)
		if page.NextCursor == "" {
			page.HasMore = false
		}
	}
	return items, page
}

// pageLimitsFor returns the limits of a paginated tool.
func pageLimitsFor(tool string) pageLimits {
	switch tool {
	case "brain_search":
		return pageLimits{def: searchDefaultPageSize, max: searchMaxPageSize, window: searchWindow}
	case "brain_records":
		return pageLimits{def: recordsDefaultPageSize, max: recordsMaxPageSize}
	case "brain_journal":
		return pageLimits{def: DefaultJournalLimit, max: journalMaxPageSize, window: journalWindow}
	default:
		return pageLimits{def: 20, max: 100}
	}
}

const (
	// searchDefaultPageSize is the page size of an unpaginated brain_search.
	searchDefaultPageSize = 20
	// searchMaxPageSize bounds one brain_search page.
	searchMaxPageSize = 100
	// searchWindow is the largest result offset the index can serve, mirroring
	// the SQLite index hit cap.
	searchWindow = 200
	// recordsDefaultPageSize is the page size of an unpaginated brain_records.
	recordsDefaultPageSize = 50
	// recordsMaxPageSize bounds one brain_records page.
	recordsMaxPageSize = 200
	// journalMaxPageSize bounds one brain_journal page.
	journalMaxPageSize = 500
	// journalWindow is the largest journal offset the journal adapter can serve.
	journalWindow = 1000
)

// limitsOf renders page limits as the limits map of a catalogue entry.
func limitsOf(limits pageLimits) map[string]int {
	out := map[string]int{"default_page_size": limits.def, "max_page_size": limits.max}
	if limits.window > 0 {
		out["max_window"] = limits.window
	}
	return out
}

// cursorProp is the shared cursor argument of the list-like tools.
func cursorProp() map[string]any {
	return stringProp("Opaque cursor from a previous page. Omit for the first page; cursors are bound to their query.")
}

// pageOutputProps are the pagination fields of a list-like tool's output schema.
func pageOutputProps() map[string]any {
	return map[string]any{
		"page":        intProp("1-based number of this page.", 1),
		"page_size":   intProp("Number of items this page may hold.", 1),
		"has_more":    boolProp("Whether another page exists."),
		"next_cursor": stringProp("Cursor of the next page; present only when has_more is true."),
	}
}

// pageFooter is the human line that tells a model how to continue reading.
func pageFooter(page Page) string {
	if !page.HasMore || page.NextCursor == "" {
		return ""
	}
	return fmt.Sprintf("more: pass cursor=%q for page %d", page.NextCursor, page.Page+1)
}
