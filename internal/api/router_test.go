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
