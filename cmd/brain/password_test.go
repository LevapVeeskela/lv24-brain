package main

import (
	"strings"
	"testing"
)

func TestReadPasswordPairReadsTwoLines(t *testing.T) {
	current, next, err := readPasswordPair(strings.NewReader("old-secret-1\r\nnew-secret-22\n"))
	if err != nil {
		t.Fatalf("readPasswordPair: %v", err)
	}
	if current != "old-secret-1" || next != "new-secret-22" {
		t.Fatalf("got (%q, %q)", current, next)
	}
}

func TestReadPasswordPairRejectsMissingValues(t *testing.T) {
	for name, input := range map[string]string{
		"no stdin lines": "",
		"only current":   "old-secret-1\n",
		"empty new":      "old-secret-1\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := readPasswordPair(strings.NewReader(input)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	if _, _, err := readPasswordPair(nil); err == nil {
		t.Fatal("nil stdin must be an error")
	}
}
