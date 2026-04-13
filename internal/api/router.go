package api

import (
	"context"
	"embed"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	"github.com/lexfrei/cozytempl/internal/handler"
	"github.com/lexfrei/cozytempl/internal/i18n"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// requestTimeout caps how long a single non-SSE handler may spend reading
// from the Kubernetes API and rendering its response. 15 seconds is well
// above normal tail latencies for a listing but short enough that a hung
// k8s watch or a slow impersonation roundtrip cannot starve goroutines.
const requestTimeout = 15 * time.Second

// RouterConfig holds all dependencies for building the HTTP router.
type RouterConfig struct {
	AuthHandler   *auth.Handler
	SessionStore  *auth.SessionStore
	OIDCProvider  *auth.OIDCProvider
	TenantHandler *TenantHandler
	AppHandler    *ApplicationHandler
	SchemaHandler *SchemaHandler
	SSEHandler    *SSEHandler
	PageHandler   *handler.PageHandler
	I18n          *i18n.Bundle
	StaticFS      embed.FS
	Log           *slog.Logger
	AuthMode      config.AuthMode
	DevMode       bool
	DevUsername   string
}

// contentSecurityPolicy is the CSP header applied to every response.
// Everything is loaded from self — htmx is bundled into bundle.js and
// the UI fonts are vendored under /static/fonts, so no third-party
// origins are allowed at all. object-src and frame-src are locked to
// 'none' for defense-in-depth against plugin and iframe injection.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"font-src 'self'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"frame-src 'none'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"

// strictTransportSecurity pins the browser to HTTPS for this host
// (and every subdomain) for two years. Production runs behind a
// TLS-terminating proxy — Cloudflare Tunnel or nginx — that serves
// every user-facing request over HTTPS, so the header is always
// accurate. `preload` is a hint for the HSTS preload list; actual
// submission to hstspreload.org is an operator decision.
const strictTransportSecurity = "max-age=63072000; includeSubDomains; preload"

// withRequestTimeout caps the context of every non-SSE request at
// requestTimeout so that a hung Kubernetes call can't leave a goroutine
// parked forever. SSE connections are long-lived by design and are left
// alone — the stream handler manages its own lifecycle.
func withRequestTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/events") {
			next.ServeHTTP(writer, req)

			return
		}

		ctx, cancel := context.WithTimeout(req.Context(), requestTimeout)
		defer cancel()

		next.ServeHTTP(writer, req.WithContext(ctx))
	})
}

// withSecurityHeaders attaches a set of conservative headers to every
// response. This is a small wrapper around http.Handler so the headers
// apply uniformly across API, page, and static routes without per-handler
// boilerplate.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		headers := writer.Header()
		headers.Set("Content-Security-Policy", contentSecurityPolicy)
		headers.Set("Strict-Transport-Security", strictTransportSecurity)
		headers.Set("X-Content-Type-Options", "nosniff")
		headers.Set("X-Frame-Options", "DENY")
		headers.Set("X-Permitted-Cross-Domain-Policies", "none")
		headers.Set("Referrer-Policy", "same-origin")
		headers.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")

		next.ServeHTTP(writer, req)
	})
}

// Router creates the HTTP route mux with all API and static routes.
// buildAuthMiddleware registers the mode-appropriate auth routes on mux
// and returns a wrapper function the router uses to protect every
// user-facing handler. Broken out of Router so the top-level function
// stays within the project funlen budget.
func buildAuthMiddleware(
	cfg *RouterConfig,
	mux *http.ServeMux,
	rateStore *rateLimitStore,
) func(http.Handler) http.Handler {
	if cfg.AuthMode == config.AuthModeDev {
		return func(next http.Handler) http.Handler {
			// Rate limit runs after auth so it sees the impersonated
			// user identity. In dev mode every request is "dev-admin"
			// so the limit is effectively global — fine for local
			// development but not for multi-user prod.
			return auth.DevAuth(cfg.DevUsername, withRateLimit(rateStore, next))
		}
	}

	// OIDC endpoints are only meaningful when an OIDC provider is
	// actually configured. BYOK and Token modes skip them and rely
	// on their own upload / paste flow instead.
	if cfg.AuthMode == config.AuthModePassthrough || cfg.AuthMode == config.AuthModeImpersonationLegacy {
		mux.HandleFunc("GET /auth/login", cfg.AuthHandler.HandleLogin)
		mux.HandleFunc("GET /auth/callback", cfg.AuthHandler.HandleCallback)
	}

	// BYOK kubeconfig upload form. Placed on the shared mux so it
	// sits OUTSIDE the protect() wrapper — otherwise a user with
	// no stored kubeconfig could never reach the form that lets
	// them upload one (RequireAuth would bounce them right back).
	if cfg.AuthMode == config.AuthModeBYOK && cfg.AuthHandler != nil {
		mux.HandleFunc("GET /auth/kubeconfig", cfg.AuthHandler.HandleKubeconfigUploadForm)
		mux.HandleFunc("POST /auth/kubeconfig", cfg.AuthHandler.HandleKubeconfigUpload)
	}

	// Token mode paste form. Same rationale as the BYOK routes above
	// — the paste form must be reachable without a stored token.
	if cfg.AuthMode == config.AuthModeToken && cfg.AuthHandler != nil {
		mux.HandleFunc("GET /auth/token", cfg.AuthHandler.HandleTokenUploadForm)
		mux.HandleFunc("POST /auth/token", cfg.AuthHandler.HandleTokenUpload)
	}

	if cfg.AuthHandler != nil {
		mux.HandleFunc("POST /auth/logout", cfg.AuthHandler.HandleLogout)
	}

	return func(next http.Handler) http.Handler {
		return auth.RequireAuth(
			cfg.SessionStore,
			cfg.OIDCProvider,
			cfg.Log,
			cfg.AuthMode,
			withRateLimit(rateStore, next),
		)
	}
}

