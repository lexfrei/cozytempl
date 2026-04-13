package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
)

// TestBuildAuthMiddleware_ModeRouteRegistration asserts that each
// non-OIDC mode registers the auth surface the handler expects —
// specifically POST /auth/logout (which lived in a silent-404 hole
// for BYOK before this branch) and the mode's own sign-in path.
// Regression guard against the earlier wiring bug where BYOK never
// had an AuthHandler created in main.go.
func TestBuildAuthMiddleware_ModeRouteRegistration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		mode              config.AuthMode
		expectLogout      bool
		expectLogin       bool
		expectKubeconfig  bool
		expectTokenRoutes bool
	}{
		{
			name:              "passthrough registers login + logout",
			mode:              config.AuthModePassthrough,
			expectLogout:      true,
			expectLogin:       true,
			expectKubeconfig:  false,
			expectTokenRoutes: false,
		},
		{
			name:              "byok registers kubeconfig + logout, no login",
			mode:              config.AuthModeBYOK,
			expectLogout:      true,
			expectLogin:       false,
			expectKubeconfig:  true,
			expectTokenRoutes: false,
		},
		{
			name:              "token registers token paste + logout, no login",
			mode:              config.AuthModeToken,
			expectLogout:      true,
			expectLogin:       false,
			expectKubeconfig:  false,
			expectTokenRoutes: true,
		},
		{
			name:              "impersonation-legacy registers login + logout",
			mode:              config.AuthModeImpersonationLegacy,
			expectLogout:      true,
			expectLogin:       true,
			expectKubeconfig:  false,
			expectTokenRoutes: false,
		},
		{
			name:              "dev registers no auth routes",
			mode:              config.AuthModeDev,
			expectLogout:      false,
			expectLogin:       false,
			expectKubeconfig:  false,
			expectTokenRoutes: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			handler := auth.NewHandler(
				nil,
				auth.NewSessionStore("test-session-secret-32-bytes-ok!"),
				slog.New(slog.DiscardHandler),
				tc.mode,
				nil,
			)
			cfg := &RouterConfig{
				AuthMode:     tc.mode,
				AuthHandler:  handler,
				SessionStore: auth.NewSessionStore("test-session-secret-32-bytes-ok!"),
				Log:          slog.New(slog.DiscardHandler),
			}

			_ = buildAuthMiddleware(cfg, mux, newRateLimitStore())

			check := func(method, path string, mustHave bool) {
				t.Helper()

				// mux.Handler returns the matched pattern as its
				// second value; empty string means the default 404
				// handler was selected, i.e. the route is not
				// registered. This avoids actually executing the
				// handler, which could panic on a nil dependency
				// (OIDC provider, etc.) — we only want to know
				// whether the route exists.
				req := httptest.NewRequestWithContext(context.Background(), method, path, nil)

				_, pattern := mux.Handler(req)
				registered := pattern != ""

				switch {
				case mustHave && !registered:
					t.Errorf("%s %s not registered in mode %q", method, path, tc.mode)
				case !mustHave && registered:
					t.Errorf("%s %s unexpectedly registered in mode %q (pattern %q)",
						method, path, tc.mode, pattern)
				}
			}

			check(http.MethodPost, "/auth/logout", tc.expectLogout)
			check(http.MethodGet, "/auth/login", tc.expectLogin)
			check(http.MethodGet, "/auth/kubeconfig", tc.expectKubeconfig)
			check(http.MethodGet, "/auth/token", tc.expectTokenRoutes)
		})
	}
}

// TestTokenUploadIsIPRateLimited asserts that hammering POST
// /auth/token from a single source IP eventually trips the
// pre-auth IP-keyed rate limiter (429). Without this guard a
// loose attacker could spin the SAR probe arbitrarily fast
// against the apiserver, using cozytempl as an amplifier.
func TestTokenUploadIsIPRateLimited(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	handler := auth.NewHandler(
		nil,
		auth.NewSessionStore("test-session-secret-32-bytes-ok!"),
		slog.New(slog.DiscardHandler),
		config.AuthModeToken,
		nil,
	)
	cfg := &RouterConfig{
		AuthMode:     config.AuthModeToken,
		AuthHandler:  handler,
		SessionStore: auth.NewSessionStore("test-session-secret-32-bytes-ok!"),
		Log:          slog.New(slog.DiscardHandler),
	}

	_ = buildAuthMiddleware(cfg, mux, newRateLimitStore())

	// Drive well past burst to guarantee we trip the bucket.
	hit429 := false
	for range rateLimitBurst + 5 {
		req := httptest.NewRequestWithContext(
			context.Background(), http.MethodPost, "/auth/token", nil,
		)
		req.RemoteAddr = "198.51.100.5:4242"

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code == http.StatusTooManyRequests {
			hit429 = true

			break
		}
	}

	if !hit429 {
		t.Errorf("never saw 429 after %d requests — IP rate limiter not engaged", rateLimitBurst+5)
	}
}

// TestKubeconfigUploadIsIPRateLimited mirrors the token-mode test
// for byok. This PR wrapped POST /auth/kubeconfig in the IP rate
// limiter, fixing the pre-existing gap where byok was equally
// vulnerable to SAR-amplification from an unauthenticated client.
func TestKubeconfigUploadIsIPRateLimited(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	handler := auth.NewHandler(
		nil,
		auth.NewSessionStore("test-session-secret-32-bytes-ok!"),
		slog.New(slog.DiscardHandler),
		config.AuthModeBYOK,
		nil,
	)
	cfg := &RouterConfig{
		AuthMode:     config.AuthModeBYOK,
		AuthHandler:  handler,
		SessionStore: auth.NewSessionStore("test-session-secret-32-bytes-ok!"),
		Log:          slog.New(slog.DiscardHandler),
	}

	_ = buildAuthMiddleware(cfg, mux, newRateLimitStore())

	hit429 := false
	for range rateLimitBurst + 5 {
		req := httptest.NewRequestWithContext(
			context.Background(), http.MethodPost, "/auth/kubeconfig", nil,
		)
		req.RemoteAddr = "198.51.100.9:4243"

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code == http.StatusTooManyRequests {
			hit429 = true

			break
		}
	}

	if !hit429 {
		t.Errorf("never saw 429 after %d requests — BYOK IP rate limiter not engaged", rateLimitBurst+5)
	}
}

// TestClientIP_IgnoresXFFWhenUntrusted asserts that a spoofed
// X-Forwarded-For cannot be used to rotate the rate-limit bucket
// away from the real source IP when trustForwardedHeaders is false.
// This is the default, and it protects deployments that expose
// cozytempl without a trusted proxy in front of it.
func TestClientIP_IgnoresXFFWhenUntrusted(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	req.RemoteAddr = "198.51.100.1:4242"

	got := clientIP(req, false)
	if got != "198.51.100.1:4242" {
		t.Errorf("untrusted clientIP = %q, want RemoteAddr (XFF should be ignored)", got)
	}
}

// TestClientIP_HonoursXFFWhenTrusted asserts the opt-in path:
// when trustForwardedHeaders is true (i.e. cozytempl sits behind
// an Ingress / CF Tunnel that strips client-supplied XFF), the
// left-most XFF entry is used as the rate-limit key instead of
// RemoteAddr (which would be the proxy's own address).
func TestClientIP_HonoursXFFWhenTrusted(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	req.RemoteAddr = "198.51.100.1:4242"

	got := clientIP(req, true)
	if got != "1.2.3.4" {
		t.Errorf("trusted clientIP = %q, want leftmost XFF entry 1.2.3.4", got)
	}
}
