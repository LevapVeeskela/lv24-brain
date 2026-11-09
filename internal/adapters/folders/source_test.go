package folders

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ingestiontest"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var fixedNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func testConnection(root string, extra map[string]string) domain.Connection {
	settings := map[string]string{"paths": root}
	for key, value := range extra {
		settings[key] = value
	}
	return domain.Connection{ID: "folders-test", Provider: Name, Settings: settings}
}

func mustSource(t *testing.T, conn domain.Connection) *Source {
	t.Helper()
	source, err := NewSource(conn, func() time.Time { return fixedNow })
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	return source
}

func writeFile(t *testing.T, full, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func chtimes(t *testing.T, full string, mod time.Time) {
	t.Helper()
	if err := os.Chtimes(full, mod, mod); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
}

func readAll(t *testing.T, source *Source, conn domain.Connection, cps map[string]domain.Checkpoint, limit int, since *time.Time) (ports.ReadBatch, map[string]domain.Checkpoint) {
	t.Helper()
	cps = cloneCps(cps)
	var items []domain.Item
	var batch ports.ReadBatch
	var err error
	for {
		batch, err = source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: cps, Limit: limit, Since: since})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		items = append(items, batch.Items...)
		for _, cp := range batch.Checkpoints {
			cps[cp.Stream] = cp
		}
		if !batch.More {
			break
		}
	}
	batch.Items = items
	return batch, cps
}

func cloneCps(in map[string]domain.Checkpoint) map[string]domain.Checkpoint {
	out := make(map[string]domain.Checkpoint, len(in))
	for stream, cp := range in {
		out[stream] = cp.Clone()
	}
	return out
}

func itemsByID(batch ports.ReadBatch) map[string]domain.Item {
	out := make(map[string]domain.Item, len(batch.Items))
	for _, item := range batch.Items {
		out[item.Ref.ExternalID] = item
	}
	return out
}

func TestDescriptor(t *testing.T) {
	desc := Descriptor()
	if err := desc.Validate(); err != nil {
		t.Fatalf("Descriptor.Validate: %v", err)
	}
	if desc.Provider != "folders" || desc.DisplayName != "Local folders" {
		t.Fatalf("descriptor identity = %q/%q", desc.Provider, desc.DisplayName)
	}
	if len(desc.Backends) != 1 || desc.Backends[0] != domain.BackendNative {
		t.Fatalf("backends = %v, want [native]", desc.Backends)
	}
	if !desc.Capabilities.Backfill || !desc.Capabilities.Incremental || desc.Capabilities.Deletes {
		t.Fatalf("capabilities = %+v, want backfill+incremental, no deletes", desc.Capabilities)
	}
	if desc.DefaultPublish != domain.PublishLocalOnly || desc.PersonalData {
		t.Fatalf("publish = %q personalData = %v", desc.DefaultPublish, desc.PersonalData)
	}
	if len(desc.Streams) != 0 {
		t.Fatalf("streams = %v, want dynamic per-root streams", desc.Streams)
	}
	keys := map[string]bool{}
	for _, field := range desc.ConfigFields {
		keys[field.Key] = true
	}
	if !keys["paths"] || !keys["extensions"] || !keys["ignore"] {
		t.Fatalf("config fields = %v, want paths/extensions/ignore", desc.ConfigFields)
	}
}

func TestNewSourceValidation(t *testing.T) {
	tmp := t.TempDir()
	plain := filepath.Join(tmp, "file.txt")
	writeFile(t, plain, "x")
	cases := []struct {
		name     string
		settings map[string]string
	}{
		{"missing paths", map[string]string{}},
		{"empty paths", map[string]string{"paths": "  \n, "}},
		{"nul", map[string]string{"paths": "a\x00b"}},
		{"nonexistent", map[string]string{"paths": filepath.Join(tmp, "nope")}},
		{"not a directory", map[string]string{"paths": plain}},
	}
	for _, c := range cases {
		if _, err := NewSource(domain.Connection{ID: "folders-test", Provider: Name, Settings: c.settings}, nil); err == nil {
			t.Errorf("%s: NewSource succeeded, want an error", c.name)
		}
	}
}

