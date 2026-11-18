//go:build ignore

// scrub-raw-pii-hash-check verifies item files against the engine's own hash:
// it reads every connectors/<id>/items/*.md the way fsstore.readItemFile does
// and compares domain.Item.ComputeHash with the stored frontmatter hash.
//
// Usage:
//
//	go run scripts/scrub-raw-pii-hash-check.go <data-dir>
//	go run scripts/scrub-raw-pii-hash-check.go -print-hash <item-file>
//
// The first form exits non-zero when any item is stale or invalid; the second
// prints only the hash a fresh ingest would write for that file's content.
// It exists so scripts/scrub-raw-pii.py can prove its Python port of
// ComputeHash stays byte-identical to the engine.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

type itemFrontmatter struct {
	SchemaVersion int `yaml:"schema_version"`
	Ref           struct {
		ConnectionID string `yaml:"connection_id"`
		ExternalID   string `yaml:"external_id"`
	} `yaml:"ref"`
	Kind      string   `yaml:"kind"`
	Title     string   `yaml:"title"`
	URL       string   `yaml:"url"`
	Author    string   `yaml:"author"`
	State     string   `yaml:"state"`
	Labels    []string `yaml:"labels"`
	ACL       []string `yaml:"acl"`
	Project   string   `yaml:"project"`
	CreatedAt string   `yaml:"created_at"`
	UpdatedAt string   `yaml:"updated_at"`
	Hash      string   `yaml:"hash"`
	Deleted   bool     `yaml:"deleted"`
}

// readItem mirrors fsstore.readItemFile: parse the frontmatter, drop one
// trailing body newline, parse provider times as RFC 3339.
func readItem(path string) (domain.Item, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.Item{}, err
	}
	var front itemFrontmatter
	body, err := markdown.Parse(raw, &front)
	if err != nil {
		return domain.Item{}, err
	}
	item := domain.Item{
		Ref:       domain.ItemRef{ConnectionID: front.Ref.ConnectionID, ExternalID: front.Ref.ExternalID},
		Kind:      domain.ItemKind(front.Kind),
		Title:     front.Title,
		Body:      strings.TrimSuffix(body, "\n"),
		URL:       front.URL,
		Author:    front.Author,
		State:     domain.ItemState(front.State),
		Labels:    front.Labels,
		ACL:       front.ACL,
		Hash:      front.Hash,
		Deleted:   front.Deleted,
		ProjectID: front.Project,
	}
	if item.Deleted {
		item.Body = ""
	}
	for _, field := range []struct {
		raw    string
		target *time.Time
	}{
		{front.CreatedAt, &item.CreatedAt},
		{front.UpdatedAt, &item.UpdatedAt},
	} {
		if field.raw == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, field.raw)
		if err != nil {
			return domain.Item{}, fmt.Errorf("%q is not RFC 3339", field.raw)
		}
		*field.target = parsed.UTC()
	}
	return item, nil
}

func main() {
	printHash := flag.Bool("print-hash", false, "print the hash for one item file")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: scrub-raw-pii-hash-check [-print-hash] <path>")
		os.Exit(2)
	}
	if *printHash {
		item, err := readItem(flag.Arg(0))
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
		fmt.Println(item.ComputeHash())
		return
	}

	paths, err := filepath.Glob(filepath.Join(flag.Arg(0), "connectors", "*", "items", "*.md"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "glob:", err)
		os.Exit(1)
	}
	stale, invalid := 0, 0
	for _, path := range paths {
		item, err := readItem(path)
		if err != nil {
			fmt.Printf("invalid\t%s\t%v\n", path, err)
			invalid++
			continue
		}
		if computed := item.ComputeHash(); computed != item.Hash {
			fmt.Printf("stale\t%s\tstored=%s\tcomputed=%s\n", path, item.Hash, computed)
			stale++
			continue
		}
		if err := item.Validate(); err != nil {
			fmt.Printf("invalid\t%s\t%v\n", path, err)
			invalid++
		}
	}
	fmt.Printf("items=%d stale=%d invalid=%d\n", len(paths), stale, invalid)
	if stale != 0 || invalid != 0 {
		os.Exit(1)
	}
}
