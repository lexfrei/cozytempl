package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/lexfrei/cozytempl/internal/audit"
	"github.com/lexfrei/cozytempl/internal/auth"
)

// recordingAudit captures every event so tests can assert the
// pod.log.view outcome chosen on the guard / failure branches.
type recordingAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordingAudit) Record(_ context.Context, evt *audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, *evt)
}

func (r *recordingAudit) snapshot() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]audit.Event, len(r.events))
	copy(out, r.events)

	return out
}

// newWSLogHandlerForTest keeps every test from re-typing the four
// constructor args. LogService is nil because no test here drives
// Stream far enough to open a real log stream — the branches
// under test bail before that (401 / 400 / same-origin).
func newWSLogHandlerForTest() *WSLogHandler {
	return NewWSLogHandler(nil, nil, "dev", slog.New(slog.DiscardHandler))
}

// TestWSLogHandlerRequiresAuth pins the 401 guard. In production
// the /api/* routes sit behind RequireAuth, but the handler's
// own check is belt-and-braces: a misconfigured route without
// the middleware must not leak pod log access to anonymous
// callers.
func TestWSLogHandlerRequiresAuth(t *testing.T) {
	t.Parallel()

	handler := newWSLogHandlerForTest()

	req := httptest.NewRequestWithContext(context.Background(),
		http.MethodGet, "/api/logs/stream?tenant=ns&pod=vm", nil)

	rec := httptest.NewRecorder()
	handler.Stream(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestWSLogHandlerRequiresTenantAndPod locks the 400 path so a
// misconfigured client doesn't open a WebSocket against an
// apiserver URL with an empty namespace and accidentally
// engage cluster-wide pod log RBAC behaviour. Both tenant and
// pod are mandatory.
func TestWSLogHandlerRequiresTenantAndPod(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
	}{
		{"both-missing", ""},
		{"tenant-missing", "?pod=vm"},
		{"pod-missing", "?tenant=ns"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			handler := newWSLogHandlerForTest()

			req := httptest.NewRequestWithContext(
				withTestUser(context.Background()),
				http.MethodGet, "/api/logs/stream"+tc.query, nil)

			rec := httptest.NewRecorder()
			handler.Stream(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}

			if !strings.Contains(rec.Body.String(), "tenant and pod parameters required") {
				t.Errorf("body = %q, want required-params copy", rec.Body.String())
			}
		})
	}
}

// TestWSLogHandlerAuditsMalformedRequestAsDenied pins the
// contract the cycle-3 review called out: an authenticated
// user whose 400 request landed (missing tenant / pod) must
// produce an audit event with outcome=denied, matching the
// SecretReveal / ConnectionView precedent on other handlers.
// Without this, a probing pattern (hit /api/logs/stream
// without params) leaves no trail.
func TestWSLogHandlerAuditsMalformedRequestAsDenied(t *testing.T) {
	t.Parallel()

	rec := &recordingAudit{}
	handler := NewWSLogHandler(nil, rec, "dev", slog.New(slog.DiscardHandler))

	req := httptest.NewRequestWithContext(
		withTestUser(context.Background()),
		http.MethodGet, "/api/logs/stream", nil)

	w := httptest.NewRecorder()
	handler.Stream(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}

	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1 denied audit event; events = %+v", len(events), events)
	}

	got := events[0]
	if got.Action != audit.ActionPodLogView {
		t.Errorf("Action = %q, want %q", got.Action, audit.ActionPodLogView)
	}

	if got.Outcome != audit.OutcomeDenied {
		t.Errorf("Outcome = %q, want Denied", got.Outcome)
	}
}

// TestSameOriginOnlyAcceptsMatch confirms the WebSocket
// CheckOrigin path accepts same-origin handshakes. The full set
// of possible schemes is matched: a vanilla HTTP dev install
// needs the ws:// Origin match, prod behind TLS gets the wss://
// equivalent.
func TestSameOriginOnlyAcceptsMatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{"empty-origin-accepts", "", "cozytempl.example.com", true},
		{"http-same-origin", "http://cozytempl.example.com", "cozytempl.example.com", true},
		{"https-same-origin", "https://cozytempl.example.com", "cozytempl.example.com", true},
		{"cross-origin-rejected", "https://evil.example.com", "cozytempl.example.com", false},
		{"scheme-mismatch-rejected", "http://cozytempl.example.com", "other.example.com", false},
		// IPv6 coverage: a browser sending https://[::1]:443
		// against a Host header of "[::1]" (no port) must
		// resolve to the same canonical host after bracket +
		// default-port stripping. The previous suffix-compare
		// implementation misclassified this as cross-origin.
		{"ipv6-default-port", "https://[::1]:443", "[::1]", true},
		{"ipv6-bare", "http://[::1]", "[::1]:80", true},
		{"ipv6-nondefault-port", "http://[::1]:8080", "[::1]:8080", true},
		{"ipv6-different-port-rejected", "http://[::1]:8080", "[::1]:9090", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(context.Background(),
				http.MethodGet, "/api/logs/stream", nil)
			req.Host = tc.host

			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}

			if got := sameOriginOnly(req); got != tc.want {
				t.Errorf("sameOriginOnly(origin=%q host=%q) = %v, want %v",
					tc.origin, tc.host, got, tc.want)
			}
		})
	}
}

