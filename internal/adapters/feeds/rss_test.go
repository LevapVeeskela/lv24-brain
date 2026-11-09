package feeds

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ingestiontest"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// escXML encodes one literal into XML character data.
func escXML(value string) string {
	var out strings.Builder
	_ = xml.EscapeText(&out, []byte(value))
	return out.String()
}

// mustJSON marshals a fixture cursor into checkpoint bytes.
func mustJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		t := fmt.Sprintf("marshal %v: %v", value, err)
		panic(t)
	}
	return data
}

// The rss fixture renders deterministic RSS 2.0 and Atom feeds from a
// versioned entry list. Content and validators only change when the test
// mutates the fixture, so drives are deterministic and replay-safe.

// fixtureBase is the all-entries base publication time; increments keep
// deterministic ordering.
var fixtureBase = time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)

type rssEntry struct {
	guid   string
	title  string
	body   string
	link   string
	author string
	date   time.Time
}

type fakeFeeds struct {
	mu      sync.Mutex
	version int
	updated int // deterministic forward mover for UpdateItem
	next    int // next entry id
	entries []rssEntry
}

func (f *fakeFeeds) etag() string { return fmt.Sprintf(`"rss-v%d"`, f.version) }
func (f *fakeFeeds) modified() string {
	return httpTime(fixtureBase.Add(time.Duration(f.version) * time.Minute))
}

// addOne appends one deterministic entry with a fresh, larger timestamp.
func (f *fakeFeeds) addOne() {
	f.next++
	index := len(f.entries) + 1
	author := "alice"
	if index%2 == 0 {
		author = "bob"
	}
	f.entries = append(f.entries, rssEntry{
		guid:   fmt.Sprintf("entry-%d", f.next),
		title:  fmt.Sprintf("Entry %d", f.next),
		body:   fmt.Sprintf("<p>Body <b>%d</b> &amp; more\nwith\tspaces</p>", f.next),
		link:   fmt.Sprintf("https://example.test/posts/%d", f.next),
		author: author,
		date:   fixtureBase.Add(time.Duration(index) * time.Hour),
	})
	f.version++
}

// AddMany appends n entries.
func (f *fakeFeeds) AddMany(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for range n {
		f.addOne()
	}
}

// AddItem appends one entry.
func (f *fakeFeeds) AddItem() { f.AddMany(1) }

// UpdateItem rewrites the first entry: new content and a strictly later,
// deterministic publication date so the incremental scan picks it up.
func (f *fakeFeeds) UpdateItem() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		f.addOne()
		return
	}
	f.updated++
	entry := &f.entries[0]
	entry.body = fmt.Sprintf("<p>Updated <b>%d</b> twice</p>", f.updated)
	entry.title = fmt.Sprintf("Entry 1 (update %d)", f.updated)
	entry.date = fixtureBase.Add(time.Duration(100+f.updated) * time.Hour)
	f.version++
}

// DeleteItem removes the last entry. The rss source declares no Deletes
// capability, so the suite never drives this.
func (f *fakeFeeds) DeleteItem() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) > 1 {
		f.entries = f.entries[:len(f.entries)-1]
		f.version++
	}
}

// renderRSS writes the current entries as RSS 2.0 (escaped).
func (f *fakeFeeds) renderRSS() string {
	var out strings.Builder
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>Fixture feed</title>`)
	for _, entry := range f.entries {
		fmt.Fprintf(&out,
			`<item><guid>%s</guid><title>%s</title><link>%s</link><pubDate>%s</pubDate><dc:creator xmlns:dc="http://purl.org/dc/elements/1.1/">%s</dc:creator><content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/">%s</content:encoded></item>`,
			escXML(entry.guid),
			escXML(entry.title),
			escXML(entry.link),
			escXML(entry.date.Format(time.RFC1123Z)),
			escXML(entry.author),
			escXML(entry.body),
		)
	}
	out.WriteString("</channel></rss>")
	return out.String()
}

