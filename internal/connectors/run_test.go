package connectors

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestFinishRunStatuses(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	success := FinishRun(NewRun("github", now), map[string]int{"repos": 1, "issues": 0}, nil, now)
	if success.Status != domain.ConnectorSuccess {
		t.Fatalf("success status = %q", success.Status)
	}
	if success.Counts["repos"] != 1 || len(success.Counts) != 1 {
		t.Fatalf("counts = %#v", success.Counts)
	}
	if success.FinishedAt == nil || !success.FinishedAt.Equal(now) {
		t.Fatalf("finishedAt = %v", success.FinishedAt)
	}

	partial := FinishRun(NewRun("github", now), map[string]int{"repos": 1}, errors.New("second repo failed"), now)
	if partial.Status != domain.ConnectorPartial || partial.Error != "second repo failed" {
		t.Fatalf("partial = %#v", partial)
	}

	failed := FinishRun(NewRun("github", now), nil, errors.New("boom"), now)
	if failed.Status != domain.ConnectorFailed || failed.Counts != nil {
		t.Fatalf("failed = %#v", failed)
	}
}

func TestFinishRunRedactsSecretsAndTruncates(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	secret := "ghp_supersecretvalue"
	err := errors.New("request failed for " + secret + ": " + strings.Repeat("x", 2000))
	run := FinishRun(NewRun("github", now), nil, err, now, secret)
	if strings.Contains(run.Error, secret) {
		t.Fatalf("run error leaked the token: %q", run.Error)
	}
	if !strings.Contains(run.Error, "[redacted]") {
		t.Fatalf("run error missing redaction marker: %q", run.Error)
	}
	if len([]rune(run.Error)) > maxRunErrorLength {
		t.Fatalf("run error length = %d", len([]rune(run.Error)))
	}
}

func TestNewRunZeroTimeUsesNow(t *testing.T) {
	run := NewRun("imap", time.Time{})
	if run.StartedAt.IsZero() || run.Status != domain.ConnectorRunning || run.ID == "" {
		t.Fatalf("run = %#v", run)
	}
}

func TestSanitizeTextRemovesControlCharacters(t *testing.T) {
	got := SanitizeText("a\x00b\n\tc", 20)
	if got != "a b c" {
		t.Fatalf("sanitized = %q", got)
	}
}
