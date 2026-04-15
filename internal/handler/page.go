// Package handler provides HTTP handlers that render templ pages.
package handler

import (
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/a-h/templ"
	"k8s.io/client-go/rest"

	"github.com/lexfrei/cozytempl/internal/actions"
	"github.com/lexfrei/cozytempl/internal/audit"
	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	"github.com/lexfrei/cozytempl/internal/i18n"
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
	// baseCfg is the in-cluster / kubeconfig rest.Config that backs
	// every k8s service above. Stored on the handler so
	// InvokeAction can build a user-credentialed derivative without
	// having to reach through ApplicationService internals.
	baseCfg *rest.Config
	log     *slog.Logger
	// auditLog receives structured events for every mutation and
	// secret-view action the handler performs. nil is not allowed;
	// NewPageHandler substitutes a NopLogger if the caller forgets
	// so the rest of the code can deref without a guard.
	auditLog audit.Logger
	// i18nBundle is the shared translation bundle. Handlers pull a
	// request-scoped Localizer out of the context and pass it into
	// templ templates so every user-visible string honours the
	// caller's locale.
	i18nBundle *i18n.Bundle
	// devMode drives the dev-mode banner at the top of every page.
	// Set at construction time from config so the layout template
	// does not need to reach into global state.
	devMode bool
	// authMode is the mode cozytempl is configured in. Injected
	// into the profile page and into audit events as the
	// fallback when RequireAuth did not run (tests, dev mode).
	authMode config.AuthMode
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
	// BaseCfg is passed through to the handler for callers (like
	// the per-resource action registry) that need to build a fresh
	// user-credentialed rest.Config outside the concrete service
	// wrappers above.
	BaseCfg  *rest.Config
	Audit    audit.Logger
	I18n     *i18n.Bundle
	Log      *slog.Logger
	AuthMode config.AuthMode
	DevMode  bool
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
		tenantSvc:  deps.TenantSvc,
		appSvc:     deps.AppSvc,
		schemaSvc:  deps.SchemaSvc,
		usageSvc:   deps.UsageSvc,
		eventSvc:   deps.EventSvc,
		logSvc:     deps.LogSvc,
		baseCfg:    deps.BaseCfg,
		auditLog:   auditLog,
		i18nBundle: deps.I18n,
		devMode:    deps.DevMode,
		authMode:   deps.AuthMode,
		log:        deps.Log,
	}
}

// Dashboard renders the dashboard page.
func (pgh *PageHandler) Dashboard(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenants, err := pgh.tenantSvc.List(req.Context(), usr)
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

	tenants, _ := pgh.tenantSvc.List(req.Context(), usr)

	tenant, err := pgh.tenantSvc.Get(req.Context(), usr, tenantNS)
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

	tenants, _ := pgh.tenantSvc.List(req.Context(), usr)

	app, err := pgh.appSvc.Get(req.Context(), usr, tenantNS, appName)
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

	tenants, _ := pgh.tenantSvc.List(req.Context(), usr)

	mode := auth.ModeFromContext(req.Context())
	if mode == "" {
		// Dev mode and tests bypass RequireAuth, so the context
		// may not carry the mode marker. Fall back to what the
		// handler itself was configured with.
		mode = pgh.authMode
	}

	content := page.Profile(usr, mode)
	pgh.render(writer, req, usr.Username, tenants, "profile", "", content)
}

