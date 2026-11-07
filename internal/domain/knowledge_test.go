package domain

import (
	"errors"
	"reflect"
	"testing"
)

func TestNormalizePaths(t *testing.T) {
	got, err := NormalizePaths([]string{"  internal/app/write.go ", "", "   ", `cmd\brain\`, "docs/"})
	if err != nil {
		t.Fatalf("NormalizePaths: %v", err)
	}
	want := []string{"internal/app/write.go", "cmd/brain/", "docs/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestNormalizePathsRejects(t *testing.T) {
	for _, bad := range []string{"/etc/passwd", `C:\secrets`, "C:/x", "../outside.go", "internal/../../x", `..\x`} {
		_, err := NormalizePaths([]string{"ok.go", bad})
		var validation *ValidationError
		if !errors.As(err, &validation) || validation.Field != "decision.paths" {
			t.Fatalf("NormalizePaths(%q) error = %v, want decision.paths validation error", bad, err)
		}
	}
}

func TestDecisionValidateChecksPaths(t *testing.T) {
	valid := Decision{ProjectID: "p", Title: "Keep", Paths: []string{"a/b.go"}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	invalid := valid
	invalid.Paths = []string{"../escape"}
	if err := invalid.Validate(); err == nil {
		t.Fatal("Validate accepted a path with ..")
	}
}