// renderAtom writes the current entries as Atom.
func (f *fakeFeeds) renderAtom() string {
	var out strings.Builder
	out.WriteString(`<feed xmlns="http://www.w3.org/2005/Atom"><title>Atom fixture</title>`)
	for _, entry := range f.entries {
		fmt.Fprintf(&out,
			`<entry><id>%s</id><title>%s</title><link href="%s"/><published>%s</published><updated>%s</updated><author><name>%s</name></author><content type="html">%s</content></entry>`,
			escXML(entry.guid),
			escXML(entry.title),
			escXML(entry.link),
			escXML(entry.date.Format(time.RFC3339)),
			escXML(entry.date.Format(time.RFC3339)),
			escXML(entry.author),
			escXML(entry.body),
		)
	}
	out.WriteString("</feed>")
	return out.String()
}

// serve mounts the feed fixture routes on the handler.
func (f *fakeFeeds) serve(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/feed.xml" && r.URL.Path != "/atom.xml" {
		return false
	}
	if (r.Header.Get("If-None-Match") != "" && r.Header.Get("If-None-Match") == f.etag()) ||
		(r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Modified-Since") != "" && f.modified() == r.Header.Get("If-Modified-Since")) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	w.Header().Set("ETag", f.etag())
	w.Header().Set("Last-Modified", f.modified())
	w.Header().Set("Content-Type", "application/rss+xml")
	if r.URL.Path == "/feed.xml" {
		_, _ = fmt.Fprint(w, f.renderRSS())
	} else {
		_, _ = fmt.Fprint(w, f.renderAtom())
	}
	return true
}

// fixtureServer owns the test servers used by the rss tests.
type rssServer struct {
	server      *httptest.Server
	foreign     *httptest.Server
	feeds       *fakeFeeds
	foreignHits *int
}

