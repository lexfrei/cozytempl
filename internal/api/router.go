package api

import (
	"context"
	"embed"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/handler"
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
	TenantHandler *TenantHandler
	AppHandler    *ApplicationHandler
	SchemaHandler *SchemaHandler
	SSEHandler    *SSEHandler
	PageHandler   *handler.PageHandler
	StaticFS      embed.FS
	Log           *slog.Logger
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
func Router(cfg *RouterConfig) http.Handler {
	mux := http.NewServeMux()

	var protect func(http.Handler) http.Handler

	if cfg.DevMode {
		protect = func(next http.Handler) http.Handler {
			return auth.DevAuth(cfg.DevUsername, next)
		}
	} else {
		mux.HandleFunc("GET /auth/login", cfg.AuthHandler.HandleLogin)
		mux.HandleFunc("GET /auth/callback", cfg.AuthHandler.HandleCallback)
		mux.HandleFunc("POST /auth/logout", cfg.AuthHandler.HandleLogout)

		protect = func(next http.Handler) http.Handler {
			return auth.RequireAuth(cfg.SessionStore, next)
		}
	}

	apiMux := registerAPIRoutes(cfg.TenantHandler, cfg.AppHandler, cfg.SchemaHandler, cfg.SSEHandler)
	pageMux := registerPageRoutes(cfg.PageHandler)

	mux.HandleFunc("GET /healthz", healthHandler)
	mux.HandleFunc("GET /readyz", healthHandler)

	mountStaticFiles(mux, cfg.StaticFS)

	mux.Handle("GET /api/", protect(apiMux))
	mux.Handle("POST /api/", protect(apiMux))
	mux.Handle("PUT /api/", protect(apiMux))
	mux.Handle("DELETE /api/", protect(apiMux))

	mux.Handle("GET /", protect(pageMux))
	mux.Handle("POST /", protect(pageMux))
	mux.Handle("PUT /", protect(pageMux))
	mux.Handle("DELETE /", protect(pageMux))

	return withSecurityHeaders(withRequestTimeout(mux))
}

func registerPageRoutes(pgh *handler.PageHandler) *http.ServeMux {
	pageMux := http.NewServeMux()

	pageMux.HandleFunc("GET /", pgh.Dashboard)
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
