package localapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Page caps of the memory routes. A page is always bounded, and the largest
// offset a client can reach is bounded too, so one request can never ask the
// engine to render an unbounded listing.
const (
	recordsDefaultPageSize = 50
	recordsMaxPageSize     = 200
	recordsWindow          = 1000

	searchDefaultPageSize = 20
	searchMaxPageSize     = 100
	// searchWindow mirrors the SQLite index hit cap: the local index serves at
	// most 200 ranked rows for one query.
	searchWindow = 200

	proposalsDefaultPageSize = 50
	proposalsMaxPageSize     = 200
	proposalsWindow          = 200

	catchupDefaultLimit = 20
	catchupMaxLimit     = 100

	recordBodyDefaultTokens = 4000
	recordBodyMaxTokens     = 32000
)

// Exported window sizes, so the composition layer can request exactly the scan
// a page needs without duplicating the caps.
const (
	// RecordScanWindow is the largest record offset GET /v1/records serves.
	RecordScanWindow = recordsWindow
	// ProposalScanWindow is the largest proposal offset GET /v1/proposals
	// serves.
	ProposalScanWindow = proposalsWindow
)

// cursorVersion is bumped when the cursor payload changes shape.
const cursorVersion = 1

// cursorPayload is the decoded form of a page cursor. Filter binds the cursor
// to the query it was issued for, so a cursor from another project or filter is
// refused instead of silently skipping or repeating rows.
type cursorPayload struct {
	Version int    `json:"v"`
	Scope   string `json:"s"`
	Offset  int    `json:"o"`
	Filter  string `json:"f"`
}

// filterFingerprint builds a short stable fingerprint of a query.
func filterFingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:8])
}

// encodeCursor renders the cursor of the page that starts at offset.
func encodeCursor(scope string, offset int, filter string) string {
	payload, err := json.Marshal(cursorPayload{Version: cursorVersion, Scope: scope, Offset: offset, Filter: filter})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// decodeCursor parses a cursor and checks it belongs to this route and query.
// An empty cursor decodes to offset zero.
func decodeCursor(scope, cursor, filter string) (int, bool) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, true
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	var payload cursorPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, false
	}
	if payload.Version != cursorVersion || payload.Scope != scope || payload.Filter != filter || payload.Offset < 0 {
		return 0, false
	}
	return payload.Offset, true
}

// pageView is the pagination envelope of the list-like memory routes.
type pageView struct {
	Page       int    `json:"page"`
	PageSize   int    `json:"page_size"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// scanLimit is the number of rows to request from the engine for one page: the
// rows before the page, the page itself and one look-ahead row that proves
// whether another page exists. A bounded window clamps it.
func scanLimit(offset, size, window int) int {
	want := offset + size + 1
	if want > window {
		return window
	}
	return want
}

// paginate slices one fetched window into a page and builds its envelope. rows
// must be the first scanLimit(offset, size, window) rows of the ordered result
// set.
func paginate[T any](rows []T, offset, size, window int, scope, filter string) ([]T, pageView) {
	page := pageView{Page: offset/size + 1, PageSize: size}
	var items []T
	if offset < len(rows) {
		end := offset + size
		if end > len(rows) {
			end = len(rows)
		}
		items = rows[offset:end]
	}
	served := offset + size
	more := len(rows) > served || (len(rows) >= window && served >= window)
	if more {
		page.HasMore = true
		page.NextCursor = encodeCursor(scope, offset+len(items), filter)
		if page.NextCursor == "" {
			page.HasMore = false
		}
	}
	return items, page
}
