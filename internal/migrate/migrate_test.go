package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

func writeFixture(t *testing.T, root, rel, content string) string {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", rel, err)
	}
	return full
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(raw)
}

func findResult(t *testing.T, result Result, sourceRel string) FileResult {
	t.Helper()
	for _, file := range result.Files {
		if file.SourcePath == sourceRel {
			return file
		}
	}
	t.Fatalf("result for %s not found in %+v", sourceRel, result.Files)
	return FileResult{}
}

func newLegacyFixture(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	writeFixture(t, source, "projects/lv24/state.md", `---
project_id: lv24
updated_at: 2026-11-05 03:55:30+03:00
branch: develop
head: 8acf1a0fb
status: active
permalink: company/projects/lv24/state
---

# lv24 — state

## Goal

Городской портал: карта, афиша и кино.

## Active

- Сканы безопасности
- Карта

## Blocked

- Прод-деплой

## Next action

Решение владельца.

## Open questions

- Включать N=2?

## Recent decisions

- SSE по умолчанию выключен.

## Evidence

docs/plans/scan.md
`)
	writeFixture(t, source, "projects/lv24/notes/2026-11-05.md", `---
project_id: lv24
kind: notes
date: 2026-11-05
permalink: company/projects/lv24/notes/2026-11-05
---

# Notes 2026-11-05

## 01:33

Инициализирован company brain: 16 проектов в реестре.

## 05:07

Проверка кириллицы: ёжик, ў, і.
`)
	writeFixture(t, source, "projects/lv24/decisions/2026-11-04-use-postgres.md", `---
id: dec:lv24:use-postgres
kind: decision
project_id: lv24
status: accepted
created_at: 2026-11-04 10:00:00+03:00
valid_from: 2026-11-04
supersedes: []
source_refs:
  - docs/plans/db.md
confidence: high
---

# Использовать PostgreSQL

Вводный абзац.

## Context

Нужен серверный режим.

## Decision

Postgres — канон.

## Alternatives considered

- SQLite
- Файлы

## Rejected approaches

- Mongo

## Consequences

- Нужны миграции.

## Revisit triggers

- Рост нагрузки.

## Evidence

- docs/product-design.md
`)
	writeFixture(t, source, "projects/lv24/attempts/2026-11-03-deploy-fail.md", `---
id: att:lv24:deploy-fail
kind: attempt
project_id: lv24
status: failed
observed_at: 2026-11-03
files:
  - infra/deploy.ps1
evidence:
  - log line 42
---

# Неудачный деплой

## Symptom

504 на edge.

## Hypothesis

Неверный порт.

## What we tried

Перезапуск nginx.

## Outcome / why it failed

Конфликт конфигов.

## What to try next

Сверить compose-файлы.
`)
	writeFixture(t, source, "projects/lv24/handoffs/2026-11-05-0505.md", `---
id: ho:lv24:2026-11-05
kind: handoff
project_id: lv24
created_at: 2026-11-05 05:05:07.156567+03:00
branch: develop
head: a74193008
dirty_files: 6
status: open
permalink: company/projects/lv24/handoffs/2026-11-05-0505
---

# Handoff 2026-11-05

## Objective

Auto handoff: OpenCode session checkpoint.

## Completed

- Импорт.

## Unfinished

- Соак.

## Blockers

- Нет.

## Next action

Проверить импорт.

## Evidence

- report.md

## Git log

- a74193008 [LV24](docs) - record security scans
`)
	writeFixture(t, source, "projects/other/state.md", `---
project_id: other
updated_at: 2026-11-01T00:00:00Z
---

# other — state

## Goal

Second project.
`)
	writeFixture(t, source, "mail/2026-11-05.md", "# Mail\n\nDo not import.\n")
	writeFixture(t, source, "projects/lv24/mail/2026-11-05.md", "# Project mail\n\nDo not import.\n")
	writeFixture(t, source, "projects/lv24/git/github.md", "# Git metadata\n\nDo not import.\n")
	writeFixture(t, source, "journal/lv24.jsonl", `{"note":"do not import"}`)
	writeFixture(t, source, "registry.json", `{"version":1,"projects":[]}`)
	return source
}