func TestCheck(t *testing.T) {
	tmp := t.TempDir()
	conn := testConnection(tmp, nil)
	source := mustSource(t, conn)
	if err := source.Check(context.Background()); err != nil {
		t.Fatalf("Check on a readable dir: %v", err)
	}
	// A source bound before the root vanished must fail Check (and must
	// not read file content: only directory entries are touched).
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := source.Check(context.Background()); err == nil {
		t.Fatal("Check on a removed root succeeded, want an error")
	}
}

func TestReadNestedIgnoredAndTitles(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "n.md"), "# Title\n\nSee [[other]] and property:: value\n")
	writeFile(t, filepath.Join(tmp, "sub", "c.txt"), "Привет, мир\n")
	writeFile(t, filepath.Join(tmp, "doc.rst"), "rst body\n")
	writeFile(t, filepath.Join(tmp, "page.html"), "<p>hi</p>\n")
	writeFile(t, filepath.Join(tmp, ".git", "x.md"), "# hidden\n")
	writeFile(t, filepath.Join(tmp, ".obsidian", "y.md"), "# hidden\n")
	writeFile(t, filepath.Join(tmp, ".logseq", "z.md"), "# hidden\n")
	writeFile(t, filepath.Join(tmp, "node_modules", "w.md"), "# hidden\n")
	writeFile(t, filepath.Join(tmp, "drafts", "d.md"), "# draft\n")
	writeFile(t, filepath.Join(tmp, "img.png"), "not text\n")
	writeFile(t, filepath.Join(tmp, "bin.md"), "text\x00binary\n")
	if err := os.WriteFile(filepath.Join(tmp, "bad.md"), []byte{0xff, 0xfe, 0x41}, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tmp, "plain.txt"), "no heading here\n")
	writeFile(t, filepath.Join(tmp, "front.md"), "---\ntitle: kept\n---\n\n# After Frontmatter\n\nbody\n")

	conn := testConnection(tmp, map[string]string{"ignore": "drafts"})
	source := mustSource(t, conn)
	batch, _ := readAll(t, source, conn, nil, 0, nil)
	if batch.More {
		t.Fatal("More = true after a full read")
	}
	got := itemsByID(batch)
	want := []string{"n.md", "sub/c.txt", "doc.rst", "page.html", "plain.txt", "front.md"}
	if len(got) != len(want) {
		t.Fatalf("external IDs = %v, want %v", keysOf(got), want)
	}
	for _, id := range want {
		item := got[id]
		if item.Kind != domain.ItemDocument {
			t.Errorf("%s: kind = %q, want document", id, item.Kind)
		}
		if item.Ref.ConnectionID != conn.ID {
			t.Errorf("%s: connection = %q", id, item.Ref.ConnectionID)
		}
		if item.Author != "" || item.URL != "" || len(item.Labels) != 0 {
			t.Errorf("%s: author/url/labels must be empty: %+v", id, item)
		}
		if item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() || item.CreatedAt.Location() != time.UTC {
			t.Errorf("%s: times must be non-zero UTC: %v %v", id, item.CreatedAt, item.UpdatedAt)
		}
		if err := item.WithHash().Validate(); err != nil {
			t.Errorf("%s: invalid item: %v", id, err)
		}
	}
	if got["n.md"].Title != "Title" {
		t.Errorf("n.md title = %q, want H1", got["n.md"].Title)
	}
	body := got["n.md"].Body
	if !strings.Contains(body, "[[other]]") || !strings.Contains(body, "property:: value") {
		t.Errorf("n.md body lost Obsidian/Logseq syntax: %q", body)
	}
	if !strings.Contains(got["sub/c.txt"].Body, "Привет, мир") {
		t.Errorf("cyrillic body mangled: %q", got["sub/c.txt"].Body)
	}
	if got["plain.txt"].Title != "plain.txt" {
		t.Errorf("fallback title = %q, want filename", got["plain.txt"].Title)
	}
	front := got["front.md"]
	if front.Title != "After Frontmatter" {
		t.Errorf("frontmatter title = %q", front.Title)
	}
	if !strings.Contains(front.Body, "title: kept") {
		t.Errorf("frontmatter must be kept verbatim: %q", front.Body)
	}
}

