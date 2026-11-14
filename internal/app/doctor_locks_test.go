package app_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
)

func doctorCheck(t *testing.T, report app.DoctorReport, name string) app.DoctorCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("doctor has no %q check", name)
	return app.DoctorCheck{}
}

func TestDoctorReportsDuplicateIDCode(t *testing.T) {
	ctx := context.Background()
	a, dataDir := newJournalApp(t)
	first, err := a.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "one", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "two"})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dataDir, "projects", "p", "handoffs", "*.md"))
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), second.Handoff.ID) {
			fixed := strings.Replace(string(raw), second.Handoff.ID, first.Handoff.ID, 1)
			if err := os.WriteFile(file, []byte(fixed), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	report, err := a.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	check := doctorCheck(t, report, "ids")
	if check.Status != "warn" || check.Code != "duplicate-id" {
		t.Fatalf("ids check = %+v, want warn with code duplicate-id", check)
	}
	if !strings.Contains(check.Message, first.Handoff.ID) {
		t.Fatalf("ids message %q does not name the duplicate id", check.Message)
	}
}

func TestDoctorReportsStaleLocks(t *testing.T) {
	ctx := context.Background()
	a, dataDir := newJournalApp(t)
	lockPath := filepath.Join(dataDir, "projects", "p", "state.md.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"pid":        os.Getpid(),
		"boot_id":    "another-boot",
		"host":       "other-machine",
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"ttl":        60,
		"token":      "pulled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := a.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	check := doctorCheck(t, report, "locks")
	if check.Status != "warn" || check.Code != "stale-lock" {
		t.Fatalf("locks check = %+v, want warn with code stale-lock", check)
	}
	if !strings.Contains(check.Message, "state.md.lock") {
		t.Fatalf("locks message %q does not name the stale file", check.Message)
	}
}
