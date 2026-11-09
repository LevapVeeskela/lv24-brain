package mcpsource

import (
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func resultRecords(result *mcp.CallToolResult) ([]any, error) {
	var value any
	if result.StructuredContent != nil {
		value = result.StructuredContent
	} else {
		var text strings.Builder
		for _, content := range result.Content {
			switch part := content.(type) {
			case *mcp.TextContent:
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString(part.Text)
			case *mcp.EmbeddedResource:
				if part.Resource != nil && part.Resource.Text != "" {
					if text.Len() > 0 {
						text.WriteByte('\n')
					}
					text.WriteString(part.Resource.Text)
				}
			}
		}
		raw := strings.TrimSpace(text.String())
		if raw == "" {
			value = map[string]any{}
		} else if err := json.Unmarshal([]byte(raw), &value); err != nil {
			value = map[string]any{"text": raw}
		}
	}
	if len(canonicalJSON(value)) > maximumResponseBytes {
		// The response has already crossed the transport boundary; do not
		// retain an unbounded payload in item construction.
		return nil, errOversizedResult
	}
	var records []any
	collectRecords(value, &records)
	if len(records) == 0 {
		return []any{value}, nil
	}
	return records, nil
}

func collectRecords(value any, records *[]any) {
	switch current := value.(type) {
	case []any:
		for _, entry := range current {
			collectRecords(entry, records)
		}
	case map[string]any:
		if looksLikeRecord(current) {
			*records = append(*records, current)
			return
		}
		keys := make([]string, 0, len(current))
		for key := range current {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectRecords(current[key], records)
		}
	}
}

func looksLikeRecord(record map[string]any) bool {
	for _, key := range []string{"title", "name", "subject", "id", "text", "content", "description", "body", "summary"} {
		if value, ok := lookup(record, key); ok {
			switch entry := value.(type) {
			case string:
				if strings.TrimSpace(entry) != "" {
					return true
				}
			case float64, json.Number:
				if key == "id" {
					return true
				}
			}
		}
	}
	return false
}

func firstString(values map[string]any, names ...string) string {
	if values == nil {
		return ""
	}
	for _, name := range names {
		value, ok := lookup(values, name)
		if !ok || value == nil {
			continue
		}
		switch entry := value.(type) {
		case string:
			if strings.TrimSpace(entry) != "" {
				return strings.TrimSpace(entry)
			}
		case float64:
			if name == "id" {
				return strconv.FormatFloat(entry, 'f', -1, 64)
			}
		case json.Number:
			if name == "id" {
				return entry.String()
			}
		}
	}
	return ""
}

func extractURL(values map[string]any) string {
	for _, name := range []string{"url", "uri", "link", "htmlurl", "weburl", "permalink"} {
		value, ok := lookup(values, name)
		candidate, okString := value.(string)
		if !ok || !okString {
			continue
		}
		candidate = strings.TrimSpace(candidate)
		if len(candidate) > 2048 {
			continue
		}
		parsed, err := url.Parse(candidate)
		if err == nil && parsed.Host != "" && parsed.User == nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) {
			return parsed.String()
		}
	}
	return ""
}

func extractTime(values map[string]any, names ...string) time.Time {
	for _, name := range names {
		value, ok := lookup(values, name)
		text, okString := value.(string)
		if !ok || !okString {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text)); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func lookup(values map[string]any, name string) (any, bool) {
	wanted := normalizedField(name)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := values[key]
		if normalizedField(key) == wanted {
			return value, true
		}
	}
	return nil, false
}

func normalizedField(value string) string {
	value = strings.ToLower(value)
	value = strings.NewReplacer("_", "", "-", "", " ", "", ".", "").Replace(value)
	return value
}

func canonicalJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return string(encoded)
}

var errOversizedResult = errors.New("mcp result exceeds size cap")

func capUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