func keysOf(items map[string]domain.Item) []string {
	out := make([]string, 0, len(items))
	for id := range items {
		out = append(out, id)
	}
	return out
}

func TestReadRespectsExtensions(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "a.md"), "# A\n")
	writeFile(t, filepath.Join(tmp, "b.txt"), "b\n")
	writeFile(t, filepath.Join(tmp, "c.rst"), "c\n")
	conn := testConnection(tmp, map[string]string{"extensions": "md"})
	batch, _ := readAll(t, mustSource(t, conn), conn, nil, 0, nil)
	got := itemsByID(batch)
	if len(got) != 1 || got["a.md"].Title != "A" {
		t.Fatalf("extension filter gave %v, want only a.md", keysOf(got))
	}
}

func TestSymlinkSkipped(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "real.md"), "# Real\n")
	link := filepath.Join(tmp, "link.md")
	if err := os.Symlink(filepath.Join(tmp, "real.md"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	conn := testConnection(tmp, nil)
	batch, _ := readAll(t, mustSource(t, conn), conn, nil, 0, nil)
	got := itemsByID(batch)
	if len(got) != 1 || got["real.md"].Title != "Real" {
		t.Fatalf("symlink walk gave %v, want only real.md", keysOf(got))
	}
	// A symlinked directory must be pruned, not followed.
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "escaped.md"), "# Escaped\n")
	dirLink := filepath.Join(tmp, "vault-link")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Skipf("dir symlinks unavailable: %v", err)
	}
	batch, _ = readAll(t, mustSource(t, conn), conn, nil, 0, nil)
	if _, ok := itemsByID(batch)["vault-link/escaped.md"]; ok {
		t.Fatal("followed a symlinked directory")
	}
}

func TestSinceAndLimitPagination(t *testing.T) {
	tmp := t.TempDir()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	writeFile(t, filepath.Join(tmp, "a.md"), "# A\n")
	chtimes(t, filepath.Join(tmp, "a.md"), base)
	writeFile(t, filepath.Join(tmp, "b.md"), "# B\n")
	chtimes(t, filepath.Join(tmp, "b.md"), base.Add(time.Hour))
	writeFile(t, filepath.Join(tmp, "c.md"), "# C\n")
	chtimes(t, filepath.Join(tmp, "c.md"), base.Add(2*time.Hour))

	conn := testConnection(tmp, nil)
	source := mustSource(t, conn)

	since := base.Add(time.Hour)
	batch, _ := readAll(t, source, conn, nil, 0, &since)
	if got := keysOf(itemsByID(batch)); len(got) != 2 {
		t.Fatalf("Since narrowed to %v, want b.md and c.md", got)
	}

	// Limit 1 pages deterministically to the same set as an unpaged read.
	var pages []ports.ReadBatch
	cps := map[string]domain.Checkpoint{}
	for {
		batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: cps, Limit: 1})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		pages = append(pages, batch)
		for _, cp := range batch.Checkpoints {
			cps[cp.Stream] = cp
		}
		if !batch.More {
			break
		}
		if len(pages) > 10 {
			t.Fatal("More never cleared")
		}
	}
	if len(pages) != 3 {
		t.Fatalf("got %d pages for 3 files at limit 1", len(pages))
	}
	for i, page := range pages {
		if len(page.Items) != 1 {
			t.Fatalf("page %d has %d items", i, len(page.Items))
		}
		if i < 2 && !page.More {
			t.Fatalf("page %d: More = false with files pending", i)
		}
	}
	if pages[2].More {
		t.Fatal("last page: More = true with nothing pending")
	}
	var paged []string
	for _, page := range pages {
		paged = append(paged, page.Items[0].Ref.ExternalID)
	}
	if paged[0] != "a.md" || paged[1] != "b.md" || paged[2] != "c.md" {
		t.Fatalf("page order = %v, want sorted", paged)
	}

	// Replaying from the same checkpoints is deterministic.
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || len(second.Items) != 1 ||
		first.Items[0].ComputeHash() != second.Items[0].ComputeHash() ||
		!first.Checkpoints[0].Equal(second.Checkpoints[0]) || first.More != second.More {
		t.Fatal("repeated read from the same checkpoints diverged")
	}
}

