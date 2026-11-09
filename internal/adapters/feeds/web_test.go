package feeds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ingestiontest"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// webFixtureBase is the deterministic lastmod base of the web fixture.
var webFixtureBase = time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)

// fakeSite renders deterministic sitemaps and pages from a versioned entry
// list. ETags and lastmods only move when a test mutates the fixture.
type fakeSite struct {
	mu      sync.Mutex
	version int
	updated int
	next    int
	robots  string
	entries []fakePage
}

// fakePage is one advertised page of the fixture.
type fakePage struct {
	id      int
	title   string
	body    string
	lastmod time.Time
}

func (f *fakeSite) etag() string { return fmt.Sprintf(`"web-v%d"`, f.version) }
func (f *fakeSite) modified() string {
	return httpTime(webFixtureBase.Add(time.Duration(f.version) * time.Minute))
}

func (f *fakeSite) addOne() {
	f.next++
	index := len(f.entries) + 1
	f.entries = append(f.entries, fakePage{
		id:      f.next,
		title:   fmt.Sprintf("Page %d &amp; more", f.next),
		body:    fmt.Sprintf("<p>Body <b>%d</b> &amp; text</p>", f.next),
		lastmod: webFixtureBase.Add(time.Duration(index) * time.Hour),
	})
	f.version++
}

// AddMany appends n entries.
func (f *fakeSite) AddMany(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for range n {
		f.addOne()
	}
}

// AddItem appends one entry.
func (f *fakeSite) AddItem() { f.AddMany(1) }

// AddSized appends one entry with a body of at least bytes bytes.
func (f *fakeSite) AddSized(bytes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addOne()
	f.entries[len(f.entries)-1].body = strings.Repeat("x", bytes)
}

// UpdateItem rewrites the first entry and moves its lastmod forward so an
// incremental pass must pick it up.
func (f *fakeSite) UpdateItem() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		f.addOne()
		return
	}
	f.updated++
	entry := &f.entries[0]
	entry.title = fmt.Sprintf("Page 1 (update %d)", f.updated)
	entry.body = fmt.Sprintf("<p>Updated <b>%d</b> content</p>", f.updated)
	entry.lastmod = webFixtureBase.Add(time.Duration(100+f.updated) * time.Hour)
	f.version++
}

// DeleteItem removes the last entry; the source declares no Deletes, so the
// contract suite never drives this.
func (f *fakeSite) DeleteItem() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) > 1 {
		f.entries = f.entries[:len(f.entries)-1]
		f.version++
	}
}

