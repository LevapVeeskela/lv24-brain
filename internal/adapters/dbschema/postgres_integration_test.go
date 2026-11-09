package dbschema

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestPostgresIntegration runs only when BRAIN_TEST_DATABASE_URL is set, for
// example:
//
//	$env:BRAIN_TEST_DATABASE_URL='postgres://postgres:brain-test@127.0.0.1:52030/brain_test?sslmode=disable'
//	go test ./internal/adapters/dbschema -run TestPostgresIntegration -count=1 -v
//
// No live credentials are used by default. The test only reads the existing
// catalog; it never creates or drops objects.
func TestPostgresIntegration(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("BRAIN_TEST_DATABASE_URL"))
	if rawURL == "" {
		t.Skip("BRAIN_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse BRAIN_TEST_DATABASE_URL: %v", err)
	}
	database := strings.Trim(parsed.Path, "/")
	if parsed.Hostname() == "" || database == "" {
		t.Fatalf("BRAIN_TEST_DATABASE_URL must include a host and database")
	}
	user := ""
	password := ""
	if parsed.User != nil {
		user = parsed.User.Username()
		password, _ = parsed.User.Password()
	}
	if user == "" {
		t.Fatalf("BRAIN_TEST_DATABASE_URL must include a user")
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatalf("BRAIN_TEST_DATABASE_URL has an invalid port")
	}
	sslMode := parsed.Query().Get("sslmode")
	if sslMode == "" {
		sslMode = "verify-full"
	}
	conn := domain.Connection{
		ID:       "pg-schema-integration",
		Provider: PostgresProvider,
		Settings: map[string]string{
			"host":     parsed.Hostname(),
			"port":     port,
			"database": database,
			"user":     user,
			"sslmode":  sslMode,
		},
	}
	source, err := NewPostgresSource(conn, staticCredentials{"password": password}, nil)
	if err != nil {
		t.Fatalf("NewPostgresSource: %v", err)
	}
	if err := source.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	batch, checkpoints := readAllPostgres(t, source, conn)
	if len(batch.Items) == 0 {
		t.Fatal("snapshot returned no items")
	}
	for _, item := range batch.Items {
		if err := item.WithHash().Validate(); err != nil {
			t.Fatalf("item %q invalid: %v", item.Ref.ExternalID, err)
		}
		if !strings.HasPrefix(item.Ref.ExternalID, PostgresProvider+"/"+database+"/") {
			t.Fatalf("item external ID %q does not use the database prefix", item.Ref.ExternalID)
		}
		if password != "" && strings.Contains(item.Body, password) {
			t.Fatalf("password leaked into item %q", item.Ref.ExternalID)
		}
	}
	unchanged, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints})
	if err != nil {
		t.Fatalf("unchanged Read: %v", err)
	}
	if len(unchanged.Items) != 0 || unchanged.More {
		t.Fatalf("unchanged schema returned %d items, More=%v", len(unchanged.Items), unchanged.More)
	}
	for stream, checkpoint := range checkpoints {
		if password != "" && strings.Contains(string(checkpoint.Data), password) {
			t.Fatalf("password leaked into checkpoint %q", stream)
		}
	}
}

func readAllPostgres(t *testing.T, source *PostgresSource, conn domain.Connection) (ports.ReadBatch, map[string]domain.Checkpoint) {
	t.Helper()
	checkpoints := map[string]domain.Checkpoint{}
	var collected []domain.Item
	var last ports.ReadBatch
	for batch := 0; ; batch++ {
		if batch > 1000 {
			t.Fatal("More never cleared")
		}
		next, err := source.Read(context.Background(), ports.ReadRequest{
			Connection: conn, Checkpoints: cloneCheckpoints(checkpoints), Limit: 100,
		})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		last = next
		collected = append(collected, next.Items...)
		for _, checkpoint := range next.Checkpoints {
			checkpoints[checkpoint.Stream] = checkpoint
		}
		if !next.More {
			break
		}
	}
	last.Items = collected
	last.More = false
	return last, checkpoints
}
