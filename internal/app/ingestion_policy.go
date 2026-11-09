package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Ingest policy (WP0.4). Every item a source returns passes through
// normalizeItem before it reaches an ItemStore:
//
//   - Ref.ConnectionID is set when empty; an item of another connection is
//     skipped (a source bug, never stored under the wrong connection);
//   - ObservedAt is the host clock; CreatedAt, UpdatedAt and ObservedAt are
//     UTC and truncated to microseconds (Postgres precision), so items read
//     back from any store still match their Hash;
//   - ProjectID is kept when it is one of Connection.Projects, else it is the
//     connection's only project, else empty (unmapped);
//   - invalid UTF-8 in text fields is replaced with U+FFFD;
//   - secrets in title, body, URL, author and labels are replaced with
//     [redacted] (RedactIngest: the curated patterns plus bearer and
//     authorization headers, URL query tokens, more Slack, GitLab, npm and
//     Stripe token shapes and unterminated private key blocks);
//   - for sources with SourceDescriptor.PersonalData (mail, chat) e-mail
//     addresses in every text field become "[email:<12 hex>]", a hash of the
//     connection ID and the lower-cased address: stable per connection (the
//     same sender always maps to the same placeholder, so threads stay
//     searchable) without being joinable across connections. Names and other
//     personal data are kept;
//   - a body longer than domain.MaxItemBodyBytes is cut on a UTF-8 boundary
//     and ends with ingestTruncationMarker. Redaction runs first, so a cut
//     never splits a secret into an unrecognizable fragment;
//   - then WithHash and Validate; an invalid item is skipped with a reason.
//
// Tombstones keep only Ref, Deleted and ObservedAt: nothing else of a
// deleted item is stored.

// ingestTruncationMarker ends a truncated body; %d are kept and original sizes.
const ingestTruncationMarker = "\n\n[truncated by lv24-brain: kept %d of %d bytes]"

// emailPattern matches common e-mail addresses (not every RFC 5322 form).
var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)

// itemReport counts what normalization changed in one item.
type itemReport struct {
	redactions    int
	emails        int
	bodyTruncated int
}

func (r *itemReport) add(other itemReport) {
	r.redactions += other.redactions
	r.emails += other.emails
	r.bodyTruncated += other.bodyTruncated
}

// ingestPolicy normalizes items of one connection.
type ingestPolicy struct {
	connection   domain.Connection
	personalData bool
}

func newIngestPolicy(connection domain.Connection, descriptor domain.SourceDescriptor) ingestPolicy {
	return ingestPolicy{connection: connection, personalData: descriptor.PersonalData}
}

// normalizeItem applies the ingest policy. A returned error is the skip
// reason; the item must not be stored.
func (p ingestPolicy) normalizeItem(item domain.Item, observedAt time.Time) (domain.Item, itemReport, error) {
	var report itemReport
	switch item.Ref.ConnectionID {
	case "":
		item.Ref.ConnectionID = p.connection.ID
	case p.connection.ID:
	default:
		return domain.Item{}, report, fmt.Errorf("item belongs to connection %q, not %q", item.Ref.ConnectionID, p.connection.ID)
	}
	observedAt = microUTC(observedAt)
	if item.Deleted {
		tombstone := domain.Item{Ref: item.Ref, Deleted: true, ObservedAt: observedAt}.WithHash()
		return tombstone, report, tombstone.Validate()
	}
	item.ObservedAt = observedAt
	item.CreatedAt = microUTC(item.CreatedAt)
	item.UpdatedAt = microUTC(item.UpdatedAt)
	item.ProjectID = p.projectFor(item.ProjectID)
	item.Labels = slices.Clone(item.Labels)
	item.ACL = slices.Clone(item.ACL)
	for _, field := range itemTextFields(&item) {
		report.add(p.cleanText(field))
	}
	var truncated bool
	item.Body, truncated = truncateBody(item.Body)
	if truncated {
		report.bodyTruncated = 1
	}
	item = item.WithHash()
	if err := item.Validate(); err != nil {
		return domain.Item{}, report, err
	}
	return item, report, nil
}

// cleanText fixes UTF-8, redacts secrets and, for personal-data sources,
// hashes e-mail addresses in place.
func (p ingestPolicy) cleanText(value *string) itemReport {
	var report itemReport
	text := strings.ToValidUTF8(*value, "�")
	text, report.redactions = RedactIngest(text)
	if p.personalData {
		text, report.emails = p.hashEmails(text)
	}
	*value = text
	return report
}

func (p ingestPolicy) hashEmails(text string) (string, int) {
	count := 0
	text = emailPattern.ReplaceAllStringFunc(text, func(address string) string {
		count++
		return EmailPlaceholder(p.connection.ID, address)
	})
	return text, count
}

// EmailPlaceholder is the stable ingest placeholder for an e-mail address of
// a personal-data connection: "[email:" + 12 hex of sha256(connectionID NUL
// lower(address)) + "]".
func EmailPlaceholder(connectionID, address string) string {
	sum := sha256.Sum256([]byte(connectionID + "\x00" + strings.ToLower(address)))
	return "[email:" + hex.EncodeToString(sum[:])[:12] + "]"
}

func (p ingestPolicy) projectFor(projectID string) string {
	projectID = strings.TrimSpace(projectID)
	if projectID != "" && slices.Contains(p.connection.Projects, projectID) {
		return projectID
	}
	if len(p.connection.Projects) == 1 {
		return p.connection.Projects[0]
	}
	return ""
}

// itemTextFields lists every free-text field of an item for cleaning. Labels
// must already be a private copy.
func itemTextFields(item *domain.Item) []*string {
	fields := []*string{&item.Title, &item.Body, &item.URL, &item.Author}
	for index := range item.Labels {
		fields = append(fields, &item.Labels[index])
	}
	return fields
}

func microUTC(at time.Time) time.Time {
	if at.IsZero() {
		return time.Time{}
	}
	return at.UTC().Truncate(time.Microsecond)
}

// truncateBody cuts body to domain.MaxItemBodyBytes including the marker,
// on a UTF-8 rune boundary. It reports whether it cut.
func truncateBody(body string) (string, bool) {
	if len(body) <= domain.MaxItemBodyBytes {
		return body, false
	}
	// The marker with the largest possible kept size bounds its own length.
	keep := domain.MaxItemBodyBytes - len(fmt.Sprintf(ingestTruncationMarker, domain.MaxItemBodyBytes, len(body)))
	for keep > 0 && !utf8.RuneStart(body[keep]) {
		keep--
	}
	return body[:keep] + fmt.Sprintf(ingestTruncationMarker, keep, len(body)), true
}
