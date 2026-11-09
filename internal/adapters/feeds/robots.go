package feeds

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Minimal RFC 9309 subset: fetch <scheme>://<host>/robots.txt once per host
// per Read, parse the groups for our user agent (and "*"), and prefix-match
// requests against Allow/Disallow so specific Allow rules can re-open
// blocked paths. Any transport failure, redirect or malformed file is
// treated as permissive (RFC 9309 §2.4 forbids caching unreachable robots
// as permanent blocks).

// robotsAgent is our user agent as matched by robots.txt groups.
const robotsAgent = UserAgent

// robots caches parsed robots.txt per host for one Read call.
type robots struct {
	perHost map[string][]robotRule
}

// newRobots returns an empty cache.
func newRobots() *robots {
	return &robots{perHost: map[string][]robotRule{}}
}

// robotRule is one Allow/Disallow path prefix from a matching group.
type robotRule struct {
	prefix string
	allow  bool
}

// disallows reports whether the URL is blocked for our agent. The first call
// per host fetches robots.txt.
func (r *robots) disallows(ctx context.Context, f *fetcher, u *url.URL) bool {
	host := strings.ToLower(u.Host)
	if _, ok := r.perHost[host]; !ok {
		r.perHost[host] = r.fetch(ctx, f, u)
	}
	rules := r.perHost[host]
	target := u.RequestURI()
	var longestAllow, longestDisallow int
	for _, rule := range rules {
		if !rule.matches(target) {
			continue
		}
		if rule.allow {
			longestAllow = max(longestAllow, len(rule.prefix))
		} else {
			longestDisallow = max(longestDisallow, len(rule.prefix))
		}
	}
	// RFC 9309 §2.2.2: the most specific (longest) matching rule wins, and
	// equal lengths count as Allow.
	return longestDisallow > 0 && longestDisallow > longestAllow
}

// fetch parses robots.txt at the page's scheme and host.
func (r *robots) fetch(ctx context.Context, f *fetcher, u *url.URL) []robotRule {
	robotsURL := u.Scheme + "://" + u.Host + "/robots.txt"
	parsed, err := validateSourceURL("robots.txt", robotsURL)
	if err != nil {
		// Nothing is fetched outside the https/http-loopback policy.
		return nil
	}
	result, err := f.fetch(ctx, parsed, plainHeaders())
	if err != nil || result.Status != http.StatusOK {
		return nil
	}
	return parseRobotsText(string(result.Body), robotsAgent)
}

// matches reports whether the request target starts with the rule prefix.
func (rule robotRule) matches(target string) bool {
	return strings.HasPrefix(target, rule.prefix)
}

// parseRobotsText collects the Allow/Disallow prefixes of the groups that
// match our agent. Patterns beyond plain prefix matching (wildcards, $) are
// handled as literal strings and an empty Disallow means "allow all".
func parseRobotsText(body, agent string) []robotRule {
	var rules []robotRule
	var agents []string // user agents of the group being collected
	var group []robotRule
	groupHasRules := false
	commit := func() {
		if groupHasRules {
			for _, candidate := range agents {
				if candidate == "*" || strings.EqualFold(agent, candidate) {
					rules = append(rules, group...)
				}
			}
		}
		agents = nil
		group = nil
		groupHasRules = false
	}
	for _, line := range strings.Split(body, "\n") {
		if index := strings.IndexByte(line, '#'); index >= 0 {
			line = line[:index]
		}
		line = strings.TrimSpace(line)
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "user-agent":
			if groupHasRules {
				commit()
			}
			agents = append(agents, value)
		case "allow":
			if value != "" {
				groupHasRules = true
				group = append(group, robotRule{prefix: value, allow: true})
			}
		case "disallow":
			if value != "" {
				groupHasRules = true
				group = append(group, robotRule{prefix: value})
			}
		default:
			// crawl-delay, sitemap and other directives are ignored.
		}
	}
	commit()
	return rules
}
