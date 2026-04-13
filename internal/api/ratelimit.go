package api

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lexfrei/cozytempl/internal/auth"
	"golang.org/x/time/rate"
)

// Rate limiting parameters. We pick numbers that let a human using
// the UI freely (the create-tenant flow fires ~6 calls in a second)
// and cap scripted abuse at a level that cannot overwhelm the k8s
// control plane through our proxy.
//
// burst is the depth of the token bucket: a single interactive
// action like "open the tenant page" easily consumes 10 tokens in
// under a second.
//
// refillPerSecond is the sustained rate. 20 req/s per user sustained
// is far beyond any real UI usage but well under what the k8s API
// can handle comfortably.
const (
	rateLimitBurst           = 30
	rateLimitRefillPerSecond = 20
	rateLimitGCInterval      = 5 * time.Minute
	rateLimitIdleTimeout     = 15 * time.Minute
)

// perUserLimiter stores a token bucket plus last-used timestamp so
// the janitor can drop stale buckets. Without the janitor, a
// long-running cozytempl would accumulate one bucket per unique
// user name forever and leak memory in proportion to cluster
// churn.
type perUserLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// rateLimitStore keeps a token bucket per user. The mutex protects
// both map access and the per-user lastSeen timestamp update.
type rateLimitStore struct {
	mu       sync.Mutex
	buckets  map[string]*perUserLimiter
	burst    int
	rate     rate.Limit
	janitor  *time.Ticker
	stopOnce sync.Once
}

// newRateLimitStore creates a store with the cozytempl rate constants.
// The background janitor starts immediately; call stop when the
// HTTP server shuts down if you need a clean test teardown (not
// required in production because the process exits).
func newRateLimitStore() *rateLimitStore {
	store := &rateLimitStore{
		buckets: make(map[string]*perUserLimiter),
		burst:   rateLimitBurst,
		rate:    rate.Limit(rateLimitRefillPerSecond),
		janitor: time.NewTicker(rateLimitGCInterval),
	}

	go store.gcLoop()

	return store
}

// gcLoop drops buckets idle for longer than rateLimitIdleTimeout.
// A user who reconnects after the GC simply gets a fresh bucket
// with the full burst, which is the correct behaviour.
func (rls *rateLimitStore) gcLoop() {
	for range rls.janitor.C {
		rls.mu.Lock()

		cutoff := time.Now().Add(-rateLimitIdleTimeout)
		for key, bucket := range rls.buckets {
			if bucket.lastSeen.Before(cutoff) {
				delete(rls.buckets, key)
			}
		}

		rls.mu.Unlock()
	}
}

// stop halts the background janitor. Tests call this in cleanup
// so the goroutine doesn't outlive the test and cross-contaminate
// later runs.
func (rls *rateLimitStore) stop() {
	rls.stopOnce.Do(func() {
		rls.janitor.Stop()
	})
}

// allow consults the per-user bucket and returns whether the
// caller may proceed. Updates lastSeen on every call so the
// janitor only evicts truly idle buckets.
func (rls *rateLimitStore) allow(key string) bool {
	rls.mu.Lock()

	bucket, ok := rls.buckets[key]
	if !ok {
		bucket = &perUserLimiter{
			limiter: rate.NewLimiter(rls.rate, rls.burst),
		}
		rls.buckets[key] = bucket
	}

	bucket.lastSeen = time.Now()
	rls.mu.Unlock()

	return bucket.limiter.Allow()
}

// withRateLimit wraps a handler in a per-authenticated-user token
// bucket. Unauthenticated requests bypass the limiter because they
// can't be attributed to any identity — OIDC callback, static
// assets, /metrics, /healthz all fall through. The point is to
// protect the k8s API from a single logged-in user running a
// scraping script; public endpoints are either cached or
// rate-limited by the upstream proxy anyway.
//
// On block we return 429 with Retry-After set to 1 second. For
// htmx clients we also flip HX-Reswap: none so the error doesn't
// blank the current view — the user sees a toast and keeps their
// work.
func withRateLimit(store *rateLimitStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		usr := auth.UserFromContext(req.Context())
		if usr == nil {
			next.ServeHTTP(writer, req)

			return
		}

		if !store.allow(usr.Username) {
			writer.Header().Set("Retry-After", "1")
			writer.Header().Set("Hx-Reswap", "none")
			http.Error(writer, "rate limit exceeded", http.StatusTooManyRequests)

			return
		}

		next.ServeHTTP(writer, req)
	})
}

// withIPRateLimit wraps a pre-auth handler (the kubeconfig upload,
// the token paste) in an IP-keyed token bucket reusing the same
// store as withRateLimit. The keys are prefixed with "ip:" so they
// live in a distinct namespace from usernames and cannot collide.
// trustForwardedHeaders controls whether X-Forwarded-For is honoured
// — only enable it when cozytempl runs behind a trusted proxy that
// strips client-supplied XFF values. Without this wrapper the paste
// endpoints perform an unauthenticated SAR round-trip to the
// apiserver on every request, which a loose attacker could spin
// into an arbitrarily high-rate load source.
func withIPRateLimit(store *rateLimitStore, trustForwardedHeaders bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		key := "ip:" + clientIP(req, trustForwardedHeaders)

		if !store.allow(key) {
			writer.Header().Set("Retry-After", "1")
			http.Error(writer, "rate limit exceeded", http.StatusTooManyRequests)

			return
		}

		next.ServeHTTP(writer, req)
	})
}

// clientIP returns the best-effort client IP for rate-limiting
// purposes. When trustForwardedHeaders is true the left-most
// X-Forwarded-For entry wins — this is only safe when the proxy
// in front of cozytempl strips client-supplied XFF. When false
// the function falls back to RemoteAddr unconditionally, so a
// hostile client cannot spoof XFF to bypass the limiter. The
// returned string may include a port — rate-limit keys do not need
// to be canonical.
func clientIP(req *http.Request, trustForwardedHeaders bool) string {
	if trustForwardedHeaders {
		xff := req.Header.Get("X-Forwarded-For")
		if xff != "" {
			if idx := strings.Index(xff, ","); idx > 0 {
				return strings.TrimSpace(xff[:idx])
			}

			return strings.TrimSpace(xff)
		}
	}

	return req.RemoteAddr
}
