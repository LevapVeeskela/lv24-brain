package fsstore

import (
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// slugify is the data-directory name for domain.Slugify: one slug rule for
// record ids and record file names.
func slugify(value string) string {
	return domain.Slugify(value)
}

// singleLine collapses a title into one line so it cannot break a Markdown
// heading layout.
func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
