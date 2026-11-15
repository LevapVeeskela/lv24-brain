package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validFixtureYAML = `version: 1
project: demo
records:
  - id: decision-2026-01-01-alpha
    kind: decision
    title: "Alpha widget rollout"
    status: accepted
    created_at: 2026-01-01T00:00:00Z
    context: "The alpha widget deploy pipeline."
tasks:
  - id: t-alpha
    query: "alpha widget"
    expected: [decision-2026-01-01-alpha]
`

func TestLoadDirReadsFixturesInOrder(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "b.yaml", strings.NewReplacer("project: demo", "project: second", "t-alpha", "t-beta").Replace(validFixtureYAML))
	writeFixtureFile(t, dir, "a.yml", validFixtureYAML)
	writeFixtureFile(t, dir, "notes.txt", "ignored")

	corpus, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(corpus.Fixtures) != 2 {
		t.Fatalf("fixtures = %d, want 2", len(corpus.Fixtures))
	}
	if got := filepath.Base(corpus.Fixtures[0].Path); got != "a.yml" {
		t.Fatalf("first fixture = %s, want a.yml (files must load in name order)", got)
	}
	if corpus.TaskCount != 2 || corpus.RecordCount != 2 {
		t.Fatalf("counts = %d tasks, %d records", corpus.TaskCount, corpus.RecordCount)
	}
	if len(corpus.Digest) != 64 {
		t.Fatalf("digest = %q, want a SHA-256 hex string", corpus.Digest)
	}
}

func TestParseFixtureRejectsUnknownFieldsAndDocuments(t *testing.T) {
	cases := map[string]string{
		"unknown field": `version: 1
project: demo
records:
  - id: decision-2026-01-01-alpha
    kind: decision
    title: "Alpha"
    created_at: 2026-01-01T00:00:00Z
    typo_field: "value"
tasks:
  - id: t
    query: "alpha"
    expected: [decision-2026-01-01-alpha]
`,
		"two documents": validFixtureYAML + "---\nversion: 1\n",
		"empty":         "",
		"bad yaml":      "version: [\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseFixture([]byte(raw)); err == nil {
				t.Fatal("ParseFixture accepted an invalid document")
			}
		})
	}
}

func TestCorpusValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Corpus)
		want   string
	}{
		"wrong version": {
			mutate: func(c *Corpus) { c.Fixtures[0].Version = 2 },
			want:   "version = 2",
		},
		"missing project": {
			mutate: func(c *Corpus) { c.Fixtures[0].Project = " " },
			want:   "project is required",
		},
		"duplicate project": {
			mutate: func(c *Corpus) {
				c.Fixtures = append(c.Fixtures, c.Fixtures[0])
			},
			want: "already defined",
		},
		"no records": {
			mutate: func(c *Corpus) { c.Fixtures[0].Records = nil },
			want:   "has no records",
		},
		"no tasks": {
			mutate: func(c *Corpus) { c.Fixtures[0].Tasks = nil },
			want:   "has no tasks",
		},
		"duplicate record id": {
			mutate: func(c *Corpus) {
				c.Fixtures[0].Records = append(c.Fixtures[0].Records, c.Fixtures[0].Records[0])
			},
			want: "defined twice",
		},
		"unknown kind": {
			mutate: func(c *Corpus) { c.Fixtures[0].Records[0].Kind = "memo" },
			want:   "kind",
		},
		"missing title": {
			mutate: func(c *Corpus) { c.Fixtures[0].Records[0].Title = "" },
			want:   "title is required",
		},
		"bad status": {
			mutate: func(c *Corpus) { c.Fixtures[0].Records[0].Status = "done" },
			want:   "status",
		},
		"bad created_at": {
			mutate: func(c *Corpus) { c.Fixtures[0].Records[0].CreatedAt = "2026-01-01" },
			want:   "not RFC 3339",
		},
		"bad valid_to": {
			mutate: func(c *Corpus) { c.Fixtures[0].Records[0].ValidTo = "tomorrow" },
			want:   "valid_to",
		},
		"unknown superseded_by": {
			mutate: func(c *Corpus) { c.Fixtures[0].Records[0].SupersededBy = "decision-missing" },
			want:   "superseded_by",
		},
		"invalid decision path": {
			mutate: func(c *Corpus) { c.Fixtures[0].Records[0].Paths = []string{"/etc/passwd"} },
			want:   "must be relative",
		},
		"empty query": {
			mutate: func(c *Corpus) { c.Fixtures[0].Tasks[0].Query = " " },
			want:   "query is required",
		},
		"missing expected": {
			mutate: func(c *Corpus) { c.Fixtures[0].Tasks[0].Expected = nil },
			want:   "expected needs at least one record id",
		},
		"unknown expected": {
			mutate: func(c *Corpus) { c.Fixtures[0].Tasks[0].Expected = []string{"decision-missing"} },
			want:   "not a record of this project",
		},
		"forbidden and expected overlap": {
			mutate: func(c *Corpus) {
				c.Fixtures[0].Tasks[0].Forbidden = []string{"decision-2026-01-01-alpha"}
			},
			want: "both expected and forbidden",
		},
		"unknown forbidden": {
			mutate: func(c *Corpus) { c.Fixtures[0].Tasks[0].Forbidden = []string{"decision-missing"} },
			want:   "forbidden id",
		},
		"duplicate task id": {
			mutate: func(c *Corpus) {
				c.Fixtures[0].Tasks = append(c.Fixtures[0].Tasks, c.Fixtures[0].Tasks[0])
			},
			want: "already used",
		},
		"bad as_of": {
			mutate: func(c *Corpus) { c.Fixtures[0].Tasks[0].AsOf = "last tuesday" },
			want:   "as_of",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			corpus := mustCorpus(t)
			tc.mutate(&corpus)
			err := corpus.Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid corpus")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadDirRejectsMissingAndEmptyDirectories(t *testing.T) {
	if _, err := LoadDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("LoadDir accepted a missing directory")
	}
	if _, err := LoadDir(t.TempDir()); err == nil {
		t.Fatal("LoadDir accepted a directory without fixtures")
	}
}

func TestTaskCategoryNameDefaultsToRetrieval(t *testing.T) {
	if got := (Task{Category: " "}).CategoryName(); got != DefaultCategory {
		t.Fatalf("CategoryName = %q, want %q", got, DefaultCategory)
	}
	if got := (Task{Category: "morphology"}).CategoryName(); got != "morphology" {
		t.Fatalf("CategoryName = %q, want morphology", got)
	}
}

func mustCorpus(t *testing.T) Corpus {
	t.Helper()
	corpus, err := LoadDir(writeValidFixture(t))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	return corpus
}

func writeValidFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFixtureFile(t, dir, "demo.yaml", validFixtureYAML)
	return dir
}

func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
