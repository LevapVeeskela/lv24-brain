// Package linear implements a read-only Linear source (ports.Source). It
// reads issues through the Linear GraphQL API, resolves the token from
// ports.CredentialProvider at run time, and never stores credentials.
package linear

import (
	"net"
	"net/url"
	"strings"
)

const (
	// Name is the source provider name.
	Name = "linear"
	// DefaultBaseURL is the public Linear GraphQL endpoint.
	DefaultBaseURL = "https://api.linear.app/graphql"
	// DefaultUserAgent identifies the engine to Linear.
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