// conditional reports whether the request matches the fixture validators and
// writes 304 when it does.
func (f *fakeSite) conditional(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("If-None-Match") != "" && r.Header.Get("If-None-Match") == f.etag() {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	if r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Modified-Since") != "" &&
		f.modified() == r.Header.Get("If-Modified-Since") {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// serveSitemap renders one urlset; parity -1 selects every page, 0/1 select
// even/odd ids.
func (f *fakeSite) serveSitemap(w http.ResponseWriter, r *http.Request, parity int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conditional(w, r) {
		return
	}
	w.Header().Set("ETag", f.etag())
	w.Header().Set("Last-Modified", f.modified())
	w.Header().Set("Content-Type", "application/xml")
	base := "http://" + r.Host
	var out strings.Builder
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, entry := range f.entries {
		if parity >= 0 && entry.id%2 != parity {
			continue
		}
		fmt.Fprintf(&out, `<url><loc>%s/page/%d</loc><lastmod>%s</lastmod></url>`,
			base, entry.id, entry.lastmod.UTC().Format(time.RFC3339))
	}
	out.WriteString(`</urlset>`)
	_, _ = io.WriteString(w, out.String())
}

// serveIndex renders a two-child sitemap index.
func (f *fakeSite) serveIndex(w http.ResponseWriter, r *http.Request, children ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conditional(w, r) {
		return
	}
	w.Header().Set("ETag", f.etag())
	w.Header().Set("Content-Type", "application/xml")
	base := "http://" + r.Host
	var out strings.Builder
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?><sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, child := range children {
		if !strings.Contains(child, "://") {
			child = base + child
		}
		fmt.Fprintf(&out, `<sitemap><loc>%s</loc></sitemap>`, child)
	}
	out.WriteString(`</sitemapindex>`)
	_, _ = io.WriteString(w, out.String())
}

// servePage renders one HTML page.
func (f *fakeSite) servePage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/page/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	var page fakePage
	found := false
	for _, entry := range f.entries {
		if entry.id == id {
			page, found = entry, true
			break
		}
	}
	f.mu.Unlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<html><head><title>%s</title><script>bad()</script></head><body>%s</body></html>`,
		page.title, page.body)
}

// webServer owns the fixture servers used by the web tests.
type webServer struct {
	server      *httptest.Server
	foreign     *httptest.Server
	site        *fakeSite
	foreignHits *int
}

func newWebServer(t *testing.T) *webServer {
	t.Helper()
	var hits int
	site := &fakeSite{}
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/page/") || strings.HasPrefix(r.URL.Path, "/foreign") {
			hits++
			_, _ = io.WriteString(w, `<html><head><title>Foreign</title></head><body>foreign</body></html>`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(foreign.Close)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sitemap.xml":
			site.serveSitemap(w, r, -1)
		case r.URL.Path == "/sitemap-a.xml":
			site.serveSitemap(w, r, 0)
		case r.URL.Path == "/sitemap-b.xml":
			site.serveSitemap(w, r, 1)
		case r.URL.Path == "/sitemap-index.xml":
			site.serveIndex(w, r, "/sitemap-a.xml", "/sitemap-b.xml")
		case r.URL.Path == "/sitemap-index-foreign.xml":
			site.serveIndex(w, r, foreign.URL+"/foreign-sitemap.xml")
		case r.URL.Path == "/sitemap-foreign.xml":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<?xml version="1.0"?><urlset><url><loc>%s/foreign-page</loc></url></urlset>`, foreign.URL)
		case r.URL.Path == "/redirect-sitemap.xml":
			w.Header().Set("Location", foreign.URL+"/foreign-sitemap.xml")
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/missing-sitemap.xml":
			http.NotFound(w, r)
		case r.URL.Path == "/malformed.xml":
			_, _ = io.WriteString(w, "this is not a sitemap")
		case r.URL.Path == "/redirect-page":
			w.Header().Set("Location", foreign.URL+"/foreign-page")
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/sitemap-redirect-page.xml":
			site.mu.Lock()
			fmt.Fprintf(w, `<?xml version="1.0"?><urlset><url><loc>http://%s/redirect-page</loc></url></urlset>`, r.Host)
			site.mu.Unlock()
		case r.URL.Path == "/robots.txt":
			site.mu.Lock()
			robots := site.robots
			site.mu.Unlock()
			if robots == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, robots)
		case strings.HasPrefix(r.URL.Path, "/page/"):
			site.servePage(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &webServer{server: server, foreign: foreign, site: site, foreignHits: &hits}
}

// webSource builds a web source bound to the fixture servers.
func webSource(g *webServer, conn domain.Connection) *WebSource {
	source, err := NewWeb(conn, g.server.Client())
	if err != nil {
		panic(fmt.Sprintf("NewWeb: %v", err))
	}
	return source
}

// webConnection builds a connection for one fixture sitemap.
func webConnection(g *webServer, path string) domain.Connection {
	return domain.Connection{
		ID:       "web-test",
		Provider: NameWeb,
		Settings: map[string]string{"sitemaps": g.server.URL + path},
	}
}

// webHarness wires the fixture into the ingestion contract suite.
func webHarness(g *webServer) ingestiontest.Harness {
	conn := webConnection(g, "/sitemap.xml")
	bad := webConnection(g, "/missing-sitemap.xml")
	bad.ID = "web-bad"
	g.site.AddMany(2)
	return ingestiontest.Harness{
		Connection:    conn,
		NewSource:     func(c domain.Connection) ports.Source { return webSource(g, c) },
		AddItem:       func() { g.site.AddItem() },
		UpdateItem:    func() { g.site.UpdateItem() },
		AddMany:       func(n int) { g.site.AddMany(n) },
		BadConnection: &bad,
		PageSize:      3,
	}
}

// TestWebSourceContract runs the ingestion contract suite against the web
// source.
func TestWebSourceContract(t *testing.T) {
	g := newWebServer(t)
	ingestiontest.RunSourceSuite(t, webHarness(g))
}

// TestWebDescriptor pins the provider contract and settings.
func TestWebDescriptor(t *testing.T) {
	desc := WebDescriptor()
	if desc.Provider != NameWeb || desc.DisplayName == "" {
		t.Fatalf("provider = %q, display = %q", desc.Provider, desc.DisplayName)
	}
	if desc.Capabilities.Deletes || !desc.Capabilities.Backfill || !desc.Capabilities.Incremental {
		t.Fatalf("capabilities = %+v", desc.Capabilities)
	}
	want := []string{"sitemaps", "maxpages", "allowhosts", "respectrobots"}
	for _, key := range want {
		found := false
		for _, field := range desc.ConfigFields {
			if field.Key == key {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("config field %q is missing", key)
		}
	}
}

// TestWebNewValidation checks settings validation and URL policy.
func TestWebNewValidation(t *testing.T) {
	g := newWebServer(t)
	cases := map[string]domain.Connection{
		"missing sitemaps": {ID: "c", Provider: NameWeb},
		"http public host": {ID: "c", Provider: NameWeb, Settings: map[string]string{"sitemaps": "http://example.com/sitemap.xml"}},
		"credentials":      {ID: "c", Provider: NameWeb, Settings: map[string]string{"sitemaps": "https://user:pass@example.com/sitemap.xml"}},
		"fragment":         {ID: "c", Provider: NameWeb, Settings: map[string]string{"sitemaps": "https://example.com/sitemap.xml#frag"}},
		"bad maxpages":     {ID: "c", Provider: NameWeb, Settings: map[string]string{"sitemaps": g.server.URL + "/sitemap.xml", "maxpages": "501"}},
		"bad zero pages":   {ID: "c", Provider: NameWeb, Settings: map[string]string{"sitemaps": g.server.URL + "/sitemap.xml", "maxpages": "0"}},
		"bad allowhosts":   {ID: "c", Provider: NameWeb, Settings: map[string]string{"sitemaps": g.server.URL + "/sitemap.xml", "allowhosts": "https://example.com"}},
		"bad robots":       {ID: "c", Provider: NameWeb, Settings: map[string]string{"sitemaps": g.server.URL + "/sitemap.xml", "respectrobots": "maybe"}},
	}
	for name, conn := range cases {
		if _, err := NewWeb(conn, g.server.Client()); err == nil {
			t.Fatalf("%s: NewWeb succeeded, want a validation error", name)
		}
	}
}

// TestWebCheck validates urlset/index documents and reports bad sitemaps.
func TestWebCheck(t *testing.T) {
	g := newWebServer(t)
	g.site.AddMany(2)
	for _, path := range []string{"/sitemap.xml", "/sitemap-index.xml"} {
		source := webSource(g, webConnection(g, path))
		if err := source.Check(context.Background()); err != nil {
			t.Fatalf("Check(%s): %v", path, err)
		}
	}
	for _, path := range []string{"/missing-sitemap.xml", "/malformed.xml", "/redirect-sitemap.xml"} {
		source := webSource(g, webConnection(g, path))
		if err := source.Check(context.Background()); err == nil {
			t.Fatalf("Check(%s) succeeded, want an error", path)
		}
	}
}

// TestWebReadsPages covers the urlset path: titles, text conversion, body
// noise removal and stable external IDs.
func TestWebReadsPages(t *testing.T) {
	g := newWebServer(t)
	g.site.AddMany(2)
	source := webSource(g, webConnection(g, "/sitemap.xml"))
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: webConnection(g, "/sitemap.xml")})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 2 || batch.More {
		t.Fatalf("items = %d, more = %v", len(batch.Items), batch.More)
	}
	first := batch.Items[0]
	if first.Kind != domain.ItemPage || first.Title != "Page 1 & more" {
		t.Fatalf("kind = %q, title = %q", first.Kind, first.Title)
	}
	if !strings.Contains(first.Body, "Body") || strings.Contains(first.Body, "bad()") {
		t.Fatalf("body = %q", first.Body)
	}
	if !strings.HasPrefix(first.URL, g.server.URL+"/page/") {
		t.Fatalf("url = %q", first.URL)
	}
	if first.Ref.ConnectionID != "web-test" || first.Ref.ExternalID == "" {
		t.Fatalf("ref = %+v", first.Ref)
	}
}

