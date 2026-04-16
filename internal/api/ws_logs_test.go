package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lexfrei/cozytempl/internal/audit"
)

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
