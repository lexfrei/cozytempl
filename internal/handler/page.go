// Package handler provides HTTP handlers that render templ pages.
package handler

import (
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/a-h/templ"
	"github.com/lexfrei/cozytempl/internal/audit"
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
	eventSvc  *k8s.EventService
	logSvc    *k8s.LogService
	log       *slog.Logger
	// auditLog receives structured events for every mutation and
	// secret-view action the handler performs. nil is not allowed;
	// NewPageHandler substitutes a NopLogger if the caller forgets
	// so the rest of the code can deref without a guard.
	auditLog audit.Logger
	// devMode drives the dev-mode banner at the top of every page.
	// Set at construction time from config so the layout template
	// does not need to reach into global state.
	devMode bool
}

// PageHandlerDeps groups the constructor arguments. Grew past the
// positional-arg sweet spot once auditLog joined the party; a
// struct keeps the call site readable and future additions
// non-breaking.
type PageHandlerDeps struct {
	TenantSvc *k8s.TenantService
	AppSvc    *k8s.ApplicationService
	SchemaSvc *k8s.SchemaService
	UsageSvc  *k8s.UsageService
	EventSvc  *k8s.EventService
	LogSvc    *k8s.LogService
	Audit     audit.Logger
	Log       *slog.Logger
	DevMode   bool
}

// NewPageHandler creates a new page handler.
//
//nolint:gocritic // hugeParam: PageHandlerDeps is a one-shot constructor arg; copying once at startup is fine
func NewPageHandler(deps PageHandlerDeps) *PageHandler {
	auditLog := deps.Audit
	if auditLog == nil {
		auditLog = audit.NopLogger{}
	}

	return &PageHandler{
		tenantSvc: deps.TenantSvc,
		appSvc:    deps.AppSvc,
		schemaSvc: deps.SchemaSvc,
		usageSvc:  deps.UsageSvc,
		eventSvc:  deps.EventSvc,
		logSvc:    deps.LogSvc,
		auditLog:  auditLog,
		devMode:   deps.DevMode,
		log:       deps.Log,
	}
}

// Dashboard renders the dashboard page.
func (pgh *PageHandler) Dashboard(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
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
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenantNS := req.PathValue("tenant")

	tenants, _ := pgh.tenantSvc.List(req.Context(), usr.Username, usr.Groups)

	tenant, err := pgh.tenantSvc.Get(req.Context(), usr.Username, usr.Groups, tenantNS)
	if err != nil {
		pgh.log.Error("getting tenant", "tenant", tenantNS, "error", err)
		pgh.renderError(writer, req, usr.Username, tenants, http.StatusNotFound,
			"Tenant not found",
			"The tenant '"+tenantNS+"' either does not exist or you do not have permission to view it.")

		return
	}

	data := pgh.buildTenantPageData(req, usr, tenantNS, tenant, tenants)

	content := page.Tenant(data)
	pgh.render(writer, req, usr.Username, tenants, "tenant", tenantNS, content)
}

// AppDetailPage renders the application detail page.
func (pgh *PageHandler) AppDetailPage(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
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
		pgh.renderError(writer, req, usr.Username, tenants, http.StatusNotFound,
			"Application not found",
			"The application '"+appName+"' in tenant '"+tenantNS+
				"' either does not exist or you do not have permission to view it.")

		return
	}

	// Audit the connection-tab view — the rendered HTML embeds
	// database passwords and API tokens pulled from tenant secrets.
	// "Who looked at the postgres password at 14:02" is the single
	// most common SOC2 / compliance question and the audit trail
	// needs to answer it.
	if tab == "connection" {
		pgh.recordAudit(req, usr, audit.ActionConnectionView, appName, tenantNS,
			audit.OutcomeSuccess, map[string]any{"kind": app.Kind})
	}

	data := pgh.buildAppDetailData(req, usr, tenantNS, appName, app, tab)

	content := page.AppDetail(data)
	pgh.render(writer, req, usr.Username, tenants, "appDetail", tenantNS, content)
}

// appEventLimit caps the number of events shown on a single tab.
const appEventLimit = 50

// appLogTailLines caps the log tail fetched for the Logs tab.
const appLogTailLines = 500

// tenantEventLimit caps the number of events shown on the tenant page
// activity card. A smaller number than appEventLimit keeps the page
// scannable rather than burying the application table.
const tenantEventLimit = 15

// ProfilePage renders the current user's identity details.
func (pgh *PageHandler) ProfilePage(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenants, _ := pgh.tenantSvc.List(req.Context(), usr.Username, usr.Groups)

	content := page.Profile(usr)
	pgh.render(writer, req, usr.Username, tenants, "profile", "", content)
}