func TestRunImportsKnowledgeAndNormalizesFrontmatter(t *testing.T) {
	source := newLegacyFixture(t)
	target := filepath.Join(t.TempDir(), "data")

	dry, err := Run(context.Background(), Options{SourceDir: source, TargetDir: target, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Created != 6 || dry.Unchanged != 0 || dry.Conflicts != 0 || dry.Failed != 0 {
		t.Fatalf("dry run counts = created %d unchanged %d conflicts %d failed %d", dry.Created, dry.Unchanged, dry.Conflicts, dry.Failed)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("dry run created target directory: %v", statErr)
	}

	result, err := Run(context.Background(), Options{SourceDir: source, TargetDir: target})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Created != 6 || result.Unchanged != 0 || result.Conflicts != 0 || result.Failed != 0 {
		t.Fatalf("run counts = created %d unchanged %d conflicts %d failed %d", result.Created, result.Unchanged, result.Conflicts, result.Failed)
	}
	if strings.Join(result.Projects, ",") != "lv24,other" {
		t.Fatalf("projects = %v", result.Projects)
	}

	if _, statErr := os.Stat(filepath.Join(target, "mail")); !os.IsNotExist(statErr) {
		t.Fatalf("mail directory was imported")
	}
	if _, statErr := os.Stat(filepath.Join(target, "journal")); !os.IsNotExist(statErr) {
		t.Fatalf("journal directory was imported")
	}
	if _, statErr := os.Stat(filepath.Join(target, "registry.json")); !os.IsNotExist(statErr) {
		t.Fatalf("registry.json was imported")
	}
	if _, statErr := os.Stat(filepath.Join(target, "projects", "lv24", "mail")); !os.IsNotExist(statErr) {
		t.Fatalf("project mail was imported")
	}
	if _, statErr := os.Stat(filepath.Join(target, "projects", "lv24", "git")); !os.IsNotExist(statErr) {
		t.Fatalf("project git metadata was imported")
	}

	store, err := fsstore.New(target)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	state, err := store.GetState(context.Background(), "lv24")
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state.Goal != "Городской портал: карта, афиша и кино." {
		t.Fatalf("goal = %q", state.Goal)
	}
	if len(state.Active) != 2 || state.Active[0] != "Сканы безопасности" {
		t.Fatalf("active = %#v", state.Active)
	}
	if len(state.Blocked) != 1 || state.Blocked[0] != "Прод-деплой" {
		t.Fatalf("blocked = %#v", state.Blocked)
	}
	wantUpdated := time.Date(2026, 11, 5, 0, 55, 30, 0, time.UTC)
	if !state.UpdatedAt.Equal(wantUpdated) {
		t.Fatalf("state updated_at = %v, want %v", state.UpdatedAt, wantUpdated)
	}
	stateRaw := readFixture(t, filepath.Join(target, "projects", "lv24", "state.md"))
	if !strings.Contains(stateRaw, "kind: state") || !strings.Contains(stateRaw, "schema_version: 1") {
		t.Fatalf("state frontmatter not normalized:\n%s", stateRaw)
	}
	if !strings.Contains(stateRaw, "## Recent decisions") || !strings.Contains(stateRaw, "docs/plans/scan.md") {
		t.Fatalf("state extra sections were dropped:\n%s", stateRaw)
	}

	notesPath := filepath.Join(target, "projects", "lv24", "notes", "2026-11-05.md")
	notesRaw := readFixture(t, notesPath)
	var notesFrontmatter noteFileFrontmatter
	notesBody, err := markdown.Parse([]byte(notesRaw), &notesFrontmatter)
	if err != nil {
		t.Fatalf("parse notes: %v", err)
	}
	if notesFrontmatter.Kind != "note" || notesFrontmatter.SchemaVersion != SchemaVersion {
		t.Fatalf("notes frontmatter = %+v", notesFrontmatter)
	}
	if notesFrontmatter.Date != "2026-11-05" {
		t.Fatalf("notes date = %q", notesFrontmatter.Date)
	}
	wantNoteUpdated := time.Date(2026, 11, 5, 5, 7, 0, 0, time.UTC)
	if !notesFrontmatter.UpdatedAt.Equal(wantNoteUpdated) {
		t.Fatalf("notes updated_at = %v, want %v", notesFrontmatter.UpdatedAt, wantNoteUpdated)
	}
	if !strings.Contains(notesBody, "Инициализирован company brain") || !strings.Contains(notesBody, "ёжик, ў, і") {
		t.Fatalf("note content missing:\n%s", notesBody)
	}
	first := parseMarkerLine(t, notesBody)
	if first.ID != "note-2026-11-05-01-33" {
		t.Fatalf("first note id = %q", first.ID)
	}
	if !first.CreatedAt.Equal(time.Date(2026, 11, 5, 1, 33, 0, 0, time.UTC)) {
		t.Fatalf("first note created_at = %v", first.CreatedAt)
	}

	decisionPath := filepath.Join(target, "projects", "lv24", "decisions", "2026-11-04-use-postgres.md")
	decisionRaw := readFixture(t, decisionPath)
	var decisionMeta decisionFrontmatter
	decisionBody, err := markdown.Parse([]byte(decisionRaw), &decisionMeta)
	if err != nil {
		t.Fatalf("parse decision: %v", err)
	}
	if decisionMeta.Kind != "decision" || decisionMeta.SchemaVersion != SchemaVersion {
		t.Fatalf("decision frontmatter = %+v", decisionMeta)
	}
	if decisionMeta.ID != "dec:lv24:use-postgres" {
		t.Fatalf("decision id = %q", decisionMeta.ID)
	}
	if decisionMeta.Status != "accepted" {
		t.Fatalf("decision status = %q", decisionMeta.Status)
	}
	if !decisionMeta.CreatedAt.Equal(time.Date(2026, 11, 4, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("decision created_at = %v", decisionMeta.CreatedAt)
	}
	if !strings.Contains(decisionBody, "Вводный абзац.") {
		t.Fatalf("decision preamble was dropped:\n%s", decisionBody)
	}
	if !strings.Contains(decisionBody, "### Decision") || !strings.Contains(decisionBody, "Postgres — канон.") {
		t.Fatalf("decision body lost the decision section:\n%s", decisionBody)
	}
	if !strings.Contains(decisionBody, "Rejected: Mongo") {
		t.Fatalf("decision body lost rejected approaches:\n%s", decisionBody)
	}
	if !strings.Contains(decisionBody, "Revisit: Рост нагрузки.") {
		t.Fatalf("decision body lost revisit triggers:\n%s", decisionBody)
	}
	if !strings.Contains(decisionBody, "docs/plans/db.md") || !strings.Contains(decisionBody, "docs/product-design.md") {
		t.Fatalf("decision evidence missing:\n%s", decisionBody)
	}

	attemptPath := filepath.Join(target, "projects", "lv24", "attempts", "2026-11-03-deploy-fail.md")
	var attemptMeta attemptFrontmatter
	attemptBody, err := markdown.Parse([]byte(readFixture(t, attemptPath)), &attemptMeta)
	if err != nil {
		t.Fatalf("parse attempt: %v", err)
	}
	if attemptMeta.ID != "att:lv24:deploy-fail" || attemptMeta.Kind != "attempt" {
		t.Fatalf("attempt frontmatter = %+v", attemptMeta)
	}
	if !attemptMeta.CreatedAt.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("attempt created_at = %v", attemptMeta.CreatedAt)
	}
	for _, want := range []string{"504 на edge.", "Перезапуск nginx.", "Конфликт конфигов.", "Сверить compose-файлы.", "infra/deploy.ps1"} {
		if !strings.Contains(attemptBody, want) {
			t.Fatalf("attempt body lost %q:\n%s", want, attemptBody)
		}
	}

	handoffPath := filepath.Join(target, "projects", "lv24", "handoffs", "2026-11-05-0505.md")
	var handoffMeta handoffFrontmatter
	handoffBody, err := markdown.Parse([]byte(readFixture(t, handoffPath)), &handoffMeta)
	if err != nil {
		t.Fatalf("parse handoff: %v", err)
	}
	if handoffMeta.ID != "ho:lv24:2026-11-05" || handoffMeta.Kind != "handoff" {
		t.Fatalf("handoff frontmatter = %+v", handoffMeta)
	}
	wantHandoffTime := time.Date(2026, 11, 5, 2, 5, 7, 156567000, time.UTC)
	if !handoffMeta.CreatedAt.Equal(wantHandoffTime) {
		t.Fatalf("handoff created_at = %v, want %v", handoffMeta.CreatedAt, wantHandoffTime)
	}
	for _, want := range []string{"Auto handoff: OpenCode session checkpoint.", "Импорт.", "Соак.", "report.md", "Проверить импорт.", "branch: develop", "a74193008 [LV24](docs)"} {
		if !strings.Contains(handoffBody, want) {
			t.Fatalf("handoff body lost %q:\n%s", want, handoffBody)
		}
	}
}

func parseMarkerLine(t *testing.T, body string) noteMetadata {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, noteMarkerPrefix) || !strings.HasSuffix(trimmed, noteMarkerSuffix) {
			continue
		}
		payload := strings.TrimSuffix(strings.TrimPrefix(trimmed, noteMarkerPrefix), noteMarkerSuffix)
		var metadata noteMetadata
		if err := json.Unmarshal([]byte(payload), &metadata); err != nil {
			t.Fatalf("note marker: %v", err)
		}
		return metadata
	}
	t.Fatalf("note marker not found in:\n%s", body)
	return noteMetadata{}
}