func newRSSServer(t *testing.T) *rssServer {
	t.Helper()
	var hits int
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	}))
	t.Cleanup(foreign.Close)
	feeds := &fakeFeeds{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if feeds.serve(w, r) {
			return
		}
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
		case "/malformed.xml":
			_, _ = fmt.Fprint(w, "this is not <rss at all")
		case "/redirect":
			w.Header().Set("Location", foreign.URL+"/elsewhere.xml")
			w.WriteHeader(http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &rssServer{server: server, foreign: foreign, feeds: feeds, foreignHits: &hits}
}

// rssSource builds a source bound to the fixture servers.
func rssSource(g *rssServer, conn domain.Connection) *RSSSource {
	source, err := NewRSS(conn, g.server.Client())
	if err != nil {
		panic(fmt.Sprintf("NewRSS: %v", err))
	}
	return source
}

func rssConnection(g *rssServer, paths ...string) domain.Connection {
	if len(paths) == 0 {
		paths = []string{"/feed.xml"}
	}
	parts := make([]string, 0, len(paths))
	for _, path := range paths {
		parts = append(parts, g.server.URL+path)
	}
	return domain.Connection{
		ID:       "rss-test",
		Provider: NameRSS,
		Settings: map[string]string{"urls": strings.Join(parts, ",")},
	}
}

// harness wires the fixture into the ingestion contract suite.
func rssHarness(g *rssServer) ingestiontest.Harness {
	conn := rssConnection(g)
	bad := rssConnection(g, "/missing")
	bad.ID = "rss-bad"
	g.feeds.AddMany(2)
	return ingestiontest.Harness{
		Connection:    conn,
		NewSource:     func(c domain.Connection) ports.Source { return rssSource(g, c) },
		AddItem:       func() { g.feeds.AddItem() },
		UpdateItem:    func() { g.feeds.UpdateItem() },
		AddMany:       func(n int) { g.feeds.AddMany(n) },
		BadConnection: &bad,
		PageSize:      5,
	}
}

// TestSourceContract runs the ingestion contract suite for rss.
func TestSourceContract(t *testing.T) {
	g := newRSSServer(t)
	t.Run("rss", func(t *testing.T) {
		ingestiontest.RunSourceSuite(t, rssHarness(g))
	})
}

// TestRSSCheckVerifiesFeeds covers the check path and its failures.
func TestRSSCheckVerifiesFeeds(t *testing.T) {
	g := newRSSServer(t)
	ctx := context.Background()
	if err := rssSource(g, rssConnection(g)).Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if err := rssSource(g, rssConnection(g, "/missing")).Check(ctx); err == nil {
		t.Fatal("Check on a 404 feed succeeded")
	}
	err := rssSource(g, rssConnection(g, "/malformed.xml")).Check(ctx)
	if err == nil || !strings.Contains(err.Error(), "malformed feed") {
		t.Fatalf("Check error = %v; want a malformed feed error", err)
	}
}

// TestRSSReadsEntriesWithStableIDs asserts fields, external IDs and streams.
func TestRSSReadsEntriesWithStableIDs(t *testing.T) {
	g := newRSSServer(t)
	g.feeds.AddMany(3)
	source := rssSource(g, rssConnection(g))
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: rssConnection(g)})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	wantStream := streamOf(rssStreamPrefix, g.server.URL+"/feed.xml")
	for _, item := range batch.Items {
		feedHash := hash16(g.server.URL + "/feed.xml")
		if !strings.HasPrefix(item.Ref.ExternalID, feedHash+"/entry-") {
			t.Fatalf("ExternalID = %q, want <feed-hash>/<guid>", item.Ref.ExternalID)
		}
		if item.Kind != domain.ItemDocument || item.Title == "" {
			t.Fatalf("item = %#v", item)
		}
		if strings.Contains(item.Body, "<") || strings.Contains(item.Body, "\t") {
			t.Fatalf("body was not HTML-stripped: %q", item.Body)
		}
	}
	if len(batch.Checkpoints) == 0 || batch.Checkpoints[0].Stream != wantStream {
		t.Fatalf("checkpoints = %#v, want stream %q", batch.Checkpoints, wantStream)
	}
	if len(batch.Items) == 0 || batch.Items[0].Author == "" {
		t.Fatalf("missing author in %#v", batch.Items)
	}
	// A repeat read yields identical items (deterministic batches).
	repeat, err := source.Read(context.Background(), ports.ReadRequest{Connection: rssConnection(g)})
	if err != nil {
		t.Fatalf("repeat Read: %v", err)
	}
	if len(repeat.Items) != len(batch.Items) {
		t.Fatalf("repeat read diverged: %d vs %d items", len(repeat.Items), len(batch.Items))
	}
	for i := range repeat.Items {
		if repeat.Items[i].ComputeHash() != batch.Items[i].ComputeHash() {
			t.Fatalf("item %d is not deterministic", i)
		}
	}
}

// TestRSSHonorsConditionalRequests asserts If-None-Match/If-Modified-Since
// and 304 handling.
func TestRSSHonorsConditionalRequests(t *testing.T) {
	g := newRSSServer(t)
	g.feeds.AddMany(2)
	source := rssSource(g, rssConnection(g))
	conn := rssConnection(g)
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if len(first.Items) != 2 {
		t.Fatalf("first Read produced %d items", len(first.Items))
	}
	checkpoints := map[string]domain.Checkpoint{}
	for _, checkpoint := range first.Checkpoints {
		checkpoints[checkpoint.Stream] = checkpoint
	}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if len(second.Items) != 0 {
		t.Fatalf("second Read emitted %d items; want a 304 short-circuit", len(second.Items))
	}
	// Simulating infrastructure without a validator cache: reset to empty.
	third, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("third Read: %v", err)
	}
	if len(third.Items) != 2 {
		t.Fatalf("third Read produced %d items; want the fresh 200 pass", len(third.Items))
	}
}

