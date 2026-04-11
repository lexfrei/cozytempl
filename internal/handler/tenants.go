package handler

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/lexfrei/cozytempl/internal/audit"
	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view"
	"github.com/lexfrei/cozytempl/internal/view/page"
)

const (
	tenantSchemaKind = "Tenant"

	// maxTenantNameLength is the hard cap on the user-visible tenant
	// name. The workload namespace prefixes it with "tenant-" (7 chars)
	// and needs to stay under DNS-1123's 63-char limit, so we allow 56
	// characters for the name itself.
	maxTenantNameLength = 56
)

// tenantNameRegex enforces cozystack 1.2's stricter rule: the Helm
// release name for a Tenant is "tenant-<name>", and the chart template
// rejects anything that contains a dash in <name> because it would
// produce more than one dash in the final release name. The message
// from the chart is literally:
//
//	The release name should start with "tenant-" and should not
//	contain any other dashes
//
// So the user-visible name must be lowercase alphanumerics only, not
// starting with a digit (so the resulting K8s namespace stays a valid
// DNS-1123 label even after the prefix is added).
var tenantNameRegex = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// validTenantName reports whether s is a valid cozystack tenant name.
// This is stricter than DNS-1123: no dashes allowed, because the
// downstream Helm chart composes release names by prepending "tenant-"
// and refuses any further dashes.
func validTenantName(s string) bool {
	if s == "" || len(s) > maxTenantNameLength {
		return false
	}

	return tenantNameRegex.MatchString(s)
}

// TenantsPage renders the tenant management page: list + create form.
func (pgh *PageHandler) TenantsPage(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenants, err := pgh.tenantSvc.List(req.Context(), usr)
	if err != nil {
		pgh.log.Error("listing tenants for tenants page", "error", err)

		tenants = []k8s.Tenant{}
	}

	namespaces := make([]string, 0, len(tenants))
	for idx := range tenants {
		namespaces = append(namespaces, tenants[idx].Namespace)
	}

	usageMap := pgh.usageSvc.CollectAll(req.Context(), usr, namespaces)

	items := make([]view.TenantWithUsage, 0, len(tenants))
	metricsEnabled := false

	for idx := range tenants {
		usage := usageMap[tenants[idx].Namespace]
		if usage.MetricsEnabled {
			metricsEnabled = true
		}

		items = append(items, view.TenantWithUsage{
			Tenant: tenants[idx],
			Usage:  usage,
		})
	}

	schema, schemaErr := pgh.schemaSvc.Get(req.Context(), usr, tenantSchemaKind)
	if schemaErr != nil {
		pgh.log.Debug("fetching tenant schema", "error", schemaErr)
	}

	data := view.TenantsPageData{
		Tenants:        items,
		TenantSchema:   schema,
		MetricsEnabled: metricsEnabled,
	}

	content := page.Tenants(data)
	pgh.render(writer, req, usr.Username, tenants, "tenants", "", content)
}

// CreateTenant handles POST /tenants to create a new tenant.
func (pgh *PageHandler) CreateTenant(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	form, ok := pgh.parseTenantForm(writer, req)
	if !ok {
		return
	}

	if !validTenantName(form.Name) {
		pgh.recordAudit(req, usr, audit.ActionTenantCreate, form.Name, form.Parent,
			audit.OutcomeDenied, map[string]any{"reason": "invalid_name"})
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.tenant.invalidName"))

		return
	}

	if k8s.IsRootTenant(form.Name) {
		pgh.recordAudit(req, usr, audit.ActionTenantCreate, form.Name, form.Parent,
			audit.OutcomeDenied, map[string]any{"reason": "reserved_name"})
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.tenant.rootReserved"))

		return
	}

	spec := pgh.tenantSpec(req, usr)

	_, err := pgh.tenantSvc.Create(req.Context(), usr, k8s.CreateTenantRequest{
		Name:   form.Name,
		Parent: form.Parent,
		Spec:   spec,
	})
	if err != nil {
		// Log full error context for operators; user-facing message is
		// generic to avoid leaking details of parent tenants the user
		// cannot see or RBAC policy.
		pgh.log.Error("creating tenant", "name", form.Name, "error", err)
		pgh.recordAudit(req, usr, audit.ActionTenantCreate, form.Name, form.Parent,
			audit.OutcomeError, map[string]any{"error": err.Error()})
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.tenant.create"))

		return
	}

	pgh.log.Info("tenant created", "name", form.Name, "parent", form.Parent)
	pgh.recordAudit(req, usr, audit.ActionTenantCreate, form.Name, form.Parent,
		audit.OutcomeSuccess, nil)
	pgh.emitSuccessToast(writer, req, pgh.t(req, "toast.tenant.created", map[string]any{"Name": form.Name}))
	// Re-render the tenants list so the new row shows up and the
	// create modal closes with a fresh (closed) template.
	pgh.TenantsPage(writer, req)
}

