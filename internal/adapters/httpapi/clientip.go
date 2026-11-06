package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ParseTrustedProxies parses a comma-separated list of IP addresses and CIDR
// prefixes (for example "127.0.0.1,10.0.0.0/8"). An empty value trusts no
// proxy, so X-Forwarded-For is ignored.
func ParseTrustedProxies(value string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			prefix, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", part, err)
			}
			out = append(out, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", part, err)
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// clientIP returns the address used for per-client rate limiting. The direct
// peer is used unless it is a trusted proxy; then X-Forwarded-For is walked
// from the right and the first untrusted hop is the client. Spoofed entries to
// the left of the trusted chain are therefore ignored.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := remoteHost(r)
	if len(trusted) == 0 {
		return peer
	}
	addr, err := netip.ParseAddr(peer)
	if err != nil || !isTrustedProxy(addr, trusted) {
		return peer
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			// A malformed hop cannot be attributed; fall back to the proxy so
			// the request is still limited.
			return peer
		}
		if !isTrustedProxy(hop, trusted) {
			return hop.Unmap().String()
		}
	}
	return peer
}

func isTrustedProxy(addr netip.Addr, trusted []netip.Prefix) bool {
	addr = addr.Unmap()
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}