// TestRSSCursorMalformed asserts malformed checkpoints fail the read.
func TestRSSCursorMalformed(t *testing.T) {
	g := newRSSServer(t)
	source := rssSource(g, rssConnection(g))
	stream := streamOf(rssStreamPrefix, g.server.URL+"/feed.xml")
	base := rssConnection(g)
	for _, tc := range []struct {
		name       string
		checkpoint domain.Checkpoint
	}{
		{"garbage", domain.Checkpoint{Stream: stream, Version: checkpointV1, Data: []byte("{not json")}},
		{"wrong version", domain.Checkpoint{Stream: stream, Version: checkpointV1 + 1, Data: mustJSON(rssCursor{Offset: 1})}},
		{"wrong stream", domain.Checkpoint{Stream: "rss-0000000000000000", Version: checkpointV1, Data: mustJSON(rssCursor{Offset: 1})}},
		{"wrong type blob", domain.Checkpoint{Stream: stream, Version: checkpointV1, Data: []byte(`{"etag":[7]}`)}},
	} {
		batch, err := source.Read(context.Background(), ports.ReadRequest{
			Connection:  base,
			Checkpoints: map[string]domain.Checkpoint{tc.checkpoint.Stream: tc.checkpoint},
		})
		if err == nil {
			t.Fatalf("%s: Read succeeded (%#v); want a cursor error", tc.name, batch.Items)
		}
	}
}

// TestRSSForeignRedirectNotFollowed asserts redirects never leave host.
func TestRSSForeignRedirectNotFollowed(t *testing.T) {
	g := newRSSServer(t)
	source := rssSource(g, rssConnection(g, "/redirect"))
	_, err := source.Read(context.Background(), ports.ReadRequest{Connection: rssConnection(g, "/redirect")})
	if err == nil || !strings.Contains(err.Error(), "redirects are not followed") {
		t.Fatalf("Read err = %v; want redirect refusal", err)
	}
	if err := source.Check(context.Background()); err == nil {
		t.Fatal("Check on a redirecting feed succeeded")
	}
	if *g.foreignHits != 0 {
		t.Fatalf("foreign host was fetched %d times", *g.foreignHits)
	}
}

// TestRSSRedirectPolicyOverridesClient asserts that even a client configured
// to follow redirects cannot make the source fetch a foreign host.
func TestRSSRedirectPolicyOverridesClient(t *testing.T) {
	g := newRSSServer(t)
	conn := rssConnection(g, "/redirect")
	source, err := NewRSS(conn, &http.Client{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Check(context.Background()); err == nil {
		t.Fatal("Check succeeded on a redirecting feed")
	}
	if *g.foreignHits != 0 {
		t.Fatalf("foreign server hits = %d, want 0", *g.foreignHits)
	}
}

// TestRSSBodyBounds caps huge entries at the domain body bound.
func TestRSSBodyBounds(t *testing.T) {
	g := newRSSServer(t)
	g.feeds.mu.Lock()
	g.feeds.entries = []rssEntry{{
		guid:  "huge-1",
		title: "Huge",
		body:  strings.Repeat("<p>"+strings.Repeat("x", 600)+"</p>", maxBodyBytes/600+1),
		link:  "https://example.test/huge",
		date:  fixtureBase,
	}}
	g.feeds.version = 1
	g.feeds.mu.Unlock()
	source := rssSource(g, rssConnection(g))
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: rssConnection(g)})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 1 || len(batch.Items[0].Body) != maxBodyBytes {
		t.Fatalf("body bytes = %d, want %d", len(batch.Items[0].Body), maxBodyBytes)
	}
}

// TestRSSEntryLimit governs the per-feed entry budget.
func TestRSSEntryLimit(t *testing.T) {
	g := newRSSServer(t)
	g.feeds.AddMany(5)
	conn := rssConnection(g)
	conn.Settings["limit"] = "3"
	source := rssSource(g, conn)
	seen := 0
	checkpoints := map[string]domain.Checkpoint{}
	for {
		batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for _, checkpoint := range batch.Checkpoints {
			checkpoints[checkpoint.Stream] = checkpoint
		}
		seen += len(batch.Items)
		if !batch.More {
			break
		}
		if seen > 5 {
			t.Fatal("entry limit leaked too many items")
		}
	}
	if seen != 5 {
		t.Fatalf("entries consumed %d, want 5", seen)
	}
}
