package api

import (
	"embed"
	"log/slog"
	"net/http"

	"github.com/lexfrei/cozytempl/internal/auth"
)

// RouterConfig holds all dependencies for building the HTTP router.
type RouterConfig struct {
	AuthHandler   *auth.Handler
	SessionStore  *auth.SessionStore
	TenantHandler *TenantHandler
	AppHandler    *ApplicationHandler
	SchemaHandler *SchemaHandler
	SSEHandler    *SSEHandler
	StaticFS      embed.FS
	ShellHandler  http.HandlerFunc
	Log           *slog.Logger
	DevMode       bool
	DevUsername   string
}

// Router creates the HTTP route mux with all API and static routes.
func Router(cfg *RouterConfig) http.Handler {
	mux := http.NewServeMux()

	// Auth middleware — either OIDC session or dev passthrough
	var protectAPI func(http.Handler) http.Handler

	var protectShell func(http.Handler) http.Handler

	if cfg.DevMode {
		devWrap := func(next http.Handler) http.Handler {
			return auth.DevAuth(cfg.DevUsername, next)
		}
		protectAPI = devWrap
		protectShell = devWrap
	} else {
		mux.HandleFunc("GET /auth/login", cfg.AuthHandler.HandleLogin)
		mux.HandleFunc("GET /auth/callback", cfg.AuthHandler.HandleCallback)
		mux.HandleFunc("POST /auth/logout", cfg.AuthHandler.HandleLogout)

		protectAPI = func(next http.Handler) http.Handler {
			return auth.RequireAuth(cfg.SessionStore, next)
		}
		protectShell = protectAPI
	}

	apiMux := registerAPIRoutes(cfg.TenantHandler, cfg.AppHandler, cfg.SchemaHandler, cfg.SSEHandler)

	mux.HandleFunc("GET /healthz", healthHandler)
	mux.HandleFunc("GET /readyz", healthHandler)

	mountStaticFiles(mux, cfg.StaticFS, cfg.Log)

	mux.Handle("GET /", protectShell(cfg.ShellHandler))
	mux.Handle("GET /api/", protectAPI(apiMux))
	mux.Handle("POST /api/", protectAPI(apiMux))
	mux.Handle("PUT /api/", protectAPI(apiMux))
	mux.Handle("DELETE /api/", protectAPI(apiMux))

	return mux
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

func mountStaticFiles(mux *http.ServeMux, staticFS embed.FS, _ *slog.Logger) {
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
}
