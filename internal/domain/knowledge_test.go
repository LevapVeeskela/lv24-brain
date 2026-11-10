package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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

func testSourceRef() SourceRef {
	return SourceRef{
		ProjectID:    "lv24-brain",
		ConnectionID: "gh-acme",
		ItemKey:      "gh-acme/" + strings.Repeat("a", itemKeyHashLength),
		ExternalID:   "acme/widgets#issue-7",
		URL:          "https://github.test/acme/widgets/issues/7",
		ObservedAt:   proposalTestTime,
		ProposalID:   ProposalID("lv24-brain", "gh-acme", "acme/widgets#issue-7"),
		Confidence:   0.82,
	}
}

func TestSourceRefValidate(t *testing.T) {
	good := testSourceRef()
	if err := good.Validate(); err != nil {
		t.Fatalf("valid source: %v", err)
	}
	for name, mutate := range map[string]func(*SourceRef){
		"empty":              func(s *SourceRef) { *s = SourceRef{} },
		"connection only":    func(s *SourceRef) { *s = SourceRef{ConnectionID: "gh-acme"} },
		"bad connection":     func(s *SourceRef) { s.ConnectionID = "GH" },
		"item key mismatch":  func(s *SourceRef) { s.ItemKey = "gh-other/" + strings.Repeat("a", itemKeyHashLength) },
		"bad item key":       func(s *SourceRef) { s.ItemKey = "gh-acme/not-a-hash" },
		"bad external":       func(s *SourceRef) { s.ExternalID = "a\x00b" },
		"bad url":            func(s *SourceRef) { s.URL = "not a url" },
		"credentials in url": func(s *SourceRef) { s.URL = "https://user:pass@example.test/x" },
		"bad proposal":       func(s *SourceRef) { s.ProposalID = "proposal-gh-acme-xyz" },
		"confidence low":     func(s *SourceRef) { s.Confidence = -0.5 },
		"confidence high":    func(s *SourceRef) { s.Confidence = 2 },
		"padded project":     func(s *SourceRef) { s.ProjectID = " lv24-brain" },
	} {
		t.Run(name, func(t *testing.T) {
			source := testSourceRef()
			mutate(&source)
			if err := source.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("Validate() = %v, want validation error", err)
			}
		})
	}
}

func TestRecordSourceValidation(t *testing.T) {
	source := testSourceRef()
	note := Note{ProjectID: "lv24-brain", Body: "body", Source: &source}
	if err := note.Validate(); err != nil {
		t.Fatalf("note: %v", err)
	}
	decision := Decision{ProjectID: "lv24-brain", Title: "Keep", Source: &source}
	if err := decision.Validate(); err != nil {
		t.Fatalf("decision: %v", err)
	}
	attempt := Attempt{ProjectID: "lv24-brain", Title: "Try", Source: &source}
	if err := attempt.Validate(); err != nil {
		t.Fatalf("attempt: %v", err)
	}

	otherProject := testSourceRef()
	otherProject.ProjectID = "other"
	for name, record := range map[string]interface{ Validate() error }{
		"note":     Note{ProjectID: "lv24-brain", Body: "body", Source: &otherProject},
		"decision": Decision{ProjectID: "lv24-brain", Title: "Keep", Source: &otherProject},
		"attempt":  Attempt{ProjectID: "lv24-brain", Title: "Try", Source: &otherProject},
	} {
		if err := record.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: Validate() = %v, want validation error", name, err)
		}
	}
	empty := SourceRef{}
	if err := (Decision{ProjectID: "lv24-brain", Title: "Keep", Source: &empty}).Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty source pointer: %v", err)
	}
}

func TestRecordSourceJSONIsOptional(t *testing.T) {
	plain, err := json.Marshal(Note{ProjectID: "p", Body: "b"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(plain), `"source"`) {
		t.Fatalf("note without source encoded a source field: %s", plain)
	}

	source := testSourceRef()
	note := Note{ProjectID: "lv24-brain", Body: "body", Source: &source, CreatedAt: proposalTestTime}
	raw, err := json.Marshal(note)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Note
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded.Source, note.Source) {
		t.Fatalf("source round trip = %+v, want %+v", decoded.Source, note.Source)
	}
	if decoded.Source.ProposalID != source.ProposalID || !decoded.Source.ObservedAt.Equal(source.ObservedAt) {
		t.Fatalf("source fields = %+v", decoded.Source)
	}
}

func TestSourceRefZeroObservedAtJSONIsOmitted(t *testing.T) {
	raw, err := json.Marshal(SourceRef{ProposalID: ProposalID("p", "c", "x")})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "observedAt") {
		t.Fatalf("zero observedAt was encoded: %s", raw)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := decoded["confidence"]; ok {
		t.Fatalf("zero confidence was encoded: %s", raw)
	}
}
