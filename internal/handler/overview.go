package handler

import (
	"net/http"
	"sort"

	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view"
	"github.com/lexfrei/cozytempl/internal/view/page"
)

// OverviewPage renders the cross-tenant application list. Fans out
// one List call per visible tenant (same pattern as the dashboard's
// aggregateApps) and groups the results by Kind. RBAC is honoured
// transitively: tenants the caller cannot see don't appear in the
// tenantSvc.List result, and applications the caller cannot list
// inside a visible tenant get an Error from appSvc.List which the
// aggregator swallows after logging — the per-tenant error never
// blocks the rest of the page.
//
// No caching: the overview is always freshly-fetched. Operators
// visiting /overview expect the view to reflect reality within a
// page-load. A short-TTL cache would be defensible once the
// generic watch proxy (issue #4) ships and SSE can push row
// updates; until then, keep it simple.
func (pgh *PageHandler) OverviewPage(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenants, err := pgh.tenantSvc.List(req.Context(), usr)
	if err != nil {
		pgh.log.Error("listing tenants for overview", "error", err)

		tenants = []k8s.Tenant{}
	}

	allApps := pgh.aggregateApps(req, usr, tenants)
	data := buildOverviewData(allApps, len(tenants), req.URL.Query().Get("q"))

	content := page.Overview(data)
	pgh.render(writer, req, usr.Username, tenants, "overview", "", content)
}

// buildOverviewData groups a flat application list by Kind,
// preserving a stable order so the page reads the same way across
// renders. Apps within a group are sorted by "tenant/name" — the
// natural key operators scan when looking for a specific workload.
//
// Extracted into a pure function (no *PageHandler receiver, no k8s
// I/O) so the unit tests can drive it with fixtures without
// standing up the whole page-handler harness.
func buildOverviewData(apps []k8s.Application, tenantCount int, query string) view.OverviewData {
	byKind := map[string][]k8s.Application{}

	for idx := range apps {
		kind := apps[idx].Kind
		byKind[kind] = append(byKind[kind], apps[idx])
	}

	kinds := make([]string, 0, len(byKind))
	for kind := range byKind {
		kinds = append(kinds, kind)
	}

	sort.Strings(kinds)

	groups := make([]view.KindGroup, 0, len(kinds))

	for _, kind := range kinds {
		items := byKind[kind]

		sort.Slice(items, func(i, j int) bool {
			if items[i].Tenant != items[j].Tenant {
				return items[i].Tenant < items[j].Tenant
			}

			return items[i].Name < items[j].Name
		})

		groups = append(groups, view.KindGroup{Kind: kind, Apps: items})
	}

	return view.OverviewData{
		Groups:       groups,
		TotalApps:    len(apps),
		TotalTenants: tenantCount,
		Query:        query,
	}
}