// Router creates the HTTP route mux with all API and static routes.
func Router(cfg *RouterConfig) http.Handler {
	mux := http.NewServeMux()

	// Shared per-user rate limit store. Created here so its
	// janitor goroutine is tied to the router's lifetime; in
	// practice the process never shuts it down because the
	// goroutine exits when the process does.
	rateStore := newRateLimitStore()

	protect := buildAuthMiddleware(cfg, mux, rateStore)

	apiMux := registerAPIRoutes(cfg.TenantHandler, cfg.AppHandler, cfg.SchemaHandler, cfg.SSEHandler)
	pageMux := registerPageRoutes(cfg.PageHandler)

	mux.HandleFunc("GET /healthz", healthHandler)
	mux.HandleFunc("GET /readyz", healthHandler)
	// /metrics is intentionally NOT wrapped in auth middleware:
	// Prometheus scrapers authenticate at the network boundary (pod
	// network policy, service mesh, or an HTTP proxy) rather than
	// via OIDC, and requiring a user session for scraping would
	// break every stock Prometheus config. The endpoint serves only
	// aggregate counts and latency histograms — no tenant data,
	// no secrets, no user identifiers.
	mux.Handle("GET /metrics", metricsHandler())

	mountStaticFiles(mux, cfg.StaticFS)

	mux.Handle("GET /api/", protect(apiMux))
	mux.Handle("POST /api/", protect(apiMux))
	mux.Handle("PUT /api/", protect(apiMux))
	mux.Handle("DELETE /api/", protect(apiMux))

	mux.Handle("GET /", protect(pageMux))
	mux.Handle("POST /", protect(pageMux))
	mux.Handle("PUT /", protect(pageMux))
	mux.Handle("DELETE /", protect(pageMux))

	// Middleware order matters. Outer → inner:
	//   otelhttp:            wraps every request in an
	//                        OpenTelemetry span and honours incoming
	//                        W3C TraceContext. Runs outermost so the
	//                        span covers the full request lifetime,
	//                        including auth, rate limiting and
	//                        handler work. Zero-cost if tracing is
	//                        not configured — the global TracerProvider
	//                        stays at its default no-op.
	//   withRequestID:       mints/reads the correlation ID and
	//                        injects it into the request context.
	//                        MUST be outside withAccessLog so the log
	//                        line carries the ID.
	//   withMetrics:         Prometheus counters/histograms/gauge.
	//                        Wraps the response writer to capture
	//                        the final status, so it must sit
	//                        outside withSecurityHeaders and the
	//                        mux itself.
	//   withAccessLog:       emits one structured log line per
	//                        completed request, including request_id
	//                        pulled from the enriched context.
	//   withSecurityHeaders: sets CSP/HSTS/etc on every response,
	//                        including error pages and 404s.
	//   i18n.Middleware:     resolves the user's locale (cookie →
	//                        Accept-Language → English) and attaches
	//                        a Localizer to the request context.
	//                        Every templ template reads it from
	//                        there. Placed inside the security/log
	//                        layers but outside the timeout wrapper
	//                        so a context cancel still has a Localizer.
	//   withRequestTimeout:  caps handler execution (bypassed for SSE).
	inner := withSecurityHeaders(cfg.I18n.Middleware(withRequestTimeout(mux)))

	return otelhttp.NewHandler(
		withRequestID(
			withMetrics(
				withAccessLog(cfg.Log, inner),
			),
		),
		"cozytempl",
	)
}