type tenantFormValues struct {
	Name   string
	Parent string
}

func (pgh *PageHandler) parseTenantForm(writer http.ResponseWriter, req *http.Request) (tenantFormValues, bool) {
	req.Body = http.MaxBytesReader(writer, req.Body, maxFormBytes)

	err := req.ParseForm()
	if err != nil {
		http.Error(writer, "bad request", http.StatusBadRequest)

		return tenantFormValues{}, false
	}

	name := req.FormValue(formFieldName)
	parent := req.FormValue("parent")

	if name == "" {
		http.Error(writer, "name required", http.StatusBadRequest)

		return tenantFormValues{}, false
	}

	return tenantFormValues{Name: name, Parent: parent}, true
}

func (pgh *PageHandler) tenantSpec(req *http.Request, usr *auth.UserContext) map[string]any {
	schema, schemaErr := pgh.schemaSvc.Get(req.Context(), usr, tenantSchemaKind)

	var fieldTypes map[string]string
	if schemaErr == nil {
		fieldTypes = extractFieldTypes(schema)
	}

	return extractTenantSpec(req, fieldTypes)
}

// UpdateTenant handles PUT /tenants/{name}?ns=... to edit a tenant's spec.
// The ns query param is the parent namespace where the CR lives (not the
// tenant's own workload namespace), same convention as DeleteTenant.
func (pgh *PageHandler) UpdateTenant(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	name := req.PathValue("name")
	namespace := req.URL.Query().Get("ns")

	if name == "" || namespace == "" {
		http.Error(writer, "name and ns required", http.StatusBadRequest)

		return
	}

	req.Body = http.MaxBytesReader(writer, req.Body, maxFormBytes)

	parseErr := req.ParseForm()
	if parseErr != nil {
		http.Error(writer, "bad request", http.StatusBadRequest)

		return
	}

	pgh.doUpdateTenant(writer, req, usr, namespace, name)
}

// doUpdateTenant is the body of UpdateTenant after form parsing.
// Split out so the public handler stays under the funlen limit and
// the error-branch plumbing around ErrConflict reads cleanly.
func (pgh *PageHandler) doUpdateTenant(
	writer http.ResponseWriter, req *http.Request, usr *auth.UserContext, namespace, name string,
) {
	spec := pgh.tenantSpec(req, usr)
	if len(spec) == 0 {
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.tenant.noFields"))

		return
	}

	// The edit form carries the resourceVersion as a hidden input
	// so the Update can pin optimistic-lock semantics. Empty string
	// falls back to last-write-wins for any caller that has not been
	// migrated yet (e.g. a direct curl against the endpoint).
	// Safe: the outer handler already wrapped req.Body with
	// MaxBytesReader and called ParseForm.
	resourceVersion := req.FormValue(formFieldResourceVersion) //nolint:gosec // body capped by caller

	_, err := pgh.tenantSvc.Update(req.Context(), usr, namespace, name, spec, resourceVersion)
	if err != nil {
		if errors.Is(err, k8s.ErrConflict) {
			pgh.log.Info("conflict updating tenant", "ns", namespace, "name", name)
			pgh.recordAudit(req, usr, audit.ActionTenantUpdate, name, namespace,
				audit.OutcomeError, map[string]any{"reason": "conflict"})
			pgh.renderErrorToast(writer, req, pgh.t(req, "error.tenant.conflict"))

			return
		}

		pgh.log.Error("updating tenant", "ns", namespace, "name", name, "error", err)
		pgh.recordAudit(req, usr, audit.ActionTenantUpdate, name, namespace,
			audit.OutcomeError, map[string]any{"error": err.Error()})
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.tenant.update"))

		return
	}

	pgh.log.Info("tenant updated", "ns", namespace, "name", name, "keys", len(spec))
	pgh.recordAudit(req, usr, audit.ActionTenantUpdate, name, namespace, audit.OutcomeSuccess,
		map[string]any{"keys": len(spec)})
	pgh.emitSuccessToast(writer, req, pgh.t(req, "toast.tenant.updated", map[string]any{"Name": name}))
	pgh.TenantsPage(writer, req)
}