// TestWebSitemapIndex covers sitemap-index expansion.
func TestWebSitemapIndex(t *testing.T) {
	g := newWebServer(t)
	g.site.AddMany(4)
	conn := webConnection(g, "/sitemap-index.xml")
	source := webSource(g, conn)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 4 {
		t.Fatalf("items = %d, want 4", len(batch.Items))
	}
	seen := map[string]bool{}
	for _, item := range batch.Items {
		seen[item.Ref.ExternalID] = true
	}
	if len(seen) != 4 {
		t.Fatalf("unique ids = %d, want 4", len(seen))
	}
}

// TestWebConditionalSitemap asserts ETag/Last-Modified short-circuiting and
// that a completed pass persists a watermark.
func TestWebConditionalSitemap(t *testing.T) {
	g := newWebServer(t)
	g.site.AddMany(2)
	conn := webConnection(g, "/sitemap.xml")
	source := webSource(g, conn)
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if len(first.Items) != 2 || len(first.Checkpoints) != 1 {
		t.Fatalf("items = %d, checkpoints = %d", len(first.Items), len(first.Checkpoints))
	}
	checkpoints := map[string]domain.Checkpoint{first.Checkpoints[0].Stream: first.Checkpoints[0]}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if len(second.Items) != 0 || second.More {
		t.Fatalf("unchanged sitemap returned %d items (more=%v), want none", len(second.Items), second.More)
	}
}

