package api

import (
	"embed"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/lexfrei/cozytempl/internal/auth"
)

// Router creates the HTTP route mux with all API and static routes.
func Router(
	authHandler *auth.Handler,
	sessionStore *auth.SessionStore,
	tenantHandler *TenantHandler,
	appHandler *ApplicationHandler,
	schemaHandler *SchemaHandler,
	sseHandler *SSEHandler,
	staticFS embed.FS,
	shellHandler http.HandlerFunc,
	log *slog.Logger,
) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /auth/login", authHandler.HandleLogin)
	mux.HandleFunc("GET /auth/callback", authHandler.HandleCallback)
	mux.HandleFunc("POST /auth/logout", authHandler.HandleLogout)

	apiMux := registerAPIRoutes(tenantHandler, appHandler, schemaHandler, sseHandler)

	mux.HandleFunc("GET /healthz", healthHandler)
	mux.HandleFunc("GET /readyz", healthHandler)

	mountStaticFiles(mux, staticFS, log)

	mux.Handle("/api/", auth.RequireAuth(sessionStore, apiMux))
	mux.Handle("GET /", auth.RequireAuth(sessionStore, shellHandler))

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

func mountStaticFiles(mux *http.ServeMux, staticFS embed.FS, log *slog.Logger) {
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Error("failed to create static sub-fs", "error", err)

		return
	}

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))
}
