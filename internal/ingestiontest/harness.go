package ingestiontest

import (
	"fmt"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Harness plugs one connector into RunSourceSuite. NewSource and Connection
// are required; the mutation closures drive the connector's fake upstream
// between reads. A nil mutation skips the cases that need it only when the
// suite cannot seed the state another way; UpdateItem/DeleteItem/AddMany
// gaps are reported as skips with a clear message.
type Harness struct {
	// Connection is the connection under test.
	Connection domain.Connection
	// NewSource builds the Source for conn bound to the fake upstream.
	// It must return a source whose Descriptor().Provider matches
	// conn.Provider. Returning a failing-Check source for bad credentials
	// is how the bad-credential case is exercised.
	NewSource func(conn domain.Connection) ports.Source
	// AddItem appends one new item to the fake upstream.
	AddItem func()
	// UpdateItem changes the content of one existing upstream item.
	UpdateItem func()
	// DeleteItem removes one existing item from the fake upstream.
	DeleteItem func()
	// AddMany appends n new items to the fake upstream.
	AddMany func(n int)
	// BadConnection, when non-nil, is a connection variant whose Check
	// must fail clearly (bad credentials). Nil skips that assertion.
	BadConnection *domain.Connection
	// PageSize is the ReadRequest.Limit used for the paging and replay
	// cases. Values <= 0 select a default of 5.
	PageSize int
}

// pageSize returns the effective page size for the suite.
func (h Harness) pageSize() int {
	if h.PageSize > 0 {
		return h.PageSize
	}
	return 5
}

// skipError marks a case that cannot run with this harness or descriptor.
// RunSourceSuite maps it to t.Skip; negative tests assert on other errors.
type skipError struct {
	msg string
}

func (e *skipError) Error() string { return "skipped: " + e.msg }

func skipf(format string, args ...any) *skipError {
	return &skipError{msg: fmt.Sprintf(format, args...)}
}
