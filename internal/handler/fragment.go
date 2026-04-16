package handler

import (
	"html"
	"net/http"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/lexfrei/cozytempl/internal/audit"
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

	appList, err := pgh.appSvc.List(req.Context(), usr, tenant)
	if err != nil {
		pgh.log.Error("listing apps for fragment", "tenant", tenant, "error", err)

		appList = k8s.ApplicationList{}
	}

	apps := filterAndSortApps(
		appList.Items,
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

// AppFormYAMLFragment renders the current form values (whatever
// is on the wire, form-mode fields only) as YAML, suitable for
// pasting into the spec_yaml textarea. The endpoint is POST so
// the client can send form-encoded state without cramming it
// into a URL; the reply is the raw YAML text inside the same
// textarea element so htmx can swap it verbatim.
//
// Used by the "Load from Form" button on the YAML tab of the
// create / edit modal — the user fills the form visually,
// switches to YAML, clicks Load, and gets a starting point they
// can tweak by hand.
func (pgh *PageHandler) AppFormYAMLFragment(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	req.Body = http.MaxBytesReader(writer, req.Body, maxFormBytes)

	parseErr := req.ParseForm()
	if parseErr != nil {
		http.Error(writer, "bad form", http.StatusBadRequest)

		return
	}

	kind := req.FormValue(formFieldKind)
	if kind == "" {
		// No kind yet — return an empty textarea. The create
		// modal only shows the YAML tab once the kind has been
		// picked, so this branch is mostly defensive.
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte(""))

		return
	}

	schema, schemaErr := pgh.schemaSvc.Get(req.Context(), usr, kind)
	if schemaErr != nil {
		pgh.log.Error("fetching schema for yaml preview", "kind", kind, "error", schemaErr)
		http.Error(writer, "schema not found", http.StatusNotFound)

		return
	}

	spec := extractSpecFromForm(req, extractFieldTypes(schema))

	raw, marshalErr := yaml.Marshal(spec)
	if marshalErr != nil {
		pgh.log.Error("marshalling spec to yaml", "kind", kind, "error", marshalErr)
		http.Error(writer, "yaml marshal failed", http.StatusInternalServerError)

		return
	}

	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = writer.Write(raw)
}

// AppFormYAMLToFormFragment is the reverse of
// AppFormYAMLFragment: the user types YAML on the YAML tab,
// clicks "Apply to Form", and this endpoint parses the YAML
// and re-renders the schema-driven form fields with the
// resulting values populated — closing the round-trip between
// raw YAML editing and the schema-driven UI.
//
// On invalid YAML the schema fields are re-rendered without
// values so the user can still fall back to the form — a
// blanked form is the honest outcome of "parse failed".
func (pgh *PageHandler) AppFormYAMLToFormFragment(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	req.Body = http.MaxBytesReader(writer, req.Body, maxFormBytes)

	parseErr := req.ParseForm()
	if parseErr != nil {
		http.Error(writer, "bad form", http.StatusBadRequest)

		return
	}

	kind := req.FormValue(formFieldKind)
	if kind == "" {
		http.Error(writer, "kind required", http.StatusBadRequest)

		return
	}

	schema, schemaErr := pgh.schemaSvc.Get(req.Context(), usr, kind)
	if schemaErr != nil {
		pgh.log.Error("fetching schema for yaml-to-form", "kind", kind, "error", schemaErr)
		http.Error(writer, "schema not found", http.StatusNotFound)

		return
	}

	spec := map[string]any{}
	// Parse the YAML but swallow a parse error here — the UI
	// should still show the schema fields (just un-populated)
	// so the user is not stuck on a dead modal. The YAML tab
	// itself keeps the user's draft because we only target
	// #schema-fields on the form pane.
	if raw := strings.TrimSpace(req.FormValue(formFieldSpecYAML)); raw != "" {
		parsed, err := parseSpecYAML(raw)
		if err != nil {
			pgh.log.Info("yaml-to-form parse failed; rendering empty fields",
				"kind", kind, "error", err)
		} else {
			spec = parsed
		}
	}

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	renderErr := fragment.SchemaFieldsWithValues(*schema, spec).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering yaml-to-form fields", "error", renderErr)
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

	schema, err := pgh.schemaSvc.Get(req.Context(), usr, kind)
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

	app, err := pgh.appSvc.Get(req.Context(), usr, tenant, name)
	if err != nil {
		pgh.log.Error("loading app for edit", "tenant", tenant, "name", name, "error", err)
		http.Error(writer, "application not found", http.StatusNotFound)

		return
	}

	snap, specErr := pgh.appSvc.GetSpecSnapshot(req.Context(), usr, tenant, name)

	var currentSpec map[string]any

	var resourceVersion string

	if specErr != nil {
		pgh.log.Debug("loading app spec for edit", "tenant", tenant, "name", name, "error", specErr)
	} else if snap != nil {
		currentSpec = snap.Spec
		resourceVersion = snap.ResourceVersion
	}

	schema, schemaErr := pgh.schemaSvc.Get(req.Context(), usr, app.Kind)
	if schemaErr != nil {
		pgh.log.Debug("loading schema for app edit", "kind", app.Kind, "error", schemaErr)
	}

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")

	renderErr := fragment.AppEditModal(tenant, *app, schema, currentSpec, resourceVersion).Render(req.Context(), writer)
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
	tenant, err := pgh.tenantSvc.Get(req.Context(), usr, name)
	if err != nil {
		pgh.log.Error("loading tenant for edit", "name", name, "error", err)
		http.Error(writer, "tenant not found", http.StatusNotFound)

		return
	}

	// Current spec from the impersonated CR read — the CRs live in the
	// parent's workload namespace, hence the 'namespace' query param.
	snap, specErr := pgh.tenantSvc.GetSpecSnapshot(req.Context(), usr, namespace, name)

	var currentSpec map[string]any

	var resourceVersion string

	if specErr != nil {
		pgh.log.Debug("loading tenant spec for edit", "ns", namespace, "name", name, "error", specErr)
	} else if snap != nil {
		currentSpec = snap.Spec
		resourceVersion = snap.ResourceVersion
	}

	// Schema is optional: if it fails we still render the modal with a
	// "no editable fields" note so the user is not left without feedback.
	schema, schemaErr := pgh.schemaSvc.Get(req.Context(), usr, tenantSchemaKind)
	if schemaErr != nil {
		pgh.log.Debug("loading tenant schema for edit", "error", schemaErr)
	}

	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")

	renderErr := fragment.TenantEditModal(*tenant, schema, currentSpec, resourceVersion).Render(req.Context(), writer)
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

	schemas, _ := pgh.schemaSvc.List(req.Context(), usr)

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

// SecretRevealFragment serves a single connection-info credential
// in response to an htmx click-to-reveal. The value is re-fetched
// from k8s via the app service rather than pulled from a cached
// render, and every call produces an audit event so the compliance
// trail can answer "who looked at the postgres password at 14:02."
//
// The response is a plain HTML fragment (no wrapper) intended to be
// swapped into the [data-reveal-target] span on the page. A
// client-side timer (static/ts/reveal.ts) hides the value again
// after ~30 seconds to bound DOM exposure.
func (pgh *PageHandler) SecretRevealFragment(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenant := req.URL.Query().Get("tenant")
	appName := req.URL.Query().Get("app")
	field := req.URL.Query().Get("field")

	if tenant == "" || appName == "" || field == "" {
		http.Error(writer, "tenant, app and field required", http.StatusBadRequest)

		return
	}

	app, err := pgh.appSvc.Get(req.Context(), usr, tenant, appName)
	if err != nil {
		pgh.log.Error("loading app for secret reveal", "tenant", tenant, "app", appName, "error", err)
		pgh.recordAudit(req, usr, audit.ActionSecretView, appName, tenant,
			audit.OutcomeError, map[string]any{"field": field, "error": err.Error()})
		http.Error(writer, "application not found", http.StatusNotFound)

		return
	}

	value, ok := app.ConnectionInfo[field]
	if !ok {
		pgh.recordAudit(req, usr, audit.ActionSecretView, appName, tenant,
			audit.OutcomeDenied, map[string]any{"field": field, "reason": "field_not_found"})
		http.Error(writer, "secret field not found", http.StatusNotFound)

		return
	}

	pgh.recordAudit(req, usr, audit.ActionSecretView, appName, tenant,
		audit.OutcomeSuccess, map[string]any{"field": field, "kind": app.Kind})

	// Emit the value wrapped in HTML escape so an unusual secret
	// (e.g. contains <) cannot break out of the target element.
	// Cache-Control: no-store keeps the revealed credential from
	// lingering in any intermediate proxy.
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	writer.Header().Set("Pragma", "no-cache")
	_, _ = writer.Write([]byte(html.EscapeString(value)))
}
