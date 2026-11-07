package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/version"
)

// latencyBuckets are the fixed upper bounds (seconds) of the request duration
// histogram.
var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// unmatchedRoute labels requests that no registered route matched.
const unmatchedRoute = "unmatched"

// knownMethods keeps the method label bounded: any other method is "OTHER".
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodOptions: true,
}

// DBStats is a point-in-time snapshot of the database connection pool.
type DBStats struct {
	Acquired int32
	Idle     int32
	Total    int32
	Max      int32
}

// Metrics records HTTP traffic and renders it in the Prometheus text
// exposition format (version 0.0.4). It is hand-written on the standard
// library so the server adds no dependency. Labels use the router pattern,
// never the raw path, which keeps cardinality bounded.
type Metrics struct {
	build BuildInfo

	mu        sync.Mutex
	requests  map[requestKey]uint64
	durations map[routeKey]*histogram

	inFlight atomic.Int64

	sseClients func() int
	dbStats    func() DBStats
}

type requestKey struct {
	method string
	route  string
	code   string
}

type routeKey struct {
	method string
	route  string
}

type histogram struct {
	buckets []uint64 // per-bucket (non-cumulative) counts; last slot is +Inf
	sum     float64
	count   uint64
}

// NewMetrics returns an empty collector for the given build.
func NewMetrics(build BuildInfo) *Metrics {
	return &Metrics{
		build:     build,
		requests:  make(map[requestKey]uint64),
		durations: make(map[routeKey]*histogram),
	}
}

// CurrentBuildInfo describes the running binary for /version and metrics.
func CurrentBuildInfo() BuildInfo {
	return BuildInfo{
		Version: version.Version,
		Commit:  version.Commit,
		Date:    version.Date,
		Go:      runtime.Version(),
	}
}

// SetSSEClients registers the source of the live SSE subscriber count.
func (m *Metrics) SetSSEClients(count func() int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sseClients = count
}

// SetDBStats registers the source of database pool statistics. Memory-mode
// servers never call it and the db_pool_* series are omitted.
func (m *Metrics) SetDBStats(stats func() DBStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dbStats = stats
}

// Handler serves the exposition at any path of the listener it is mounted on.
// The server mounts it on its own listener, never on the public API port.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = m.Render(w)
	})
}

// instrument wraps the router. It resolves the matched route pattern before
// dispatch, records the request, and stores the route for the request log.
func (a *API) instrument(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		route := routeLabel(pattern)
		if state := requestStateFrom(r.Context()); state != nil {
			state.route = route
		}
		m := a.metrics
		if m == nil {
			mux.ServeHTTP(w, r)
			return
		}
		m.inFlight.Add(1)
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		started := time.Now()
		completed := false
		defer func() {
			m.inFlight.Add(-1)
			code := recorder.status
			if !completed {
				// The handler panicked; recoverPanics answers with 500.
				code = http.StatusInternalServerError
			}
			m.observe(r.Method, route, code, time.Since(started))
		}()
		mux.ServeHTTP(recorder, r)
		completed = true
	})
}

// routeLabel converts a ServeMux pattern such as "GET /v1/projects/{id}" into
// the route label "/v1/projects/{id}". Unmatched requests get one stable label.
func routeLabel(pattern string) string {
	if pattern == "" {
		return unmatchedRoute
	}
	if _, path, ok := strings.Cut(pattern, " "); ok {
		return strings.TrimSpace(path)
	}
	return pattern
}

func normalizeMethod(method string) string {
	if knownMethods[method] {
		return method
	}
	return "OTHER"
}

func (m *Metrics) observe(method, route string, code int, elapsed time.Duration) {
	method = normalizeMethod(method)
	seconds := elapsed.Seconds()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[requestKey{method: method, route: route, code: strconv.Itoa(code)}]++

	key := routeKey{method: method, route: route}
	hist, ok := m.durations[key]
	if !ok {
		hist = &histogram{buckets: make([]uint64, len(latencyBuckets)+1)}
		m.durations[key] = hist
	}
	slot := len(latencyBuckets)
	for i, bound := range latencyBuckets {
		if seconds <= bound {
			slot = i
			break
		}
	}
	hist.buckets[slot]++
	hist.sum += seconds
	hist.count++
}