// MarketplaceLaunchPage is the smart-router behind a marketplace card
// click. It exists so the user no longer has to detour through the
// full /tenants?kind=X picker page when there's only one tenant they
// could possibly create the chosen kind in.
//
// Behaviour (kind comes from the marketplace card click):
//
//   - Zero tenants visible to the user: 4xx error toast — there's no
//     valid destination, the catch-all redirect would loop.
//   - Exactly one tenant: HX-Redirect (htmx) or 303 (non-htmx) straight
//     to /tenants/{ns}?createKind=X so the create-app modal opens with
//     no extra clicks.
//   - Two or more tenants: an HTML modal fragment listing the tenants
//     so the user picks one. The fragment is swapped into
//     #marketplace-modal-slot on the marketplace page.
//
// Unknown / missing kind collapses to "" via the same selectKnownKind
// guard used by extractCreateKindFromQuery — the picker still renders
// in that case (with no preselection) so the user isn't dead-ended.
func (pgh *PageHandler) MarketplaceLaunchPage(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenants, err := pgh.tenantSvc.List(req.Context(), usr)
	if err != nil {
		pgh.log.Error("listing tenants for marketplace launch", "error", err)
		http.Error(writer, `{"error":"could not list tenants"}`, http.StatusInternalServerError)

		return
	}

	if len(tenants) == 0 {
		pgh.renderErrorToast(writer, req,
			pgh.t(req, "error.marketplaceLaunch.noTenants"))

		return
	}

	schemas, _ := pgh.schemaSvc.List(req.Context(), usr)

	rawKind := req.URL.Query().Get(createKindQueryParam)
	kind := selectKnownKind(rawKind, schemas)
	kindDisplay := displayNameForKind(kind, schemas)

	if len(tenants) == 1 {
		redirectToCreateKind(writer, req, tenants[0].Namespace, kind)

		return
	}

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	renderErr := partial.MarketplaceLaunchModal(kind, kindDisplay, tenants).
		Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering marketplace launch modal", "error", renderErr)
	}
}

// redirectToCreateKind sends the browser to the destination tenant's
// create-app form. Uses Hx-Redirect for htmx clients (htmx follows it
// without a full reload) and 303 See Other for direct browser
// navigation. Extracted from MarketplaceLaunchPage so the latter
// stays under the funlen budget.
func redirectToCreateKind(writer http.ResponseWriter, req *http.Request, namespace, kind string) {
	dest := "/tenants/" + namespace
	if kind != "" {
		dest += "?createKind=" + url.QueryEscape(kind)
	}

	if req.Header.Get("Hx-Request") != "" {
		writer.Header().Set("Hx-Redirect", dest)
		writer.WriteHeader(http.StatusOK)

		return
	}

	http.Redirect(writer, req, dest, http.StatusSeeOther)
}

// displayNameForKind looks up the human-readable name for a Kind
// (e.g. "Bucket" → "Buckets") from the schema list. Returns the raw
// kind unchanged when no schema matches — the modal title falls back
// to the kind, which is still meaningful, and an empty kind yields an
// empty string the caller can short-circuit on.
func displayNameForKind(kind string, schemas []k8s.AppSchema) string {
	if kind == "" {
		return ""
	}

	// Index access (rather than range value-copy) avoids cloning the
	// whole AppSchema struct on every iteration; the JSONSchema field
	// alone can be many KB.
	for i := range schemas {
		if schemas[i].Kind == kind {
			return schemas[i].DisplayName
		}
	}

	return kind
}

// MarketplacePage renders the marketplace catalog.
func (pgh *PageHandler) MarketplacePage(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenants, _ := pgh.tenantSvc.List(req.Context(), usr)
	schemas, _ := pgh.schemaSvc.List(req.Context(), usr)

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

	tenants, _ := pgh.tenantSvc.List(req.Context(), usr)
	pgh.renderError(writer, req, usr.Username, tenants, http.StatusNotFound,
		"Page not found",
		"The page '"+req.URL.Path+"' does not exist. Use the navigation on the left or head back to the dashboard.")
}

// SetLanguage persists the user's chosen locale in a cookie and
// redirects back to the page they came from. POST because writing
// a cookie is a mutation. The Referer header drives the redirect
// target so the language switch feels in-place — no forced
// navigation to the dashboard.
func (pgh *PageHandler) SetLanguage(writer http.ResponseWriter, req *http.Request) {
	// Cap the form body before ParseForm so a hostile client
	// can't stream megabytes of form data. 1 KB is plenty for
	// a single `lang=xx` field.
	req.Body = http.MaxBytesReader(writer, req.Body, maxLangFormBytes)

	parseErr := req.ParseForm()
	if parseErr != nil {
		http.Error(writer, "bad request", http.StatusBadRequest)

		return
	}

	chosen := req.FormValue("lang")

	tag, ok := i18n.LookupSupported(chosen)
	if !ok {
		http.Error(writer, "unsupported locale", http.StatusBadRequest)

		return
	}

	i18n.SetLocaleCookie(writer, req, tag)

	// Prefer Referer so the switch stays on the current page.
	// Fall back to the dashboard if the header is missing
	// (direct curl, privacy mode, etc).
	target := req.Header.Get("Referer")
	if target == "" {
		target = "/"
	}

	// Use HX-Redirect when the request came from htmx so the
	// browser does a full reload to re-render every translated
	// string. Without the full reload the already-swapped
	// fragments keep their old-language text.
	if req.Header.Get("Hx-Request") != "" {
		writer.Header().Set("Hx-Redirect", target)
		writer.WriteHeader(http.StatusOK)

		return
	}

	http.Redirect(writer, req, target, http.StatusSeeOther)
}

