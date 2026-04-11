// Package audit emits structured audit events for every security-
// relevant action cozytempl takes on behalf of a user. The default
// implementation writes JSON lines to the provided slog.Logger at
// a dedicated "audit" level, so the stream lands in the same pod
// logs everything else goes to — pod logs forwarded to Loki / ELK
// become the append-only audit store without any new infrastructure.
//
// The contract is deliberately narrow: every call to Logger.Record
// produces one immutable event. Callers never mutate Event after
// handing it off. Adding a new action type means adding a constant
// to this file so the set of possible values stays discoverable in
// code review.
//
// This package also owns the per-request correlation ID context
// key. It has to live somewhere both `api` (which mints the ID in
// withRequestID middleware) and `handler` (which reads it when
// emitting audit events) can import without creating a cycle. The
// audit package is low enough in the dependency DAG to be that
// neutral ground.
package audit

import (
	"context"
	"log/slog"
	"time"
)

// requestIDKey is the context key for the per-request correlation
// ID. An unexported zero-sized type prevents collisions with any
// other package using context values and keeps the key value
// inaccessible to outside code.
type requestIDKey struct{}

// RequestIDFromContext returns the correlation ID attached to ctx
// by the api.withRequestID middleware, or an empty string if none
// was set. Every handler that emits an audit event uses this so
// the event lines up with the HTTP access log entry.
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}

	return ""
}

// ContextWithRequestID returns a child context with the given
// correlation ID attached. The api package's withRequestID
// middleware is the canonical caller; tests can use this to seed
// a context before invoking a handler directly.
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// Action is the verb-object pair that identifies what happened.
// The constants are stable strings that the log pipeline can index
// on — never rename a shipped action without a migration plan,
// because operators will have alert rules tied to the old name.
type Action string

// Action constants cover the full set of mutation and
// secret-access events cozytempl currently emits. Adding a new
// one is how you declare "this is a sensitive action worth
// persisting in the audit trail" and shows up in code review.
const (
	ActionTenantCreate   Action = "tenant.create"
	ActionTenantUpdate   Action = "tenant.update"
	ActionTenantDelete   Action = "tenant.delete"
	ActionAppCreate      Action = "app.create"
	ActionAppUpdate      Action = "app.update"
	ActionAppDelete      Action = "app.delete"
	ActionSecretView     Action = "secret.view"
	ActionConnectionView Action = "connection.view"
	ActionAuthLogin      Action = "auth.login"
	ActionAuthLogout     Action = "auth.logout"
)

// Outcome captures whether the action succeeded. Keeping it as a
// separate field (instead of folding it into Action) means the
// same action string means the same thing regardless of whether
// it succeeded, which makes log queries for "all deletes" straight-
// forward — you filter by action only, and can optionally further
// narrow by outcome.
type Outcome string

const (
	// OutcomeSuccess is the happy path — the action completed.
	OutcomeSuccess Outcome = "success"
	// OutcomeDenied is used when RBAC or our own policy refused
	// the action (e.g. invalid name, reserved tenant, RBAC 403).
	OutcomeDenied Outcome = "denied"
	// OutcomeError is any other failure: k8s API down, conflict,
	// unexpected exception. The Details field should carry the
	// error message.
	OutcomeError Outcome = "error"
)

// Event is one audit record. It is JSON-serialisable by design —
// every field must round-trip through encoding/json so an operator
// can rehydrate a log line into a query tool.
//
// Field tags are snake_case to match every other log field in the
// project and the broader k8s ecosystem conventions; tagliatelle's
// default camelCase preference is disabled here deliberately.
//
//nolint:tagliatelle // snake_case matches cozytempl log stream convention
type Event struct {
	Time      time.Time      `json:"time"`
	RequestID string         `json:"request_id,omitempty"`
	Actor     string         `json:"actor"`
	Groups    []string       `json:"groups,omitempty"`
	Action    Action         `json:"action"`
	Resource  string         `json:"resource,omitempty"`
	Tenant    string         `json:"tenant,omitempty"`
	Outcome   Outcome        `json:"outcome"`
	AuthMode  string         `json:"auth_mode,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// Logger is the abstraction the rest of the app depends on, so a
// test can swap in an in-memory recorder and a future operator can
// replace the slog backend with e.g. a Kafka producer without
// touching any handler code. Record takes *Event because Event is
// ~150 bytes; pass-by-pointer avoids copying on every call.
type Logger interface {
	Record(ctx context.Context, event *Event)
}

// SlogLogger writes audit events as JSON lines at the configured
// level to an underlying slog.Logger. The "audit" marker attribute
// makes it trivial to split the audit stream from regular logs in
// downstream filters.
type SlogLogger struct {
	log *slog.Logger
}

// NewSlogLogger constructs a SlogLogger. Pass nil to fall back to
// slog.Default() so a forgotten wiring doesn't panic — audit lines
// still end up somewhere operator-visible.
func NewSlogLogger(log *slog.Logger) *SlogLogger {
	if log == nil {
		log = slog.Default()
	}

	return &SlogLogger{log: log}
}

// Record emits a single audit event. Time is stamped server-side
// so a caller that forgets to set it cannot back-date events.
// Missing Outcome defaults to success rather than an empty string,
// because the audit stream must never contain untyped enums.
// The event pointer is mutated to fill defaults — acceptable
// because the documented contract is that the caller hands off
// the event and never touches it again.
func (logger *SlogLogger) Record(ctx context.Context, event *Event) {
	if event == nil {
		return
	}

	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}

	if event.Outcome == "" {
		event.Outcome = OutcomeSuccess
	}

	attrs := []slog.Attr{
		slog.String("audit", "event"),
		slog.Time("event_time", event.Time),
		slog.String("request_id", event.RequestID),
		slog.String("actor", event.Actor),
		slog.Any("groups", event.Groups),
		slog.String("action", string(event.Action)),
		slog.String("resource", event.Resource),
		slog.String("tenant", event.Tenant),
		slog.String("outcome", string(event.Outcome)),
		slog.String("auth_mode", event.AuthMode),
	}

	if len(event.Details) > 0 {
		attrs = append(attrs, slog.Any("details", event.Details))
	}

	logger.log.LogAttrs(ctx, slog.LevelInfo, "audit", attrs...)
}

// NopLogger is a do-nothing implementation for callers that want
// to disable auditing entirely (tests that don't care, early-stage
// scripts). Using this is always opt-in — default wiring uses
// SlogLogger.
type NopLogger struct{}

// Record drops the event. It accepts the same pointer signature
// as SlogLogger so swapping implementations is mechanical.
func (NopLogger) Record(_ context.Context, _ *Event) {}