func TestRunIsIdempotent(t *testing.T) {
	source := newLegacyFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	if _, err := Run(context.Background(), Options{SourceDir: source, TargetDir: target}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	decisionPath := filepath.Join(target, "projects", "lv24", "decisions", "2026-11-04-use-postgres.md")
	before := readFixture(t, decisionPath)

	second, err := Run(context.Background(), Options{SourceDir: source, TargetDir: target})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.Created != 0 || second.Unchanged != 6 || second.Conflicts != 0 || second.Failed != 0 {
		t.Fatalf("second run counts = created %d unchanged %d conflicts %d failed %d", second.Created, second.Unchanged, second.Conflicts, second.Failed)
	}
	if after := readFixture(t, decisionPath); after != before {
		t.Fatalf("second run changed target content")
	}
}

func TestConflictDoesNotOverwrite(t *testing.T) {
	source := newLegacyFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	if _, err := Run(context.Background(), Options{SourceDir: source, TargetDir: target}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	decisionPath := filepath.Join(target, "projects", "lv24", "decisions", "2026-11-04-use-postgres.md")
	edited := readFixture(t, decisionPath) + "\nLocal edit kept.\n"
	if err := os.WriteFile(decisionPath, []byte(edited), 0o644); err != nil {
		t.Fatalf("edit target: %v", err)
	}

	result, err := Run(context.Background(), Options{SourceDir: source, TargetDir: target})
	var incomplete *IncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("expected IncompleteError, got %v", err)
	}
	if incomplete.Conflicts != 1 || incomplete.Failed != 0 {
		t.Fatalf("IncompleteError = %+v", incomplete)
	}
	conflict := findResult(t, result, "projects/lv24/decisions/2026-11-04-use-postgres.md")
	if conflict.Status != StatusConflict {
		t.Fatalf("decision status = %q", conflict.Status)
	}
	if after := readFixture(t, decisionPath); !strings.Contains(after, "Local edit kept.") {
		t.Fatalf("conflict overwrote the target file")
	}
}

func TestMalformedFrontmatterIsReportedAndOtherFilesContinue(t *testing.T) {
	source := t.TempDir()
	writeFixture(t, source, "projects/lv24/notes/2026-11-05.md", `---
project_id: lv24
kind: notes
date: 2026-11-05
---

# Notes

## 10:00

Valid note.
`)
	writeFixture(t, source, "projects/lv24/decisions/2026-11-04-broken.md", `---
project_id: [unterminated
---

# Broken
`)
	target := filepath.Join(t.TempDir(), "data")
	result, err := Run(context.Background(), Options{SourceDir: source, TargetDir: target})
	var incomplete *IncompleteError
	if !errors.As(err, &incomplete) || incomplete.Failed != 1 {
		t.Fatalf("expected one failure, got %v", err)
	}
	failed := findResult(t, result, "projects/lv24/decisions/2026-11-04-broken.md")
	if failed.Status != StatusFailed || failed.Error == "" {
		t.Fatalf("failed result = %+v", failed)
	}
	note := findResult(t, result, "projects/lv24/notes/2026-11-05.md")
	if note.Status != StatusCreated {
		t.Fatalf("valid note status = %q", note.Status)
	}
	if _, statErr := os.Stat(filepath.Join(target, "projects", "lv24", "notes", "2026-11-05.md")); statErr != nil {
		t.Fatalf("valid note was not imported: %v", statErr)
	}
}

func TestMissingFrontmatterIsReported(t *testing.T) {
	source := t.TempDir()
	writeFixture(t, source, "projects/lv24/notes/2026-11-05.md", "# Notes\n\n## 10:00\n\nNo frontmatter.\n")
	result, err := Run(context.Background(), Options{SourceDir: source, TargetDir: filepath.Join(t.TempDir(), "data")})
	var incomplete *IncompleteError
	if !errors.As(err, &incomplete) || incomplete.Failed != 1 {
		t.Fatalf("expected one failure, got %v (%+v)", err, result)
	}
}

func TestInvalidProjectIDIsRejected(t *testing.T) {
	source := t.TempDir()
	writeFixture(t, source, "projects/bad id/state.md", "---\nproject_id: bad id\n---\n\n# state\n")
	writeFixture(t, source, "projects/good/state.md", "---\nproject_id: good\n---\n\n# state\n")
	target := filepath.Join(t.TempDir(), "data")
	result, err := Run(context.Background(), Options{SourceDir: source, TargetDir: target})
	var incomplete *IncompleteError
	if !errors.As(err, &incomplete) || incomplete.Failed != 1 {
		t.Fatalf("expected one failure, got %v", err)
	}
	invalid := findResult(t, result, "projects/bad id")
	if invalid.Status != StatusFailed {
		t.Fatalf("invalid project status = %q", invalid.Status)
	}
	if _, statErr := os.Stat(filepath.Join(target, "projects", "bad id")); !os.IsNotExist(statErr) {
		t.Fatalf("invalid project directory was created")
	}
	if _, statErr := os.Stat(filepath.Join(target, "projects", "good", "state.md")); statErr != nil {
		t.Fatalf("valid project was not imported: %v", statErr)
	}
}

func TestRejectsOverlappingDirectories(t *testing.T) {
	source := t.TempDir()
	writeFixture(t, source, "projects/lv24/state.md", "---\nproject_id: lv24\n---\n\n# state\n")
	if _, err := Run(context.Background(), Options{SourceDir: source, TargetDir: source}); err == nil {
		t.Fatalf("same source and target must fail")
	}
	if _, err := Run(context.Background(), Options{SourceDir: source, TargetDir: filepath.Join(source, "out")}); err == nil {
		t.Fatalf("target nested in source must fail")
	}
	if _, err := Run(context.Background(), Options{SourceDir: filepath.Join(source, "projects"), TargetDir: filepath.Join(source, "projects")}); err == nil {
		t.Fatalf("same nested source and target must fail")
	}
}

func TestMissingProjectsDirectoryFails(t *testing.T) {
	source := t.TempDir()
	if _, err := Run(context.Background(), Options{SourceDir: source, TargetDir: filepath.Join(t.TempDir(), "data")}); err == nil {
		t.Fatalf("expected an error for a directory without projects/")
	}
}

func TestSymlinkedProjectIsNotFollowed(t *testing.T) {
	outside := t.TempDir()
	writeFixture(t, outside, "state.md", "---\nproject_id: escape\n---\n\n# escaped\n")
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "projects"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "projects", "escape")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	target := filepath.Join(t.TempDir(), "data")
	result, err := Run(context.Background(), Options{SourceDir: source, TargetDir: target})
	var incomplete *IncompleteError
	if !errors.As(err, &incomplete) || incomplete.Failed != 1 {
		t.Fatalf("expected one failure, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(target, "projects", "escape", "state.md")); !os.IsNotExist(statErr) {
		t.Fatalf("symlinked project was imported")
	}
	_ = result
}

func TestParseNoteEntriesGeneratesUniqueIDs(t *testing.T) {
	date := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	entries := parseNoteEntries("# Notes 2026-11-05\n\n## 01:33\n\nA\n\n## 01:33\n\nB\n", date)
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].id != "note-2026-11-05-01-33" {
		t.Fatalf("first id = %q", entries[0].id)
	}
	if entries[1].id != "note-2026-11-05-01-33-2" {
		t.Fatalf("second id = %q", entries[1].id)
	}
	if !entries[1].createdAt.Equal(time.Date(2026, 11, 5, 1, 33, 0, 0, time.UTC)) {
		t.Fatalf("second created_at = %v", entries[1].createdAt)
	}
}

