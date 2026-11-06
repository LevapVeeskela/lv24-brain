package fsstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cp1251Mojibake is the prefix UTF-8 punctuation such as an em-dash gains when
// it is re-encoded through CP1251 (U+2014 becomes U+0432 U+0402 U+201D). It is
// spelled with escapes so this file does not trip its own check.
const cp1251Mojibake = "\u0432\u0402"

// TestSourcesHaveNoMojibake guards against CP1251 mojibake in Go sources, which
// once leaked into every generated Markdown heading.
func TestSourcesHaveNoMojibake(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), cp1251Mojibake) {
			t.Errorf("%s contains CP1251 mojibake", name)
		}
	}
}
