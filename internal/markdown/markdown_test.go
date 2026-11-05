package markdown

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRenderParseRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 5, 10, 30, 0, 0, time.UTC)
	header := Frontmatter{
		ProjectID:     "lv24-brain",
		Kind:          "decision",
		UpdatedAt:     at,
		SchemaVersion: 1,
		Permalink:     "/projects/lv24-brain/decisions/demo",
	}
	body := "# Решение\n\nТекст на кириллице 🧠\n\n---\n\nStill body text.\n"
	raw, err := Render(header, body)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(string(raw), "---\n") {
		t.Fatalf("rendered document does not start with frontmatter: %q", raw)
	}
	var got Frontmatter
	parsedBody, err := Parse(raw, &got)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got != header {
		t.Fatalf("frontmatter mismatch\n got: %+v\nwant: %+v", got, header)
	}
	if parsedBody != body {
		t.Fatalf("body mismatch\n got: %q\nwant: %q", parsedBody, body)
	}
}

func TestRenderAddsTrailingNewline(t *testing.T) {
	raw, err := Render(Frontmatter{ProjectID: "p", Kind: "note", SchemaVersion: 1}, "body without newline")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("rendered document must end with a newline: %q", raw)
	}
	var got Frontmatter
	body, err := Parse(raw, &got)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if body != "body without newline\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestParseEmptyBody(t *testing.T) {
	raw, err := Render(Frontmatter{ProjectID: "p", Kind: "state", SchemaVersion: 1}, "")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var got Frontmatter
	body, err := Parse(raw, &got)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if body != "" {
		t.Fatalf("body = %q, want empty", body)
	}
}

func TestParseRejectsMissingFrontmatter(t *testing.T) {
	_, err := Parse([]byte("# just markdown\n"), nil)
	if !errors.Is(err, ErrMissingFrontmatter) {
		t.Fatalf("error = %v, want ErrMissingFrontmatter", err)
	}
}

func TestParseRejectsUnterminatedFrontmatter(t *testing.T) {
	_, err := Parse([]byte("---\nproject_id: p\nkind: note\n"), nil)
	if !errors.Is(err, ErrUnterminatedFrontmatter) {
		t.Fatalf("error = %v, want ErrUnterminatedFrontmatter", err)
	}
}

func TestParseAcceptsBOMAndCRLF(t *testing.T) {
	document := "\ufeff---\r\nproject_id: lv24\r\nkind: note\r\nschema_version: 1\r\n---\r\n\r\nline one\r\nline two\r\n"
	var got Frontmatter
	body, err := Parse([]byte(document), &got)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.ProjectID != "lv24" || got.Kind != "note" || got.SchemaVersion != 1 {
		t.Fatalf("frontmatter = %+v", got)
	}
	if body != "line one\nline two\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestParseIgnoresUnknownFrontmatterKeys(t *testing.T) {
	document := "---\nproject_id: p\nkind: note\nschema_version: 1\nfuture_field: value\n---\nbody\n"
	var got Frontmatter
	if _, err := Parse([]byte(document), &got); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.ProjectID != "p" {
		t.Fatalf("frontmatter = %+v", got)
	}
}