func TestLegacyStringListAcceptsScalar(t *testing.T) {
	frontmatter, _, err := parseLegacyDocument([]byte("---\nproject_id: p\nsource_refs: docs/a.md\nevidence: null\n---\n\n# t\n"), "x.md")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(frontmatter.SourceRefs) != 1 || frontmatter.SourceRefs[0] != "docs/a.md" {
		t.Fatalf("source_refs = %#v", frontmatter.SourceRefs)
	}
	if len(frontmatter.Evidence) != 0 {
		t.Fatalf("evidence = %#v", frontmatter.Evidence)
	}
}

func TestSchemaVersionMatchesFSStore(t *testing.T) {
	if SchemaVersion != fsstore.SchemaVersion {
		t.Fatalf("migrate schema version %d != fsstore schema version %d", SchemaVersion, fsstore.SchemaVersion)
	}
}

func TestParseLegacyTimeLayouts(t *testing.T) {
	want := time.Date(2026, 11, 5, 0, 55, 30, 0, time.UTC)
	parsed, ok := parseLegacyTime("2026-11-05 03:55:30+03:00")
	if !ok || !parsed.Equal(want) {
		t.Fatalf("parsed = %v ok=%t", parsed, ok)
	}
	if _, ok := parseLegacyTime("null"); ok {
		t.Fatalf("null must not parse")
	}
}
