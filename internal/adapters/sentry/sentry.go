// Package sentry implements a read-only native Sentry source.
//
// Design choice (events): each issue fans out to at most 3 newest events
// from /api/0/issues/{id}/events/ so item bodies carry a bounded stack
// trace/message for attempts candidates. The issue's metadata/culprit is
// always included; when the events endpoint errors, the body falls back to
// metadata/culprit alone (context cancellation still aborts the read). This
// bounds per-issue fan-out to 3 requests while keeping bodies useful;
// raising the cap would multiply request volume per read with little extra
// signal. Sentry event entries store frames oldest-first, so the body keeps
// the last frames of each event (nearest the crash).
package sentry

import (
	"net"
	"net/url"
	"strings"
)

const (
	// Name is the source provider name.
	Name = "sentry"
	// DefaultBaseURL is the Sentry SaaS API endpoint.
	DefaultBaseURL = "https://sentry.io"
	// DefaultUserAgent identifies the engine to Sentry.
	DefaultUserAgent = "lv24-brain"
)

// isAllowedBaseURL reports whether u may carry the token: https always, http
// only when the host is loopback so local test servers keep working.
func isAllowedBaseURL(u *url.URL) bool {
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}
