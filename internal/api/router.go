package api

import (
	"embed"
	"log/slog"
	"net/http"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/handler"
)

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
	mux.Handle("DELETE /", protect(pageMux))

	return mux
}

func registerPageRoutes(pgh *handler.PageHandler) *http.ServeMux {
	pageMux := http.NewServeMux()

	pageMux.HandleFunc("GET /", pgh.Dashboard)
	pageMux.HandleFunc("GET /marketplace", pgh.MarketplacePage)
	pageMux.HandleFunc("GET /tenants", pgh.TenantsPage)
	pageMux.HandleFunc("POST /tenants", pgh.CreateTenant)
	pageMux.HandleFunc("PUT /tenants/{name}", pgh.UpdateTenant)
	pageMux.HandleFunc("DELETE /tenants/{name}", pgh.DeleteTenant)
	pageMux.HandleFunc("GET /tenants/{tenant}", pgh.TenantPage)
	pageMux.HandleFunc("GET /tenants/{tenant}/apps/{name}", pgh.AppDetailPage)

	pageMux.HandleFunc("POST /tenants/{tenant}/apps", pgh.CreateApp)
	pageMux.HandleFunc("DELETE /tenants/{tenant}/apps/{name}", pgh.DeleteApp)

	pageMux.HandleFunc("GET /fragments/app-table", pgh.AppTableFragment)
	pageMux.HandleFunc("GET /fragments/marketplace", pgh.MarketplaceFragment)
	pageMux.HandleFunc("GET /fragments/schema-fields", pgh.SchemaFieldsFragment)

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
