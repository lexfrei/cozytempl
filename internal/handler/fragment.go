package handler

import (
	"net/http"
	"sort"
	"strings"

	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view/fragment"
	"github.com/lexfrei/cozytempl/internal/view/page"
)

// AppTableFragment renders filtered app table rows for htmx swap.
func (pgh *PageHandler) AppTableFragment(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenant := req.URL.Query().Get("tenant")

	apps, err := pgh.appSvc.List(req.Context(), usr.Username, usr.Groups, tenant)
	if err != nil {
		pgh.log.Error("listing apps for fragment", "tenant", tenant, "error", err)

		apps = []k8s.Application{}
	}

	apps = filterAndSortApps(
		apps,
		req.URL.Query().Get("q"),
		req.URL.Query().Get("kind"),
		req.URL.Query().Get("sort"),
	)

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	renderErr := page.AppTableRows(tenant, apps).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering app table fragment", "error", renderErr)
	}
}

// SchemaFieldsFragment renders schema-driven form fields for the create app modal.
func (pgh *PageHandler) SchemaFieldsFragment(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	kind := req.URL.Query().Get("kind")
	if kind == "" {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte(""))

		return
	}

	schema, err := pgh.schemaSvc.Get(req.Context(), usr.Username, usr.Groups, kind)
	if err != nil {
		pgh.log.Error("fetching schema", "kind", kind, "error", err)
		http.Error(writer, "schema not found", http.StatusNotFound)

		return
	}

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	renderErr := fragment.SchemaFields(*schema).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering schema fields fragment", "error", renderErr)
	}
}

// AppEditFragment renders the edit modal for a single application with
// the form fields pre-populated from its current spec. Mirrors the
// TenantEditFragment flow — schema + current spec are fetched via
// impersonation so permission errors surface here, not on PUT.
func (pgh *PageHandler) AppEditFragment(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenant := req.URL.Query().Get("tenant")
	name := req.URL.Query().Get("name")

	if tenant == "" || name == "" {
		http.Error(writer, "tenant and name required", http.StatusBadRequest)

		return
	}

	app, err := pgh.appSvc.Get(req.Context(), usr.Username, usr.Groups, tenant, name)
	if err != nil {
		pgh.log.Error("loading app for edit", "tenant", tenant, "name", name, "error", err)
		http.Error(writer, "application not found", http.StatusNotFound)

		return
	}

	currentSpec, _, specErr := pgh.appSvc.GetSpec(req.Context(), usr.Username, usr.Groups, tenant, name)
	if specErr != nil {
		pgh.log.Debug("loading app spec for edit", "tenant", tenant, "name", name, "error", specErr)
	}

	schema, schemaErr := pgh.schemaSvc.Get(req.Context(), usr.Username, usr.Groups, app.Kind)
	if schemaErr != nil {
		pgh.log.Debug("loading schema for app edit", "kind", app.Kind, "error", schemaErr)
	}

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")

	renderErr := fragment.AppEditModal(tenant, *app, schema, currentSpec).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering app edit modal", "error", renderErr)
	}
}

// TenantEditFragment renders the edit modal for a single tenant, pre-
// populating the form fields from the tenant's current spec so the user
// edits in place.
func (pgh *PageHandler) TenantEditFragment(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	name := req.URL.Query().Get("name")
	namespace := req.URL.Query().Get("ns")

	if name == "" || namespace == "" {
		http.Error(writer, "name and ns required", http.StatusBadRequest)

		return
	}

	// The current tenant view is looked up under its workload namespace
	// (what the sidebar / tenant list links to). Pull the full CR through
	// TenantService.Get so we get DisplayName, Namespace, ParentNamespace.
	tenant, err := pgh.tenantSvc.Get(req.Context(), usr.Username, usr.Groups, name)
	if err != nil {
		pgh.log.Error("loading tenant for edit", "name", name, "error", err)
		http.Error(writer, "tenant not found", http.StatusNotFound)

		return
	}

	// Current spec from the impersonated CR read — the CRs live in the
	// parent's workload namespace, hence the 'namespace' query param.
	currentSpec, specErr := pgh.tenantSvc.GetSpec(req.Context(), usr.Username, usr.Groups, namespace, name)
	if specErr != nil {
		pgh.log.Debug("loading tenant spec for edit", "ns", namespace, "name", name, "error", specErr)
	}

	// Schema is optional: if it fails we still render the modal with a
	// "no editable fields" note so the user is not left without feedback.
	schema, schemaErr := pgh.schemaSvc.Get(req.Context(), usr.Username, usr.Groups, tenantSchemaKind)
	if schemaErr != nil {
		pgh.log.Debug("loading tenant schema for edit", "error", schemaErr)
	}

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")

	renderErr := fragment.TenantEditModal(*tenant, schema, currentSpec).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering tenant edit modal", "error", renderErr)
	}
}

// MarketplaceFragment renders filtered marketplace grid for htmx swap.
func (pgh *PageHandler) MarketplaceFragment(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	schemas, _ := pgh.schemaSvc.List(req.Context(), usr.Username, usr.Groups)

	data := buildMarketplaceData(
		schemas,
		req.URL.Query().Get("q"),
		req.URL.Query().Get("category"),
		req.URL.Query().Get("tag"),
	)

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	renderErr := page.MarketplaceGrid(data).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering marketplace fragment", "error", renderErr)
	}
}

func filterAndSortApps(apps []k8s.Application, query, kindFilter, sortBy string) []k8s.Application {
	var filtered []k8s.Application

	for idx := range apps {
		if kindFilter != "" && apps[idx].Kind != kindFilter {
			continue
		}

		if query != "" {
			q := strings.ToLower(query)
			if !strings.Contains(strings.ToLower(apps[idx].Name), q) {
				continue
			}
		}

		filtered = append(filtered, apps[idx])
	}

	if sortBy == "" {
		sortBy = sortByName
	}

	sort.Slice(filtered, func(left, right int) bool {
		switch sortBy {
		case sortByKind:
			return filtered[left].Kind < filtered[right].Kind
		case "status":
			return string(filtered[left].Status) < string(filtered[right].Status)
		default:
			return filtered[left].Name < filtered[right].Name
		}
	})

	return filtered
}