// maxLangFormBytes is the body cap on POST /lang. The form has
// exactly one field so a kilobyte is wildly generous.
const maxLangFormBytes = 1024

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
			req.Context(), usr, tenantNS, appName, appEventLimit,
		)
		if err != nil {
			pgh.log.Debug("listing app events", "tenant", tenantNS, "app", appName, "error", err)
		}

		data.Events = events
	case "logs":
		data.Pods, data.SelectedPod, data.SelectedContainer, data.LogTail, data.LogError = pgh.fetchAppLogs(req, usr, tenantNS, appName)
	}

	data.AllowedActions = pgh.capabilityProbedActions(req, usr, tenantNS, app.Kind)

	return data
}

// capabilityProbedActions turns the compile-time action registry
// into the user-specific subset the UI should render. Runs one
// SelfSubjectAccessReview per registered action in the tenant
// namespace — cheap at the apiserver level, and cached by the
// CachedDiscovery+RBAC path so repeat loads on the same page are
// near-free. A probe error drops the offending action (safer than
// showing a button that 403s) but leaves the rest intact, and the
// first probe error is logged for an operator to investigate
// without spamming the log if every probe fails.
//
// Returns nil when no actions are registered for the Kind, so the
// caller can treat empty-list and nil identically.
func (pgh *PageHandler) capabilityProbedActions(
	req *http.Request, usr *auth.UserContext, tenantNS, kind string,
) []actions.Action {
	list := actions.For(kind)
	if len(list) == 0 {
		return nil
	}

	userCfg, err := k8s.BuildUserRESTConfig(pgh.baseCfg, usr, pgh.authMode)
	if err != nil {
		pgh.log.Debug("building rest config for action probe", "error", err)

		return nil
	}

	kept, probeErr := actions.FilterAllowed(req.Context(), userCfg, list, tenantNS)
	if probeErr != nil {
		pgh.log.Debug("probing action capabilities", "tenant", tenantNS, "kind", kind, "error", probeErr)
	}

	return kept
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
	pods, listErr := pgh.logSvc.ListPodsForApp(req.Context(), usr, tenantNS, appName)
	if listErr != nil {
		pgh.log.Debug("listing pods for logs tab", "tenant", tenantNS, "app", appName, "error", listErr)

		return nil, "", "", "", pgh.t(req, "error.logs.list") + ": " + listErr.Error()
	}

	if len(pods) == 0 {
		return pods, "", "", "", pgh.t(req, "empty.pods.body")
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
		return pods, selectedPod, "", "", pgh.t(req, "error.pods.selectedMissing")
	}

	selectedContainer := req.URL.Query().Get("container")
	if selectedContainer == "" && len(chosen.Containers) > 0 {
		selectedContainer = chosen.Containers[0]
	}

	tail, tailErr := pgh.logSvc.TailLogs(req.Context(), usr, tenantNS,
		selectedPod, selectedContainer, appLogTailLines)
	if tailErr != nil {
		pgh.log.Debug("tailing logs", "pod", selectedPod, "container", selectedContainer, "error", tailErr)

		return pods, selectedPod, selectedContainer, "", pgh.t(req, "error.logs.read") + ": " + tailErr.Error()
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
	appList, _ := pgh.appSvc.List(req.Context(), usr, tenantNS)

	schemas, schemasErr := pgh.schemaSvc.List(req.Context(), usr)
	if schemasErr != nil {
		pgh.log.Warn("listing app schemas for kind validation", "error", schemasErr)
	}

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
		req.Context(), usr, tenantNS, tenantEventLimit,
	)
	if evtErr != nil {
		pgh.log.Debug("listing tenant events", "tenant", tenantNS, "error", evtErr)
	}

	usage, usageErr := pgh.usageSvc.Collect(req.Context(), usr, tenantNS)
	if usageErr != nil {
		pgh.log.Debug("collecting tenant usage", "tenant", tenantNS, "error", usageErr)
	}

	quotas, quotaErr := pgh.usageSvc.ListQuotas(req.Context(), usr, tenantNS)
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
		CreateKind:    extractCreateKindFromQuery(req, schemas),
	}
}