// TestWebIncrementalWatermark asserts that a newer sitemap lastmod refetches
// exactly the changed page.
func TestWebIncrementalWatermark(t *testing.T) {
	g := newWebServer(t)
	g.site.AddMany(3)
	conn := webConnection(g, "/sitemap.xml")
	source := webSource(g, conn)
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if len(first.Items) != 3 {
		t.Fatalf("first items = %d", len(first.Items))
	}
	checkpoints := map[string]domain.Checkpoint{}
	for _, checkpoint := range first.Checkpoints {
		checkpoints[checkpoint.Stream] = checkpoint
	}
	g.site.UpdateItem()
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if len(second.Items) != 1 {
		t.Fatalf("updated items = %d, want 1", len(second.Items))
	}
	if second.Items[0].Ref.ExternalID != first.Items[0].Ref.ExternalID {
		t.Fatalf("updated item id = %q", second.Items[0].Ref.ExternalID)
	}
	if second.Items[0].Title != "Page 1 (update 1)" {
		t.Fatalf("updated title = %q", second.Items[0].Title)
	}
}

// TestWebPagingPageBudget asserts that a large sitemap is consumed across
// reads without losing or duplicating URLs and respects the per-read budget.
func TestWebPagingPageBudget(t *testing.T) {
	g := newWebServer(t)
	g.site.AddMany(10)
	conn := webConnection(g, "/sitemap.xml")
	source := webSource(g, conn)
	checkpoints := map[string]domain.Checkpoint{}
	seen := map[string]bool{}
	batches := 0
	for {
		batches++
		batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints})
		if err != nil {
			t.Fatalf("Read %d: %v", batches, err)
		}
		for _, item := range batch.Items {
			if seen[item.Ref.ExternalID] {
				t.Fatalf("duplicate %s", item.Ref.ExternalID)
			}
			seen[item.Ref.ExternalID] = true
		}
		for _, checkpoint := range batch.Checkpoints {
			checkpoints[checkpoint.Stream] = checkpoint
		}
		if !batch.More {
			break
		}
		if batches > 10 {
			t.Fatal("More never cleared")
		}
	}
	if len(seen) != 10 || batches != 2 {
		t.Fatalf("unique pages = %d across %d batches, want 10 across 2", len(seen), batches)
	}
}

// TestWebMaxPages caps the per-sitemap page list.
func TestWebMaxPages(t *testing.T) {
	g := newWebServer(t)
	g.site.AddMany(5)
	conn := webConnection(g, "/sitemap.xml")
	conn.Settings["maxpages"] = "2"
	source := webSource(g, conn)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 2 || batch.More {
		t.Fatalf("items = %d, more = %v", len(batch.Items), batch.More)
	}
}

