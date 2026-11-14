package domain

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// RecordIDULIDLength is the Crockford base32 length of the ULID suffix of a
// record id.
const RecordIDULIDLength = 26

// ulidAlphabet is Crockford base32 in lower case: no I, L, O or U, so a
// case-insensitive filesystem cannot fold two ids together.
const ulidAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// recordIDDateLayout is the UTC date segment of a record id.
const recordIDDateLayout = "2006-01-02"

// NewULID returns a lowercase 26-character ULID: 48 bits of milliseconds from
// at followed by 80 random bits. The random part is generated with crypto/rand,
// so two writers never need to coordinate and a retry never reuses an id.
func NewULID(at time.Time) (string, error) {
	var raw [16]byte
	ms := uint64(at.UTC().UnixMilli())
	for i := 5; i >= 0; i-- {
		raw[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", fmt.Errorf("domain: generate record ulid: %w", err)
	}
	return encodeULID(raw), nil
}

// encodeULID writes 128 bits as 26 base32 characters (the first character
// carries two leading zero bits).
func encodeULID(raw [16]byte) string {
	out := make([]byte, RecordIDULIDLength)
	for i := range out {
		var value byte
		for bit := 0; bit < 5; bit++ {
			value <<= 1
			position := i*5 + bit
			if position < 2 {
				continue // the 130-bit form is left-padded to 128 bits
			}
			rawBit := position - 2
			value |= (raw[rawBit/8] >> (7 - uint(rawBit%8))) & 1
		}
		out[i] = ulidAlphabet[value]
	}
	return string(out)
}

// IsULID reports whether value is a 26-character lowercase Crockford base32
// ULID.
func IsULID(value string) bool {
	if len(value) != RecordIDULIDLength {
		return false
	}
	for i := 0; i < len(value); i++ {
		if !strings.ContainsRune(ulidAlphabet, rune(value[i])) {
			return false
		}
	}
	return true
}

// NewRecordID joins a record kind, a UTC date, a slug and a ULID into the
// globally unique record id "<kind>-<YYYY-MM-DD>-<slug>-<ULID>". The id is
// also the file name stem of a decision, attempt or handoff, so the slug must
// already be a path-safe ASCII string (Slugify).
func NewRecordID(kind string, date time.Time, slug, ulid string) string {
	return strings.ToLower(kind) + "-" + date.UTC().Format(recordIDDateLayout) + "-" +
		slug + "-" + strings.ToLower(strings.TrimSpace(ulid))
}

// IsRecordID reports whether id has the globally unique
// "<kind>-<YYYY-MM-DD>-<slug>-<ULID>" shape. Legacy dated ids and hand-pinned
// ids do not match, so a store can keep their old naming and suffix rules.
//
// The check is strict because the id becomes a file name: the slug may contain
// only lowercase letters, digits and dashes, and may not be "..".
func IsRecordID(kind, id string) bool {
	rest, ok := strings.CutPrefix(id, strings.ToLower(kind)+"-")
	if !ok {
		return false
	}
	if len(rest) < len(recordIDDateLayout)+1+1+1+RecordIDULIDLength {
		return false
	}
	ulidStart := len(rest) - RecordIDULIDLength
	if !IsULID(rest[ulidStart:]) {
		return false
	}
	if rest[ulidStart-1] != '-' {
		return false
	}
	if rest[len(recordIDDateLayout)] != '-' {
		return false
	}
	if _, err := time.Parse(recordIDDateLayout, rest[:len(recordIDDateLayout)]); err != nil {
		return false
	}
	slug := rest[len(recordIDDateLayout)+1 : ulidStart-1]
	return validRecordIDSlug(slug)
}

func validRecordIDSlug(slug string) bool {
	if slug == "" {
		return false
	}
	for i := 0; i < len(slug); i++ {
		char := slug[i]
		allowed := char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-'
		if !allowed {
			return false
		}
	}
	return true
}
