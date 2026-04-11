package api

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// requestIDKey is the private context key for the per-request ID.
// Declaring a dedicated empty-struct type makes it impossible for any
// other package to collide with our key, even accidentally.
type requestIDKey struct{}

// maxInboundRequestIDLength caps the length of a client-supplied
// X-Request-ID. We accept trace IDs coming from an upstream proxy
// (Cloudflare, nginx, Envoy) so a single ID ties a request together
// across hops, but anything beyond 64 characters or containing
// non-ASCII looks hostile and is discarded in favour of a fresh UUID.
const maxInboundRequestIDLength = 64

// inboundRequestIDPattern only accepts alphanumerics, hyphens and
// underscores. Wide enough for both UUIDs and hex-style IDs,
// narrow enough that a log-injection payload cannot slip through as
// a request ID and wind up in the access log.
var inboundRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// RequestIDFromContext returns the correlation ID attached to the
// request by withRequestID, or an empty string if the context is
// not from an incoming HTTP request.
func RequestIDFromContext(ctx context.Context) string {
	if requestID, ok := ctx.Value(requestIDKey{}).(string); ok {
		return requestID
	}

	return ""
}

// withRequestID attaches a correlation ID to every incoming request.
// If the client (or an upstream proxy) already set an X-Request-ID
// header with a sane value, we honour it so distributed traces
// stitch together across hops; otherwise we mint a fresh UUID.
// The ID is echoed in the response header, stored on the request
// context, and — when logAccess is wired — emitted as an access-log
// field so grep'ing `request_id=<x>` finds every log line from the
// same request.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		requestID := req.Header.Get("X-Request-ID")
		if len(requestID) > maxInboundRequestIDLength || !inboundRequestIDPattern.MatchString(requestID) {
			requestID = uuid.NewString()
		}

		writer.Header().Set("X-Request-ID", requestID)

		ctx := context.WithValue(req.Context(), requestIDKey{}, requestID)
		next.ServeHTTP(writer, req.WithContext(ctx))
	})
}

// statusRecorder wraps http.ResponseWriter to capture the status
// code written by the inner handler so the access-log middleware
// can report it. http.ResponseWriter has no built-in reader for
// this; Go's standard library expects you to record it manually.
type statusRecorder struct {
	http.ResponseWriter

	status int
}

// WriteHeader records the status before delegating. We intentionally
// do NOT override Write — Go's default behaviour of implicit 200 on
// first Write is fine, and the status field is initialised to 200
// in withAccessLog so it matches.
func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// withAccessLog emits one structured log line per completed HTTP
// request. Paired with withRequestID, this gives every line a
// correlation ID so post-incident analysis is grep-able. The SSE
// stream is skipped because its lifetime is minutes-to-hours and a
// "request finished" line on disconnect is more misleading than
// useful.
func withAccessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/events" {
			next.ServeHTTP(writer, req)

			return
		}

		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(recorder, req)

		log.LogAttrs(req.Context(), slog.LevelInfo, "http",
			slog.String("request_id", RequestIDFromContext(req.Context())),
			slog.String("method", req.Method),
			slog.String("path", req.URL.Path),
			slog.Int("status", recorder.status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}