// TestWebHostPolicy asserts the same-host default and allowhosts override.
func TestWebHostPolicy(t *testing.T) {
	g := newWebServer(t)
	conn := webConnection(g, "/sitemap-foreign.xml")
	source := webSource(g, conn)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 0 {
		t.Fatalf("foreign page was fetched without allowhosts: %d items", len(batch.Items))
	}
	if *g.foreignHits != 0 {
		t.Fatalf("foreign server hits = %d, want 0", *g.foreignHits)
	}
	allowed := webConnection(g, "/sitemap-foreign.xml")
	parsedForeign, err := url.Parse(g.foreign.URL)
	if err != nil {
		t.Fatal(err)
	}
	allowed.Settings["allowhosts"] = parsedForeign.Host
	source = webSource(g, allowed)
	batch, err = source.Read(context.Background(), ports.ReadRequest{Connection: allowed})
	if err != nil {
		t.Fatalf("allowed Read: %v", err)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("allowed items = %d, want 1", len(batch.Items))
	}
	if *g.foreignHits != 1 {
		t.Fatalf("foreign server hits = %d, want 1", *g.foreignHits)
	}
}

// TestWebIndexForeignChildSkipped asserts index children outside the host
// policy are never fetched.
func TestWebIndexForeignChildSkipped(t *testing.T) {
	g := newWebServer(t)
	conn := webConnection(g, "/sitemap-index-foreign.xml")
	source := webSource(g, conn)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 0 {
		t.Fatalf("foreign child sitemap produced %d items", len(batch.Items))
	}
	if *g.foreignHits != 0 {
		t.Fatalf("foreign hits = %d, want 0", *g.foreignHits)
	}
}

// TestWebRedirectsNotFollowed asserts redirects are reported, not fetched.
func TestWebRedirectsNotFollowed(t *testing.T) {
	g := newWebServer(t)
	conn := webConnection(g, "/redirect-sitemap.xml")
	source := webSource(g, conn)
	if _, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn}); err != nil {
		t.Fatalf("redirected sitemap should be skipped, got %v", err)
	}
	if *g.foreignHits != 0 {
		t.Fatalf("redirect target was fetched: hits = %d", *g.foreignHits)
	}
	g.site.AddMany(1)
	conn = webConnection(g, "/sitemap-redirect-page.xml")
	source = webSource(g, conn)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 0 || *g.foreignHits != 0 {
		t.Fatalf("redirected page produced %d items and %d foreign hits", len(batch.Items), *g.foreignHits)
	}
}

// TestWebRobots asserts Disallow enforcement and the respectrobots toggle.
func TestWebRobots(t *testing.T) {
	g := newWebServer(t)
	g.site.AddMany(2)
	g.site.robots = "User-agent: *\nDisallow: /page/\n"
	conn := webConnection(g, "/sitemap.xml")
	source := webSource(g, conn)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 0 {
		t.Fatalf("robots-disallowed pages were fetched: %d", len(batch.Items))
	}
	conn = webConnection(g, "/sitemap.xml")
	conn.Settings["respectrobots"] = "false"
	source = webSource(g, conn)
	batch, err = source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 2 {
		t.Fatalf("robots disabled: %d items, want 2", len(batch.Items))
	}
}

// TestWebBodyBounds caps page bodies at the domain limit.
func TestWebBodyBounds(t *testing.T) {
	g := newWebServer(t)
	g.site.AddSized(int(maxBodyBytes) + 4096)
	conn := webConnection(g, "/sitemap.xml")
	source := webSource(g, conn)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 1 || len(batch.Items[0].Body) != maxBodyBytes {
		t.Fatalf("body bytes = %d, want %d", len(batch.Items[0].Body), maxBodyBytes)
	}
}

// TestWebCursorMalformed rejects broken and mismatched cursors.
func TestWebCursorMalformed(t *testing.T) {
	g := newWebServer(t)
	g.site.AddMany(1)
	conn := webConnection(g, "/sitemap.xml")
	stream := streamOf(webStreamPrefix, conn.Settings["sitemaps"])
	source := webSource(g, conn)
	bad := domain.Checkpoint{Stream: stream, Version: checkpointV1, Data: []byte("{not json")}
	if _, err := source.Read(context.Background(), ports.ReadRequest{
		Connection:  conn,
		Checkpoints: map[string]domain.Checkpoint{stream: bad},
	}); err == nil {
		t.Fatal("malformed cursor was accepted")
	}
	wrong, err := json.Marshal(webCursor{SitemapHash: "other"})
	if err != nil {
		t.Fatal(err)
	}
	mismatch := domain.Checkpoint{Stream: stream, Version: checkpointV1, Data: wrong}
	if _, err := source.Read(context.Background(), ports.ReadRequest{
		Connection:  conn,
		Checkpoints: map[string]domain.Checkpoint{stream: mismatch},
	}); err == nil {
		t.Fatal("cursor for another sitemap was accepted")
	}
}
