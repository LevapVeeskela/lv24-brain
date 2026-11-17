package postgres

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationFilesHaveNoBOM: PostgreSQL rejects a UTF-8 BOM as a syntax
// error before the first SQL token. A BOM can slip in when a file is written
// by a Windows editor or a script; fail the unit suite instead of waiting for
// the Postgres integration job.
func TestMigrationFilesHaveNoBOM(t *testing.T) {
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		path := filepath.Join("migrations", entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if len(raw) >= 3 && raw[0] == 0xef && raw[1] == 0xbb && raw[2] == 0xbf {
			t.Errorf("%s starts with a UTF-8 BOM", path)
		}
		if strings.TrimSpace(string(raw)) == "" {
			t.Errorf("%s is empty", path)
		}
	}
}
