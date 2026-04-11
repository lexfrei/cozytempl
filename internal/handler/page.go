// Package handler provides HTTP handlers that render templ pages.
package handler

import (
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/a-h/templ"
	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view"
	"github.com/lexfrei/cozytempl/internal/view/layout"
	"github.com/lexfrei/cozytempl/internal/view/page"
	"github.com/lexfrei/cozytempl/internal/view/partial"
)

// PageHandler renders full HTML pages via templ.
type PageHandler struct {
	tenantSvc *k8s.TenantService
	appSvc    *k8s.ApplicationService
	schemaSvc *k8s.SchemaService
	usageSvc  *k8s.UsageService
	log       *slog.Logger
}

// NewPageHandler creates a new page handler.
func NewPageHandler(
	tenantSvc *k8s.TenantService,
	appSvc *k8s.ApplicationService,
	schemaSvc *k8s.SchemaService,
	usageSvc *k8s.UsageService,
	log *slog.Logger,
) *PageHandler {
	return &PageHandler{
		tenantSvc: tenantSvc,
		appSvc:    appSvc,
		schemaSvc: schemaSvc,
		usageSvc:  usageSvc,
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

// TenantPage renders the tenant detail with app list.
func (pgh *PageHandler) TenantPage(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	tenantNS := req.PathValue("tenant")

	tenants, _ := pgh.tenantSvc.List(req.Context(), usr.Username, usr.Groups)

	tenant, err := pgh.tenantSvc.Get(req.Context(), usr.Username, usr.Groups, tenantNS)
	if err != nil {
		pgh.log.Error("getting tenant", "tenant", tenantNS, "error", err)
		http.Error(writer, "tenant not found", http.StatusNotFound)

		return
	}

	apps, _ := pgh.appSvc.List(req.Context(), usr.Username, usr.Groups, tenantNS)
	schemas, _ := pgh.schemaSvc.List(req.Context(), usr.Username, usr.Groups)

	// Direct children of this tenant, scoped to what the user can see: we
	// reuse the already-listed tenants slice so the child list inherits the
	// caller's RBAC view without a second impersonated call.
	var children []k8s.Tenant

	for idx := range tenants {
		if tenants[idx].Parent == tenantNS {
			children = append(children, tenants[idx])
		}
	}

	data := view.TenantPageData{
		Tenant:     *tenant,
		Children:   children,
		Apps:       apps,
		Schemas:    schemas,
		Query:      req.URL.Query().Get("q"),
		KindFilter: req.URL.Query().Get("kind"),
		SortBy:     req.URL.Query().Get("sort"),
	}

	content := page.Tenant(data)
	pgh.render(writer, req, usr.Username, tenants, "tenant", tenantNS, content)
}

// AppDetailPage renders the application detail page.
func (pgh *PageHandler) AppDetailPage(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	// The connection tab embeds database passwords and API tokens pulled
	// from tenant secrets. Keep the rendered page off every intermediate
	// cache (Cloudflare, browsers, corporate proxies) so a shared tab
	// cannot replay cached credentials later.
	writer.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Set("Expires", "0")

	tenantNS := req.PathValue("tenant")
	appName := req.PathValue("name")
	tab := req.URL.Query().Get("tab")

	if tab == "" {
		tab = "overview"
	}

	tenants, _ := pgh.tenantSvc.List(req.Context(), usr.Username, usr.Groups)

	app, err := pgh.appSvc.Get(req.Context(), usr.Username, usr.Groups, tenantNS, appName)
	if err != nil {
		pgh.log.Error("getting app", "tenant", tenantNS, "app", appName, "error", err)
		http.Error(writer, "application not found", http.StatusNotFound)

		return
	}

	data := view.AppDetailData{
		App:    *app,
		Tenant: tenantNS,
		Tab:    tab,
	}

	content := page.AppDetail(data)
	pgh.render(writer, req, usr.Username, tenants, "appDetail", tenantNS, content)
}

// ProfilePage renders the current user's identity details.
func (pgh *PageHandler) ProfilePage(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	tenants, _ := pgh.tenantSvc.List(req.Context(), usr.Username, usr.Groups)

	content := page.Profile(usr)
	pgh.render(writer, req, usr.Username, tenants, "profile", "", content)
}

// MarketplacePage renders the marketplace catalog.
func (pgh *PageHandler) MarketplacePage(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	tenants, _ := pgh.tenantSvc.List(req.Context(), usr.Username, usr.Groups)
	schemas, _ := pgh.schemaSvc.List(req.Context(), usr.Username, usr.Groups)

	categoryFilter := req.URL.Query().Get("category")
	tagFilter := req.URL.Query().Get("tag")
	query := req.URL.Query().Get("q")

	data := buildMarketplaceData(schemas, query, categoryFilter, tagFilter)

	content := page.Marketplace(data)
	pgh.render(writer, req, usr.Username, tenants, "marketplace", "", content)
}

func buildMarketplaceData(schemas []k8s.AppSchema, query, categoryFilter, tagFilter string) view.MarketplaceData {
	filtered := filterSchemas(schemas, query, categoryFilter, tagFilter)

	allTags := collectTags(schemas)
	categories := groupByCategory(filtered)

	return view.MarketplaceData{
		Schemas:        filtered,
		Categories:     categories,
		AllTags:        allTags,
		Query:          query,
		CategoryFilter: categoryFilter,
		TagFilter:      tagFilter,
	}
}

func filterSchemas(schemas []k8s.AppSchema, query, category, tag string) []k8s.AppSchema {
	var result []k8s.AppSchema

	for idx := range schemas {
		if category != "" && schemas[idx].Category != category {
			continue
		}

		if tag != "" && !slices.Contains(schemas[idx].Tags, tag) {
			continue
		}

		if query != "" && !matchesQuery(&schemas[idx], query) {
			continue
		}

		result = append(result, schemas[idx])
	}

	return result
}

func matchesQuery(schema *k8s.AppSchema, query string) bool {
	q := strings.ToLower(query)

	return strings.Contains(strings.ToLower(schema.Kind), q) ||
		strings.Contains(strings.ToLower(schema.DisplayName), q) ||
		strings.Contains(strings.ToLower(schema.Description), q)
}

func collectTags(schemas []k8s.AppSchema) []string {
	seen := map[string]bool{}

	var tags []string

	for idx := range schemas {
		for _, tag := range schemas[idx].Tags {
			if !seen[tag] {
				seen[tag] = true
				tags = append(tags, tag)
			}
		}
	}

	sort.Strings(tags)

	return tags
}

func groupByCategory(schemas []k8s.AppSchema) []view.CategoryGroup {
	catMap := map[string][]k8s.AppSchema{}

	var order []string

	for idx := range schemas {
		cat := schemas[idx].Category
		if cat == "" {
			cat = "Other"
		}

		if _, exists := catMap[cat]; !exists {
			order = append(order, cat)
		}

		catMap[cat] = append(catMap[cat], schemas[idx])
	}

	sort.Strings(order)

	groups := make([]view.CategoryGroup, 0, len(order))

	for _, cat := range order {
		groups = append(groups, view.CategoryGroup{
			Name:    cat,
			Schemas: catMap[cat],
		})
	}

	return groups
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

	if req.Header.Get("Hx-Request") != "" {
		// Render page content + OOB sidebar swap to update active state
		renderErr = content.Render(req.Context(), writer)
		if renderErr == nil {
			renderErr = partial.SidebarOOB(tenants, activePage, activeTenant).Render(req.Context(), writer)
		}
	} else {
		wrapped := layout.App(username, tenants, activePage, activeTenant)
		renderErr = wrapped.Render(templ.WithChildren(req.Context(), content), writer)
	}

	if renderErr != nil {
		pgh.log.Error("rendering page", "error", renderErr)
	}
}