func TestNoDeletesInferred(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "keep.md"), "# Keep\n")
	writeFile(t, filepath.Join(tmp, "gone.md"), "# Gone\n")
	conn := testConnection(tmp, nil)
	source := mustSource(t, conn)
	_, cps := readAll(t, source, conn, nil, 0, nil)
	if err := os.Remove(filepath.Join(tmp, "gone.md")); err != nil {
		t.Fatal(err)
	}
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: cps})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range batch.Items {
		if item.Deleted {
			t.Fatalf("emitted a tombstone for %q", item.Ref.ExternalID)
		}
	}
}

func TestBodyTruncatedToCap(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "big.md"), "# Big\n\n"+strings.Repeat("я", domain.MaxItemBodyBytes))
	conn := testConnection(tmp, nil)
	batch, _ := readAll(t, mustSource(t, conn), conn, nil, 0, nil)
	body := itemsByID(batch)["big.md"].Body
	if len(body) > domain.MaxItemBodyBytes {
		t.Fatalf("body is %d bytes, over the %d cap", len(body), domain.MaxItemBodyBytes)
	}
	if err := itemsByID(batch)["big.md"].WithHash().Validate(); err != nil {
		t.Fatalf("truncated item invalid: %v", err)
	}
}

func TestMultiRootExternalIDsDiffer(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeFile(t, filepath.Join(first, "note.md"), "# First\n")
	writeFile(t, filepath.Join(second, "note.md"), "# Second\n")
	conn := testConnection(first+","+second, nil)
	batch, _ := readAll(t, mustSource(t, conn), conn, nil, 0, nil)
	if len(batch.Items) != 2 {
		t.Fatalf("got %d items, want 2 distinct IDs", len(batch.Items))
	}
	if batch.Items[0].Ref.ExternalID == batch.Items[1].Ref.ExternalID {
		t.Fatal("multi-root collision on identical relative paths")
	}
	if len(batch.Checkpoints) != 2 {
		t.Fatalf("got %d checkpoints, want one stream per root", len(batch.Checkpoints))
	}
}

func TestConnectionMismatch(t *testing.T) {
	tmp := t.TempDir()
	conn := testConnection(tmp, nil)
	source := mustSource(t, conn)
	other := conn
	other.ID = "folders-other"
	if _, err := source.Read(context.Background(), ports.ReadRequest{Connection: other}); err == nil {
		t.Fatal("Read with a foreign connection ID succeeded")
	}
}

func TestSourceContract(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "a.md"), "# Hello\n\nSee [[other]] and property:: value\n")
	writeFile(t, filepath.Join(tmp, "sub", "b.txt"), "plain body\n")

	conn := testConnection(tmp, nil)

	var mu sync.Mutex
	counter := 0
	addOne := func() {
		mu.Lock()
		defer mu.Unlock()
		counter++
		name := fmt.Sprintf("gen-%04d.md", counter)
		writeFile(t, filepath.Join(tmp, name), fmt.Sprintf("# Generated %d\n\nbody %d\n", counter, counter))
	}
	harness := ingestiontest.Harness{
		Connection: conn,
		PageSize:   3,
		NewSource: func(c domain.Connection) ports.Source {
			source, err := NewSource(c, func() time.Time { return fixedNow })
			if err != nil {
				t.Fatalf("NewSource: %v", err)
			}
			return source
		},
		AddItem: addOne,
		UpdateItem: func() {
			mu.Lock()
			defer mu.Unlock()
			full := filepath.Join(tmp, "a.md")
			raw, err := os.ReadFile(full)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if err := os.WriteFile(full, append(raw, []byte("\nUpdated.\n")...), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			chtimes(t, full, time.Now().UTC().Add(time.Hour))
		},
		AddMany: func(n int) {
			for range n {
				addOne()
			}
		},
	}
	ingestiontest.RunSourceSuite(t, harness)
}