// createKindQueryParam is the single source of truth for the query
// parameter name the marketplace flow uses. Defined as a constant so
// the test for extractCreateKindFromQuery can pin it: any rename in
// production code that doesn't update both the constant and the test
// expectation surfaces as a compile failure.
const createKindQueryParam = "createKind"

// extractCreateKindFromQuery reads the createKind query param and
// validates it against the supplied schema list. Wraps two operations
// — the param read and the whitelist check — into a single helper so
// the test for it covers both, including a typo guard for the param
// name itself.
func extractCreateKindFromQuery(req *http.Request, schemas []k8s.AppSchema) string {
	return selectKnownKind(req.URL.Query().Get(createKindQueryParam), schemas)
}

// selectKnownKind returns raw only when it exactly matches one of the
// AppSchema kinds. Any empty, unknown, or injection-crafted value
// collapses to "", so downstream templates stay closed and no
// parameter-spoofed URL ever reaches the browser. Defence-in-depth on
// top of url.QueryEscape at the rendering layer.
func selectKnownKind(raw string, schemas []k8s.AppSchema) string {
	if raw == "" {
		return ""
	}

	for idx := range schemas {
		if schemas[idx].Kind == raw {
			return raw
		}
	}

	return ""
}

func (pgh *PageHandler) aggregateApps(
	req *http.Request, usr *auth.UserContext, tenants []k8s.Tenant,
) []k8s.Application {
	var allApps []k8s.Application

	for idx := range tenants {
		appList, err := pgh.appSvc.List(req.Context(), usr, tenants[idx].Namespace)
		if err != nil {
			pgh.log.Error("listing apps", "tenant", tenants[idx].Namespace, "error", err)

			continue
		}

		allApps = append(allApps, appList.Items...)
	}

	return allApps
}

// localizer returns the per-request Localizer the i18n middleware
// attached to ctx. Handlers pass the result to page templates so
// every user-visible string honours the caller's locale. Falls
// back to an English Localizer if the bundle is nil (tests).
func (pgh *PageHandler) localizer(req *http.Request) *i18n.Localizer {
	if pgh.i18nBundle == nil {
		return nil
	}

	return pgh.i18nBundle.LocalizerFromContext(req.Context())
}

// t resolves the request Localizer and returns the translated string
// for the given message ID. Used by the many handler call sites that
// format error-toast and success-toast copy — centralising the resolve
// keeps the call sites a single line and makes it obvious that the
// string is user-visible.
func (pgh *PageHandler) t(req *http.Request, messageID string, data ...map[string]any) string {
	loc := i18n.FromContext(req.Context())
	if loc == nil {
		return "[" + messageID + "]"
	}

	return loc.T(messageID, data...)
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
		AuthMode:  string(auth.ModeFromContext(req.Context())),
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

	loc := pgh.localizer(req)
	statusText := http.StatusText(status)
	backLabel := "Back to dashboard"

	if loc != nil {
		backLabel = loc.T("action.backToDashboard")
	}

	content := page.ErrorPage(status, statusText, title, detail, backLabel, "/")

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
			Localizer:    loc,
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

	loc := pgh.localizer(req)

	if req.Header.Get("Hx-Request") != "" {
		// Render page content + OOB sidebar swap to update active state
		renderErr = content.Render(req.Context(), writer)
		if renderErr == nil {
			renderErr = partial.SidebarOOB(tenants, activePage, activeTenant, loc).Render(req.Context(), writer)
		}
	} else {
		wrapped := layout.App(layout.AppProps{
			Username:     username,
			Tenants:      tenants,
			ActivePage:   activePage,
			ActiveTenant: activeTenant,
			DevMode:      pgh.devMode,
			Localizer:    loc,
		})
		renderErr = wrapped.Render(templ.WithChildren(req.Context(), content), writer)
	}

	if renderErr != nil {
		pgh.log.Error("rendering page", "error", renderErr)
	}
}
