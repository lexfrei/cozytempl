package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Path constants shared between router and metrics so there is a
// single source of truth for label values and the lint-checker stops
// complaining about duplicated string literals.
const (
	pathRoot        = "/"
	pathMarketplace = "/marketplace"
	pathProfile     = "/profile"
	pathTenants     = "/tenants"
	pathMetrics     = "/metrics"
	pathSSEEvents   = "/api/events"
	// pathWatchPrefix matches every user-credentialed watch stream
	// served by WatchSSEHandler. Stream lifetime is ~30 minutes, so
	// including them in request-duration histograms and inflight
	// gauges would pin the +Inf bucket and leave the gauge
	// permanently elevated with no meaningful load signal.
	pathWatchPrefix = "/api/watch/"
)

// Request histogram buckets tuned for an htmx UI fronting a k8s API.
// The bulk of requests finish inside 100 ms (cached reads, fragment
// renders) but tenant-listing spikes can push into several seconds
// when the control plane is under load, so the tail is generous.
//
//nolint:gochecknoglobals // buckets are effectively a constant, var only because slices can't be
var httpDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// metricsRegistry is the Prometheus registry that /metrics serves
// from. Using a private registry (rather than the global
// prometheus.DefaultRegisterer) means we ship exactly the metrics
// declared here — no leaked counters from vendored libraries.
//
//nolint:gochecknoglobals // canonical prometheus pattern: one registry per binary
var metricsRegistry = prometheus.NewRegistry()

//nolint:gochecknoglobals // canonical prometheus pattern: one counter per metric, registered once
var httpRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "cozytempl_http_requests_total",
		Help: "Total HTTP requests processed, labelled by method, path pattern and status class.",
	},
	[]string{"method", "path", "status"},
)

//nolint:gochecknoglobals // canonical prometheus pattern
var httpRequestDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "cozytempl_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: httpDurationBuckets,
	},
	[]string{"method", "path"},
)

//nolint:gochecknoglobals // canonical prometheus pattern
var httpInflight = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "cozytempl_http_requests_inflight",
		Help: "Number of HTTP requests currently being handled.",
	},
)

// Collectors are registered once on package init. We add Go runtime
// and process collectors so /metrics shows goroutine count, GC
// pauses, file descriptors and similar baseline signals for free.
//
//nolint:gochecknoinits // prometheus registration is idiomatic package init
func init() {
	metricsRegistry.MustRegister(
		httpRequestsTotal,
		httpRequestDuration,
		httpInflight,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// metricsHandler serves the /metrics endpoint. Private registry means
// the response lists only cozytempl_* series plus Go/process baseline.
func metricsHandler() http.Handler {
	return promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{
		// Fail loudly if a collector panics rather than silently
		// dropping the series, so a broken collector shows up as
		// an alert instead of a subtle metric gap.
		ErrorHandling: promhttp.PanicOnError,
	})
}

// withMetrics wraps the application mux in a counting middleware.
// SSE is skipped because a long-lived stream would skew the duration
// histogram, and /metrics itself is skipped to avoid self-recursion
// polluting its own series.
func withMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.URL.Path == pathSSEEvents ||
			req.URL.Path == pathMetrics ||
			strings.HasPrefix(req.URL.Path, pathWatchPrefix) {
			next.ServeHTTP(writer, req)

			return
		}

		httpInflight.Inc()
		defer httpInflight.Dec()

		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(recorder, req)

		path := normalisePathForMetrics(req.URL.Path)
		status := strconv.Itoa(recorder.status)

		httpRequestsTotal.WithLabelValues(req.Method, path, status).Inc()
		httpRequestDuration.WithLabelValues(req.Method, path).Observe(time.Since(start).Seconds())
	})
}

// pathPattern tags a URL path with a bounded label so Prometheus
// label cardinality cannot run away when tenants or app names
// appear in the URL.
type pathPattern struct {
	prefix   string
	suffix   string
	contains string
	label    string
}

// pathPatterns is scanned top-to-bottom in normalisePathForMetrics;
// the first match wins. Order matters — more specific rules
// (/tenants/:ns/apps/:name) must come before more general ones
// (/tenants/:ns).
//
//nolint:gochecknoglobals // pattern table is effectively a constant map
var pathPatterns = []pathPattern{
	{prefix: "/fragments/", label: "/fragments/*"},
	{prefix: "/static/", label: "/static/*"},
	{prefix: "/auth/", label: ""}, // preserve the exact sub-path; handled inline below
	{prefix: "/tenants/", contains: "/apps/", label: "/tenants/:ns/apps/:name"},
	{prefix: "/tenants/", suffix: "/apps", label: "/tenants/:ns/apps"},
	{prefix: "/tenants/", label: "/tenants/:ns"},
	{prefix: "/api/tenants/", contains: "/apps/", label: "/api/tenants/:ns/apps/:name"},
	{prefix: "/api/tenants/", suffix: "/apps", label: "/api/tenants/:ns/apps"},
	{prefix: "/api/tenants/", label: "/api/tenants/:ns"},
	{prefix: "/api/schemas/", label: "/api/schemas/:kind"},
}

// exactPaths are preserved as-is in metric labels. Any URL not in
// this set runs through the pattern table.
//
//nolint:gochecknoglobals // effectively a constant lookup table
var exactPaths = map[string]string{
	"":              pathRoot,
	pathRoot:        pathRoot,
	pathMarketplace: pathMarketplace,
	pathProfile:     pathProfile,
	pathTenants:     pathTenants,
	"/healthz":      "/healthz",
	"/readyz":       "/readyz",
}

// normalisePathForMetrics collapses request paths into a bounded set
// of labels. Without this, every `/tenants/{ns}` and
// `/tenants/{ns}/apps/{name}` produces a unique label value and
// Prometheus cardinality runs away.
func normalisePathForMetrics(urlPath string) string {
	if label, ok := exactPaths[urlPath]; ok {
		return label
	}

	// /auth/login, /auth/callback, /auth/logout — small fixed set,
	// preserve as-is so the dashboard can distinguish them.
	if strings.HasPrefix(urlPath, "/auth/") {
		return urlPath
	}

	if label := matchPathPattern(urlPath); label != "" {
		return label
	}

	if strings.HasPrefix(urlPath, "/api/") {
		return urlPath
	}

	return "other"
}

// matchPathPattern walks pathPatterns top-to-bottom and returns the
// first matching label, or empty string if nothing matches.
func matchPathPattern(urlPath string) string {
	for _, pat := range pathPatterns {
		if pat.prefix == "/auth/" {
			continue // handled inline in normalisePathForMetrics
		}

		if !strings.HasPrefix(urlPath, pat.prefix) {
			continue
		}

		if pat.contains != "" && !strings.Contains(urlPath, pat.contains) {
			continue
		}

		if pat.suffix != "" && !strings.HasSuffix(urlPath, pat.suffix) {
			continue
		}

		return pat.label
	}

	return ""
}