// MarketplacePage renders the marketplace catalog.
func (pgh *PageHandler) MarketplacePage(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
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

// NotFoundPage is the catch-all 404 handler registered for any
// GET path not claimed by a more specific route. Without this,
// Go 1.22's ServeMux would either silently render the Dashboard
// (because "GET /" is a prefix match) or return a plain-text
// "404 page not found" body — neither is a good user experience.
func (pgh *PageHandler) NotFoundPage(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenants, _ := pgh.tenantSvc.List(req.Context(), usr.Username, usr.Groups)
	pgh.renderError(writer, req, usr.Username, tenants, http.StatusNotFound,
		"Page not found",
		"The page '"+req.URL.Path+"' does not exist. Use the navigation on the left or head back to the dashboard.")
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

// buildAppDetailData composes the AppDetailData for a single request.
// Splits the per-tab fetches (events, logs) out of AppDetailPage so
// the outer handler stays short enough to satisfy the funlen linter
// and so tab-specific logic stays in one place. Application is passed
// by pointer because k8s.Application is ~150 bytes and the linter
// flags the by-value copy.
func (pgh *PageHandler) buildAppDetailData(
	req *http.Request, usr *auth.UserContext, tenantNS, appName string, app *k8s.Application, tab string,
) view.AppDetailData {
	data := view.AppDetailData{
		App:    *app,
		Tenant: tenantNS,
		Tab:    tab,
	}

	switch tab {
	case "events":
		events, err := pgh.eventSvc.ListForObject(
			req.Context(), usr.Username, usr.Groups, tenantNS, appName, appEventLimit,
		)
		if err != nil {
			pgh.log.Debug("listing app events", "tenant", tenantNS, "app", appName, "error", err)
		}

		data.Events = events
	case "logs":
		data.Pods, data.SelectedPod, data.SelectedContainer, data.LogTail, data.LogError = pgh.fetchAppLogs(req, usr, tenantNS, appName)
	}

	return data
}

// fetchAppLogs pulls the pod list for the app, picks the selected pod
// and container from the query string (or the first of each if the
// client did not specify), and returns the tail of that container's
// log. Errors on the log fetch are returned as a message string for
// the template to render inline — we do not surface them as toast /
// 500, because "pod is terminating" or "container has no logs yet"
// are perfectly normal states.
//
//nolint:gocritic // unnamedResult conflicts with nonamedreturns linter
func (pgh *PageHandler) fetchAppLogs(
	req *http.Request, usr *auth.UserContext, tenantNS, appName string,
) ([]k8s.PodInfo, string, string, string, string) {
	pods, listErr := pgh.logSvc.ListPodsForApp(req.Context(), usr.Username, usr.Groups, tenantNS, appName)
	if listErr != nil {
		pgh.log.Debug("listing pods for logs tab", "tenant", tenantNS, "app", appName, "error", listErr)

		return nil, "", "", "", "Failed to list pods: " + listErr.Error()
	}

	if len(pods) == 0 {
		return pods, "", "", "", "No pods found for this application."
	}

	selectedPod := req.URL.Query().Get("pod")
	if selectedPod == "" {
		selectedPod = pods[0].Name
	}

	var chosen *k8s.PodInfo

	for idx := range pods {
		if pods[idx].Name == selectedPod {
			chosen = &pods[idx]

			break
		}
	}

	if chosen == nil {
		return pods, selectedPod, "", "", "Selected pod not found in this application."
	}

	selectedContainer := req.URL.Query().Get("container")
	if selectedContainer == "" && len(chosen.Containers) > 0 {
		selectedContainer = chosen.Containers[0]
	}

	tail, tailErr := pgh.logSvc.TailLogs(req.Context(), usr.Username, usr.Groups, tenantNS,
		selectedPod, selectedContainer, appLogTailLines)
	if tailErr != nil {
		pgh.log.Debug("tailing logs", "pod", selectedPod, "container", selectedContainer, "error", tailErr)

		return pods, selectedPod, selectedContainer, "", "Failed to read logs: " + tailErr.Error()
	}

	return pods, selectedPod, selectedContainer, tail, ""
}

// buildTenantPageData gathers every per-tenant collection (apps,
// schemas, children, events, usage) in one place so TenantPage stays
// under the function-length linter limit and the read path is easy to
// follow. Errors on the optional collections are logged and dropped —
// the UI renders an empty section instead of failing the whole page.
// Tenant is passed by pointer because k8s.Tenant is ~150 bytes of
// metadata and the linter flags the by-value copy.
func (pgh *PageHandler) buildTenantPageData(
	req *http.Request, usr *auth.UserContext, tenantNS string, tenant *k8s.Tenant, allTenants []k8s.Tenant,
) view.TenantPageData {
	appList, _ := pgh.appSvc.List(req.Context(), usr.Username, usr.Groups, tenantNS)
	schemas, _ := pgh.schemaSvc.List(req.Context(), usr.Username, usr.Groups)

	// Direct children of this tenant, scoped to what the user can see:
	// reuse the already-listed tenants slice so the child list inherits
	// the caller's RBAC view without a second impersonated call.
	var children []k8s.Tenant

	for idx := range allTenants {
		if allTenants[idx].Parent == tenantNS {
			children = append(children, allTenants[idx])
		}
	}

	events, evtErr := pgh.eventSvc.ListInNamespace(
		req.Context(), usr.Username, usr.Groups, tenantNS, tenantEventLimit,
	)
	if evtErr != nil {
		pgh.log.Debug("listing tenant events", "tenant", tenantNS, "error", evtErr)
	}

	usage, usageErr := pgh.usageSvc.Collect(req.Context(), usr.Username, usr.Groups, tenantNS)
	if usageErr != nil {
		pgh.log.Debug("collecting tenant usage", "tenant", tenantNS, "error", usageErr)
	}

	quotas, quotaErr := pgh.usageSvc.ListQuotas(req.Context(), usr.Username, usr.Groups, tenantNS)
	if quotaErr != nil {
		pgh.log.Debug("listing tenant quotas", "tenant", tenantNS, "error", quotaErr)
	}

	return view.TenantPageData{
		Tenant:        *tenant,
		Usage:         usage,
		Quotas:        quotas,
		Children:      children,
		Apps:          appList.Items,
		AppsTruncated: appList.Truncated,
		Schemas:       schemas,
		Events:        events,
		Query:         req.URL.Query().Get("q"),
		KindFilter:    req.URL.Query().Get("kind"),
		SortBy:        req.URL.Query().Get("sort"),
	}
}

func (pgh *PageHandler) aggregateApps(
	req *http.Request, usr *auth.UserContext, tenants []k8s.Tenant,
) []k8s.Application {
	var allApps []k8s.Application

	for idx := range tenants {
		appList, err := pgh.appSvc.List(req.Context(), usr.Username, usr.Groups, tenants[idx].Namespace)
		if err != nil {
			pgh.log.Error("listing apps", "tenant", tenants[idx].Namespace, "error", err)

			continue
		}

		allApps = append(allApps, appList.Items...)
	}

	return allApps
}

// recordAudit is the ergonomic shorthand handlers use to emit an
// audit event. It pulls the correlation ID out of the request
// context, fills in actor/groups from the auth middleware, and
// delegates to the wired Logger. Keeping this in one place means
// adding a new field to Event only requires editing this helper
// instead of every call site.
func (pgh *PageHandler) recordAudit(
	req *http.Request,
	usr *auth.UserContext,
	action audit.Action,
	resource, tenant string,
	outcome audit.Outcome,
	details map[string]any,
) {
	pgh.auditLog.Record(req.Context(), &audit.Event{
		RequestID: audit.RequestIDFromContext(req.Context()),
		Actor:     usr.Username,
		Groups:    usr.Groups,
		Action:    action,
		Resource:  resource,
		Tenant:    tenant,
		Outcome:   outcome,
		Details:   details,
	})
}

// renderError writes a branded error page instead of plain-text
// http.Error. Htmx navigation to a missing resource (typed URL,
// stale link, deleted app) used to land on a text/plain "tenant not
// found" body that blew through the layout; now it swaps the error
// template into #main-content and keeps the header/sidebar intact.
// Non-htmx direct hits get the full layout wrap, same as render().
func (pgh *PageHandler) renderError(
	writer http.ResponseWriter,
	req *http.Request,
	username string,
	tenants []k8s.Tenant,
	status int,
	title string,
	detail string,
) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)

	statusText := http.StatusText(status)
	content := page.ErrorPage(status, statusText, title, detail, "Back to dashboard", "/")

	var renderErr error

	if req.Header.Get("Hx-Request") != "" {
		renderErr = content.Render(req.Context(), writer)
	} else {
		wrapped := layout.App(layout.AppProps{
			Username:     username,
			Tenants:      tenants,
			ActivePage:   "",
			ActiveTenant: "",
			DevMode:      pgh.devMode,
		})
		renderErr = wrapped.Render(templ.WithChildren(req.Context(), content), writer)
	}

	if renderErr != nil {
		pgh.log.Error("rendering error page", "error", renderErr)
	}
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
		wrapped := layout.App(layout.AppProps{
			Username:     username,
			Tenants:      tenants,
			ActivePage:   activePage,
			ActiveTenant: activeTenant,
			DevMode:      pgh.devMode,
		})
		renderErr = wrapped.Render(templ.WithChildren(req.Context(), content), writer)
	}

	if renderErr != nil {
		pgh.log.Error("rendering page", "error", renderErr)
	}
}
