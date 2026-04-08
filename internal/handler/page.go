// Package handler provides HTTP handlers that render templ pages.
package handler

import (
	"log/slog"
	"net/http"
	"sort"

	"github.com/a-h/templ"
	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view"
	"github.com/lexfrei/cozytempl/internal/view/layout"
	"github.com/lexfrei/cozytempl/internal/view/page"
)

// PageHandler renders full HTML pages via templ.
type PageHandler struct {
	tenantSvc *k8s.TenantService
	appSvc    *k8s.ApplicationService
	schemaSvc *k8s.SchemaService
	log       *slog.Logger
}

// NewPageHandler creates a new page handler.
func NewPageHandler(
	tenantSvc *k8s.TenantService,
	appSvc *k8s.ApplicationService,
	schemaSvc *k8s.SchemaService,
	log *slog.Logger,
) *PageHandler {
	return &PageHandler{
		tenantSvc: tenantSvc,
		appSvc:    appSvc,
		schemaSvc: schemaSvc,
		log:       log,
	}
}

// Dashboard renders the dashboard page.
func (pgh *PageHandler) Dashboard(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	tenants, err := pgh.tenantSvc.List(req.Context(), usr.Username, usr.Groups)
	if err != nil {
		pgh.log.Error("listing tenants for dashboard", "error", err)

		tenants = []k8s.Tenant{}
	}

	allApps := pgh.aggregateApps(req, usr, tenants)

	readyCount := 0
	failedCount := 0

	for idx := range allApps {
		switch allApps[idx].Status {
		case k8s.AppStatusReady:
			readyCount++
		case k8s.AppStatusFailed:
			failedCount++
		case k8s.AppStatusReconciling, k8s.AppStatusUnknown:
			// not counted in ready/failed
		}
	}

	sort.Slice(allApps, func(i, j int) bool {
		return allApps[i].CreatedAt.After(allApps[j].CreatedAt)
	})

	data := view.DashboardData{
		Tenants:    len(tenants),
		Apps:       len(allApps),
		Ready:      readyCount,
		Failed:     failedCount,
		RecentApps: allApps,
	}

	content := page.Dashboard(data)
	pgh.render(writer, req, usr.Username, tenants, "dashboard", "", content)
}

func (pgh *PageHandler) aggregateApps(
	req *http.Request, usr *auth.UserContext, tenants []k8s.Tenant,
) []k8s.Application {
	var allApps []k8s.Application

	for idx := range tenants {
		apps, err := pgh.appSvc.List(req.Context(), usr.Username, usr.Groups, tenants[idx].Namespace)
		if err != nil {
			pgh.log.Error("listing apps", "tenant", tenants[idx].Namespace, "error", err)

			continue
		}

		allApps = append(allApps, apps...)
	}

	return allApps
}

// render handles full page vs htmx partial rendering.
func (pgh *PageHandler) render(
	writer http.ResponseWriter,
	req *http.Request,
	username string,
	tenants []k8s.Tenant,
	activePage string,
	activeTenant string,
	content templ.Component,
) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	var renderErr error

	if req.Header.Get("HX-Request") != "" {
		renderErr = content.Render(req.Context(), writer)
	} else {
		wrapped := layout.App(username, tenants, activePage, activeTenant)
		renderErr = wrapped.Render(templ.WithChildren(req.Context(), content), writer)
	}

	if renderErr != nil {
		pgh.log.Error("rendering page", "error", renderErr)
	}
}
