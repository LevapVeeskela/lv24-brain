package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/app"
)

func TestDoctorReportsDuplicateRecordIDs(t *testing.T) {
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
	idsCheck := func() app.DoctorCheck {
		report, err := a.Doctor(ctx, app.DoctorOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, check := range report.Checks {
			if check.Name == "ids" {
				return check
			}
		}
		t.Fatal("no ids check")
		return app.DoctorCheck{}
	}
	if check := idsCheck(); check.Status != "ok" {
		t.Fatalf("unique ids = %+v", check)
	}

	// A legacy tool gave two handoffs the same per-day id.
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
	if check := idsCheck(); check.Status != "warn" || !strings.Contains(check.Message, first.Handoff.ID) {
		t.Fatalf("duplicate ids = %+v", check)
	}
}