// Render writes every series. Output order is deterministic.
func (m *Metrics) Render(w io.Writer) error {
	m.mu.Lock()
	requests := make([]requestSample, 0, len(m.requests))
	for key, value := range m.requests {
		requests = append(requests, requestSample{key: key, value: value})
	}
	durations := make([]durationSample, 0, len(m.durations))
	for key, hist := range m.durations {
		cp := histogram{buckets: append([]uint64(nil), hist.buckets...), sum: hist.sum, count: hist.count}
		durations = append(durations, durationSample{key: key, hist: cp})
	}
	sseClients, dbStats := m.sseClients, m.dbStats
	m.mu.Unlock()

	var b strings.Builder
	writeBuildInfo(&b, m.build)
	writeRequests(&b, requests)
	writeDurations(&b, durations)
	writeGauge(&b, "http_requests_in_flight", "Requests currently being served.", float64(m.inFlight.Load()))
	if sseClients != nil {
		writeGauge(&b, "sse_clients", "Connected server-sent event subscribers.", float64(sseClients()))
	}
	if dbStats != nil {
		writeDBStats(&b, dbStats())
	}
	_, err := io.WriteString(w, b.String())
	if err != nil {
		return fmt.Errorf("write metrics: %w", err)
	}
	return nil
}

type requestSample struct {
	key   requestKey
	value uint64
}

type durationSample struct {
	key  routeKey
	hist histogram
}

func writeBuildInfo(b *strings.Builder, build BuildInfo) {
	b.WriteString("# HELP brain_build_info Build metadata; the value is always 1.\n")
	b.WriteString("# TYPE brain_build_info gauge\n")
	fmt.Fprintf(b, "brain_build_info{version=\"%s\",commit=\"%s\",go_version=\"%s\"} 1\n",
		escapeLabel(build.Version), escapeLabel(build.Commit), escapeLabel(build.Go))
}

func writeRequests(b *strings.Builder, samples []requestSample) {
	sort.Slice(samples, func(i, j int) bool {
		a, c := samples[i].key, samples[j].key
		if a.route != c.route {
			return a.route < c.route
		}
		if a.method != c.method {
			return a.method < c.method
		}
		return a.code < c.code
	})
	b.WriteString("# HELP http_requests_total Completed HTTP requests by method, route pattern and status code.\n")
	b.WriteString("# TYPE http_requests_total counter\n")
	for _, s := range samples {
		fmt.Fprintf(b, "http_requests_total{method=\"%s\",route=\"%s\",code=\"%s\"} %d\n",
			escapeLabel(s.key.method), escapeLabel(s.key.route), escapeLabel(s.key.code), s.value)
	}
}

func writeDurations(b *strings.Builder, samples []durationSample) {
	sort.Slice(samples, func(i, j int) bool {
		a, c := samples[i].key, samples[j].key
		if a.route != c.route {
			return a.route < c.route
		}
		return a.method < c.method
	})
	b.WriteString("# HELP http_request_duration_seconds Request latency by method and route pattern.\n")
	b.WriteString("# TYPE http_request_duration_seconds histogram\n")
	for _, s := range samples {
		labels := fmt.Sprintf("method=\"%s\",route=\"%s\"", escapeLabel(s.key.method), escapeLabel(s.key.route))
		var cumulative uint64
		for i, bound := range latencyBuckets {
			cumulative += s.hist.buckets[i]
			fmt.Fprintf(b, "http_request_duration_seconds_bucket{%s,le=\"%s\"} %d\n",
				labels, formatFloat(bound), cumulative)
		}
		cumulative += s.hist.buckets[len(latencyBuckets)]
		fmt.Fprintf(b, "http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, cumulative)
		fmt.Fprintf(b, "http_request_duration_seconds_sum{%s} %s\n", labels, formatFloat(s.hist.sum))
		fmt.Fprintf(b, "http_request_duration_seconds_count{%s} %d\n", labels, s.hist.count)
	}
}

func writeDBStats(b *strings.Builder, stats DBStats) {
	writeGauge(b, "db_pool_acquired_connections", "Database connections in use.", float64(stats.Acquired))
	writeGauge(b, "db_pool_idle_connections", "Idle database connections.", float64(stats.Idle))
	writeGauge(b, "db_pool_total_connections", "Database connections open.", float64(stats.Total))
	writeGauge(b, "db_pool_max_connections", "Configured maximum database connections.", float64(stats.Max))
}

func writeGauge(b *strings.Builder, name, help string, value float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, formatFloat(value))
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// escapeLabel applies the exposition-format escaping for label values.
func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return strings.ReplaceAll(value, `"`, `\"`)
}
