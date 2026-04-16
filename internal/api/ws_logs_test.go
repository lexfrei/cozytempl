package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWSLogHandlerRequiresAuth pins the 401 guard. In production
// the /api/* routes sit behind RequireAuth, but the handler's
// own check is belt-and-braces: a misconfigured route without
// the middleware must not leak pod log access to anonymous
// callers.
func TestWSLogHandlerRequiresAuth(t *testing.T) {
	t.Parallel()

	handler := NewWSLogHandler(nil, slog.New(slog.DiscardHandler))

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

			handler := NewWSLogHandler(nil, slog.New(slog.DiscardHandler))

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
