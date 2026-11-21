package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metrics counts what the service does and prints the counts in the
// Prometheus text format, which most monitoring systems can scrape.
//
// It is written with the standard library to avoid a dependency. Every label
// value comes from a fixed set (route patterns, status codes, cache results),
// never from client input, so the number of series stays small and no flag
// name or user ID can appear in the output.
type metrics struct {
	mu sync.Mutex

	requests      map[requestSeries]uint64 // requests by route and status
	durationSum   map[string]float64       // seconds spent, by route
	durationCount map[string]uint64        // requests timed, by route
	cacheResults  map[string]uint64        // evaluations by hit, miss, bypass

	// cacheEntries reports how many results the cache holds right now.
	cacheEntries func() int
}

// requestSeries identifies one http_requests_total counter.
type requestSeries struct {
	route  string
	status int
}

// newMetrics returns an empty metrics collector.
func newMetrics(cacheEntries func() int) *metrics {
	return &metrics{
		requests:      map[requestSeries]uint64{},
		durationSum:   map[string]float64{},
		durationCount: map[string]uint64{},
		cacheResults:  map[string]uint64{},
		cacheEntries:  cacheEntries,
	}
}

// observeRequest records one finished request. A nil collector records
// nothing, which keeps the middleware usable without metrics.
func (m *metrics) observeRequest(route string, status int, duration time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[requestSeries{route: route, status: status}]++
	m.durationSum[route] += duration.Seconds()
	m.durationCount[route]++
}

// observeCacheResult records how one evaluation was answered: "hit", "miss",
// or "bypass".
func (m *metrics) observeCacheResult(result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheResults[result]++
}

// write prints every metric in the Prometheus text format, in a stable
// order.
func (m *metrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder

	b.WriteString("# HELP http_requests_total Requests handled, by route pattern and status code.\n")
	b.WriteString("# TYPE http_requests_total counter\n")
	series := make([]requestSeries, 0, len(m.requests))
	for s := range m.requests {
		series = append(series, s)
	}
	sort.Slice(series, func(i, j int) bool {
		if series[i].route != series[j].route {
			return series[i].route < series[j].route
		}
		return series[i].status < series[j].status
	})
	for _, s := range series {
		fmt.Fprintf(&b, "http_requests_total{route=%s,status=\"%d\"} %d\n", strconv.Quote(s.route), s.status, m.requests[s])
	}

	// Sum and count give the average latency per route. Percentiles need a
	// histogram; see docs/next-steps.md.
	b.WriteString("# HELP http_request_duration_seconds Time spent handling requests, by route pattern.\n")
	b.WriteString("# TYPE http_request_duration_seconds summary\n")
	routes := make([]string, 0, len(m.durationCount))
	for route := range m.durationCount {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	for _, route := range routes {
		fmt.Fprintf(&b, "http_request_duration_seconds_sum{route=%s} %g\n", strconv.Quote(route), m.durationSum[route])
		fmt.Fprintf(&b, "http_request_duration_seconds_count{route=%s} %d\n", strconv.Quote(route), m.durationCount[route])
	}

	// hit / (hit + miss) is the cache hit ratio.
	b.WriteString("# HELP evaluation_cache_results_total Evaluations by how the cache answered.\n")
	b.WriteString("# TYPE evaluation_cache_results_total counter\n")
	for _, result := range []string{"hit", "miss", "bypass"} {
		fmt.Fprintf(&b, "evaluation_cache_results_total{result=%q} %d\n", result, m.cacheResults[result])
	}

	// If this sits at CACHE_MAX_ENTRIES, the cache is full and evicting.
	b.WriteString("# HELP evaluation_cache_entries Evaluation results held in the cache now.\n")
	b.WriteString("# TYPE evaluation_cache_entries gauge\n")
	fmt.Fprintf(&b, "evaluation_cache_entries %d\n", m.cacheEntries())

	_, _ = io.WriteString(w, b.String())
}

// metricsHandler handles GET /metrics. It requires an API key like every
// other route, because the service is reachable from the internet.
func (a *api) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	a.metrics.write(w)
}