// DeleteTenant handles DELETE /tenants/{name}?ns=... to remove a tenant.
// Namespace disambiguates same-named tenants under different parents.
func (pgh *PageHandler) DeleteTenant(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	name := req.PathValue("name")
	namespace := req.URL.Query().Get("ns")

	if name == "" || namespace == "" {
		http.Error(writer, "name and ns required", http.StatusBadRequest)

		return
	}

	err := pgh.tenantSvc.Delete(req.Context(), usr, namespace, name)
	if err != nil {
		if errors.Is(err, k8s.ErrProtectedTenant) {
			pgh.recordAudit(req, usr, audit.ActionTenantDelete, name, namespace,
				audit.OutcomeDenied, map[string]any{"reason": "protected_tenant"})
			pgh.renderErrorToast(writer, req, pgh.t(req, "error.tenant.rootProtected"))

			return
		}

		pgh.log.Error("deleting tenant", "ns", namespace, "name", name, "error", err)
		pgh.recordAudit(req, usr, audit.ActionTenantDelete, name, namespace,
			audit.OutcomeError, map[string]any{"error": err.Error()})
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.tenant.delete"))

		return
	}

	pgh.log.Info("tenant deleted", "ns", namespace, "name", name)
	pgh.recordAudit(req, usr, audit.ActionTenantDelete, name, namespace,
		audit.OutcomeSuccess, nil)
	// Delete is hx-swap="delete swap:500ms" — the row disappears
	// client-side regardless of the response body. Toast only; no
	// re-render so the row-delete animation plays cleanly.
	pgh.emitSuccessToast(writer, req, pgh.t(req, "toast.tenant.deleted", map[string]any{"Name": name}))
}

// isReservedTenantFormKey reports whether a form key should be skipped
// when building the tenant spec. The tenant name, the chosen parent
// namespace, the "ns" query-string param used by Update/Delete to
// disambiguate sibling tenants, and the _resource_version optimistic
// locking token must not leak into spec — ParseForm merges query and
// body into req.Form, so any of these can appear alongside real form
// fields and would otherwise land on spec.foo = "bar".
func isReservedTenantFormKey(key string) bool {
	return key == formFieldName ||
		key == "parent" ||
		key == "ns" ||
		key == formFieldResourceVersion
}

// extractTenantSpec pulls schema-driven fields out of the tenant form.
// Dot-path keys ("backup.enabled") are un-flattened into nested maps
// via setNestedSpec, mirroring the app-create/update path so nested
// schema fields round-trip correctly.
// Always returns a non-nil map so the CRD validation path does not need
// to special-case empty objects.
func extractTenantSpec(req *http.Request, fieldTypes map[string]string) map[string]any {
	spec := map[string]any{}

	for key, values := range req.Form {
		if isReservedTenantFormKey(key) {
			continue
		}

		if len(values) == 0 || values[0] == "" {
			continue
		}

		setNestedSpec(spec, key, convertValue(values[0], fieldTypes[key]))
	}

	return spec
}
