package gitlab

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

const (
	// maxPages caps how many pages a single collection request may walk.
	maxPages = 10
	// maxPerPage is the largest page size GitLab accepts.
	maxPerPage = 100
)

// listPaged walks up to maxPages pages of a collection and returns at most
// limit items accepted by keep (nil keeps all). stop, when set, ends paging
// after a page whose items make further pages irrelevant to the caller.
// The page size is fixed for the whole walk because pages are numbered, so
// changing per_page mid-walk would skip or repeat items.
func listPaged[T any](ctx context.Context, c *Connector, token, path string, query url.Values, limit int, keep func(T) bool, stop func([]T) bool) ([]T, error) {
	query.Set("per_page", strconv.Itoa(min(maxPerPage, limit)))
	pageURL := c.baseURL + path + "?" + query.Encode()
	var out []T
	for range maxPages {
		var items []T
		headers, err := c.getURL(ctx, token, pageURL, &items)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if keep == nil || keep(item) {
				out = append(out, item)
			}
		}
		if len(out) >= limit || (stop != nil && stop(items)) {
			break
		}
		next, ok := c.nextPage(pageURL, headers)
		if !ok {
			break
		}
		pageURL = next
	}
	return out[:min(len(out), limit)], nil
}

// nextPage returns the URL of the following page. X-Next-Page is authoritative
// when set; otherwise the Link header rel="next" is used. A Link target on a
// different scheme or host ends the collection, so the token is never sent
// elsewhere.
func (c *Connector) nextPage(current string, headers http.Header) (string, bool) {
	if page := strings.TrimSpace(headers.Get("X-Next-Page")); page != "" {
		return withPage(current, page)
	}
	target, ok := linkNext(strings.Join(headers.Values("Link"), ","))
	if !ok {
		return "", false
	}
	return sameAPIHost(c.baseURL, target)
}

// withPage returns current with its page query parameter set to page.
func withPage(current, page string) (string, bool) {
	if n, err := strconv.Atoi(page); err != nil || n <= 0 {
		return "", false
	}
	parsed, err := url.Parse(current)
	if err != nil {
		return "", false
	}
	query := parsed.Query()
	query.Set("page", page)
	parsed.RawQuery = query.Encode()
	return parsed.String(), true
}

// linkNext extracts the rel="next" URL from a Link header value.
func linkNext(header string) (string, bool) {
	for _, link := range strings.Split(header, ",") {
		segments := strings.Split(link, ";")
		if len(segments) < 2 {
			continue
		}
		target := strings.Trim(strings.TrimSpace(segments[0]), "<>")
		for _, param := range segments[1:] {
			key, value, found := strings.Cut(strings.TrimSpace(param), "=")
			if found && strings.EqualFold(key, "rel") && slices.Contains(strings.Fields(strings.Trim(value, `"`)), "next") {
				return target, true
			}
		}
	}
	return "", false
}

// sameAPIHost returns raw when it is an absolute URL with the same scheme and
// host as base.
func sameAPIHost(base, raw string) (string, bool) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", false
	}
	target, err := url.Parse(raw)
	if err != nil || target.Scheme != baseURL.Scheme || !strings.EqualFold(target.Host, baseURL.Host) {
		return "", false
	}
	return target.String(), true
}