func registerPageRoutes(pgh *handler.PageHandler) *http.ServeMux {
	pageMux := http.NewServeMux()

	// Note the {$} on the root route: in Go 1.22's ServeMux, a bare
	// "GET /" pattern matches EVERY path not otherwise claimed by a
	// more specific handler. That turned /nonexistent into a
	// silently-rendered Dashboard page which was a real bug — the
	// user had no idea they'd typed a wrong URL. {$} constrains the
	// match to exactly "/" so unknown paths fall through to the
	// NotFoundPage handler below.
	pageMux.HandleFunc("GET /{$}", pgh.Dashboard)
	pageMux.HandleFunc("GET /marketplace", pgh.MarketplacePage)
	pageMux.HandleFunc("GET /profile", pgh.ProfilePage)
	pageMux.HandleFunc("GET /tenants", pgh.TenantsPage)
	pageMux.HandleFunc("POST /tenants", pgh.CreateTenant)
	pageMux.HandleFunc("PUT /tenants/{name}", pgh.UpdateTenant)
	pageMux.HandleFunc("DELETE /tenants/{name}", pgh.DeleteTenant)
	pageMux.HandleFunc("GET /tenants/{tenant}", pgh.TenantPage)
	pageMux.HandleFunc("GET /tenants/{tenant}/apps/{name}", pgh.AppDetailPage)

	pageMux.HandleFunc("POST /tenants/{tenant}/apps", pgh.CreateApp)
	pageMux.HandleFunc("PUT /tenants/{tenant}/apps/{name}", pgh.UpdateApp)
	pageMux.HandleFunc("DELETE /tenants/{tenant}/apps/{name}", pgh.DeleteApp)

	pageMux.HandleFunc("GET /fragments/app-table", pgh.AppTableFragment)
	pageMux.HandleFunc("GET /fragments/marketplace", pgh.MarketplaceFragment)
	pageMux.HandleFunc("GET /fragments/schema-fields", pgh.SchemaFieldsFragment)
	pageMux.HandleFunc("GET /fragments/tenant-edit", pgh.TenantEditFragment)
	pageMux.HandleFunc("GET /fragments/app-edit", pgh.AppEditFragment)
	pageMux.HandleFunc("GET /fragments/secrets/reveal", pgh.SecretRevealFragment)

	// Language switcher. POST because it writes a cookie; the
	// handler redirects back to the Referer (or dashboard) and
	// the next render picks up the new locale via i18n middleware.
	pageMux.HandleFunc("POST /lang", pgh.SetLanguage)

	// Catch-all 404 for unknown GET paths. Placed last so it only
	// matches when nothing above did. The handler renders the
	// branded error page with the layout wrapper.
	pageMux.HandleFunc("GET /", pgh.NotFoundPage)

	return pageMux
}

func registerAPIRoutes(
	tenantHandler *TenantHandler,
	appHandler *ApplicationHandler,
	schemaHandler *SchemaHandler,
	sseHandler *SSEHandler,
) *http.ServeMux {
	apiMux := http.NewServeMux()

	apiMux.HandleFunc("GET /api/tenants", tenantHandler.List)
	apiMux.HandleFunc("GET /api/tenants/{name}", tenantHandler.Get)
	apiMux.HandleFunc("POST /api/tenants", tenantHandler.Create)
	apiMux.HandleFunc("DELETE /api/tenants/{name}", tenantHandler.Delete)

	apiMux.HandleFunc("GET /api/tenants/{tenant}/apps", appHandler.List)
	apiMux.HandleFunc("GET /api/tenants/{tenant}/apps/{name}", appHandler.Get)
	apiMux.HandleFunc("POST /api/tenants/{tenant}/apps", appHandler.Create)
	apiMux.HandleFunc("PUT /api/tenants/{tenant}/apps/{name}", appHandler.Update)
	apiMux.HandleFunc("DELETE /api/tenants/{tenant}/apps/{name}", appHandler.Delete)

	apiMux.HandleFunc("GET /api/schemas", schemaHandler.List)
	apiMux.HandleFunc("GET /api/schemas/{kind}", schemaHandler.Get)

	apiMux.HandleFunc("GET /api/events", sseHandler.Stream)

	return apiMux
}

func healthHandler(writer http.ResponseWriter, _ *http.Request) {
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("ok"))
}

func mountStaticFiles(mux *http.ServeMux, staticFS embed.FS) {
	fileServer := http.StripPrefix("/static/", http.FileServer(http.FS(staticFS)))

	// Add no-cache headers so Cloudflare and browsers don't serve stale bundles.
	// For production we should use versioned filenames + long cache, but while
	// iterating locally this is the simpler and safer default.
	mux.Handle("GET /static/", http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		writer.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		writer.Header().Set("Pragma", "no-cache")
		writer.Header().Set("Expires", "0")
		fileServer.ServeHTTP(writer, req)
	}))
}