// TestParseTailParam pins the ?tail= semantics:
//
//   - empty / invalid / negative → default (500) so a client
//     that forgets the param still gets a useful bootstrap;
//   - over the max → clamp to logTailMax so a caller cannot ask
//     the apiserver for an arbitrarily large initial slice;
//   - valid in-range → pass through unchanged.
//
// The reconnect path in static/ts/logs.ts sends tail=0 after the
// first connect to skip history replay; that only works if this
// code accepts 0 as a legitimate value, which the "zero passes
// through" case documents.
func TestParseTailParam(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want int64
	}{
		{"", logTailDefault},
		{"not-a-number", logTailDefault},
		{"-5", logTailDefault},
		{"0", 0},
		{"100", 100},
		{"9999999", logTailMax},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()

			if got := parseTailParam(tc.raw); got != tc.want {
				t.Errorf("parseTailParam(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// TestClassifyStreamError pins the outcome mapping the audit
// event records for a failed StreamLogs. A Forbidden or
// Unauthorized error from the apiserver — wrapped via
// fmt.Errorf the way StreamLogs wraps its returns — must
// surface as OutcomeDenied so downstream audit queries looking
// for denied-access events see it. Everything else (pod
// missing, apiserver down, transport glitch) falls through to
// OutcomeError.
//
// Uses the real apierrors factories so the test would catch a
// regression to substring matching on rendered messages, which
// is brittle across apiserver versions and broke with the
// original implementation.
func TestClassifyStreamError(t *testing.T) {
	t.Parallel()

	gr := schema.GroupResource{Group: "", Resource: "pods"}

	cases := []struct {
		name string
		err  error
		want audit.Outcome
	}{
		{
			"forbidden-wrapped",
			fmt.Errorf("opening pod log stream ns/pod: %w",
				apierrors.NewForbidden(gr, "pod", errors.New("no RBAC"))),
			audit.OutcomeDenied,
		},
		{
			"unauthorized-wrapped",
			fmt.Errorf("opening pod log stream ns/pod: %w",
				apierrors.NewUnauthorized("token expired")),
			audit.OutcomeDenied,
		},
		{
			"not-found-wrapped",
			fmt.Errorf("opening pod log stream ns/pod: %w",
				apierrors.NewNotFound(gr, "pod")),
			audit.OutcomeError,
		},
		{
			"plain-error",
			errors.New("context deadline exceeded"),
			audit.OutcomeError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := classifyStreamError(tc.err)
			if got != tc.want {
				t.Errorf("classifyStreamError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestRecordAuditEmitsOutcome pins that recordAudit actually
// passes the provided outcome through to the underlying
// audit.Logger. Without this guard a future refactor that hard-
// codes OutcomeSuccess would silently regress the denial-
// accuracy contract the cycle-1 review called out as blocking.
func TestRecordAuditEmitsOutcome(t *testing.T) {
	t.Parallel()

	rec := &recordingAudit{}

	handler := NewWSLogHandler(nil, rec, "dev", slog.New(slog.DiscardHandler))
	usr := testUser(t)

	handler.recordAudit(context.Background(), usr,
		"tenant-root", "vm-42", "main", 500,
		audit.OutcomeDenied, "pods \"vm-42\" is forbidden")

	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	got := events[0]
	if got.Action != audit.ActionPodLogView {
		t.Errorf("Action = %q, want %q", got.Action, audit.ActionPodLogView)
	}

	if got.Outcome != audit.OutcomeDenied {
		t.Errorf("Outcome = %q, want Denied", got.Outcome)
	}

	if got.Tenant != "tenant-root" || got.Resource != "vm-42" {
		t.Errorf("Tenant/Resource = %q/%q, want tenant-root/vm-42", got.Tenant, got.Resource)
	}

	if got.Details["error"] != "pods \"vm-42\" is forbidden" {
		t.Errorf("Details[error] = %v, want forbidden message", got.Details["error"])
	}
}

// testUser constructs a bare auth.UserContext for a handler-
// level test. Kept local so each test file stays self-contained.
func testUser(t *testing.T) *auth.UserContext {
	t.Helper()

	return &auth.UserContext{Username: "test-user"}
}

// TestWSLogHandlerAcceptsNilAudit confirms the constructor
// substitutes a NopLogger when auditLog is nil so a production
// wiring that forgot to pass the audit dependency does not
// nil-panic on the first request. The handler's own Record call
// is exercised implicitly by compile — if the substitution
// regressed, every subsequent call would segfault.
func TestWSLogHandlerAcceptsNilAudit(t *testing.T) {
	t.Parallel()

	h := NewWSLogHandler(nil, nil, "dev", slog.New(slog.DiscardHandler))
	if h.audit == nil {
		t.Fatal("audit logger left nil; constructor must substitute NopLogger")
	}

	// Touch Record to prove the substituted logger is usable.
	h.audit.Record(context.Background(), &audit.Event{
		Action:  audit.ActionPodLogView,
		Outcome: audit.OutcomeSuccess,
	})
}
