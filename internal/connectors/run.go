// Package connectors holds shared plumbing for read-only external connectors:
// run records, HTTP retry/rate-limit handling, Markdown sinks and formatting.
package connectors

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const maxRunErrorLength = 500

// NewRun starts a connector run record. The caller finishes it with FinishRun.
func NewRun(source string, at time.Time) domain.ConnectorRun {
	at = normalizeTime(at)
	return domain.ConnectorRun{
		ID:        newRunID(at),
		Source:    source,
		StartedAt: at,
		Status:    domain.ConnectorRunning,
		Counts:    map[string]int{},
	}
}

// FinishRun sets the terminal status, counts and a sanitized error on a run.
//
// A nil error means success. An error with at least one positive count means
// partial success. Secrets passed in secrets are removed from the error text
// before it is stored on the run.
func FinishRun(run domain.ConnectorRun, counts map[string]int, runErr error, at time.Time, secrets ...string) domain.ConnectorRun {
	finished := normalizeTime(at)
	if run.StartedAt.IsZero() {
		run.StartedAt = finished
	}
	run.FinishedAt = &finished
	run.Counts = cleanCounts(counts)
	run.Error = ""

	if runErr == nil {
		run.Status = domain.ConnectorSuccess
		return run
	}

	run.Error = SanitizeError(runErr, secrets...)
	run.Status = domain.ConnectorFailed
	for _, count := range run.Counts {
		if count > 0 {
			run.Status = domain.ConnectorPartial
			break
		}
	}
	return run
}

// SanitizeError renders an error for a ConnectorRun or a caller-facing return
// value. Secrets are replaced before truncation so a token can never survive
// just past a length boundary.
func SanitizeError(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	message = sanitizeText(message, maxRunErrorLength)
	if message == "" {
		return "connector failed"
	}
	return message
}

// SanitizeText removes control characters, collapses whitespace and truncates
// s to max runes. It never adds content.
func SanitizeText(s string, max int) string {
	return sanitizeText(s, max)
}

func sanitizeText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len([]rune(s)) > max {
		s = string([]rune(s)[:max])
	}
	return strings.TrimSpace(s)
}

func cleanCounts(counts map[string]int) map[string]int {
	cleaned := make(map[string]int, len(counts))
	for key, value := range counts {
		if value > 0 {
			cleaned[key] = value
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

func normalizeTime(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now().UTC()
	}
	return at.UTC()
}

func newRunID(at time.Time) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("run-%d", at.UnixNano())
	}
	return "run-" + hex.EncodeToString(raw[:])
}
