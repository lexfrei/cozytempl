package handler

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view"
	"github.com/lexfrei/cozytempl/internal/view/page"
)

const (
	tenantSchemaKind = "Tenant"

	// maxDNS1123LabelLength matches the Kubernetes limit for object names.
	maxDNS1123LabelLength = 63
)

// dns1123LabelRegex matches RFC 1123 labels: lowercase alphanumerics and
// hyphens, starting and ending with alphanumeric, up to 63 chars.
var dns1123LabelRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// validTenantName reports whether s is a valid Kubernetes DNS-1123 label.
func validTenantName(s string) bool {
	if s == "" || len(s) > maxDNS1123LabelLength {
		return false
	}

	return dns1123LabelRegex.MatchString(s)
}

// TenantsPage renders the tenant management page: list + create form.
func (pgh *PageHandler) TenantsPage(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	tenants, err := pgh.tenantSvc.List(req.Context(), usr.Username, usr.Groups)
	if err != nil {
		pgh.log.Error("listing tenants for tenants page", "error", err)

		tenants = []k8s.Tenant{}
	}

	namespaces := make([]string, 0, len(tenants))
	for idx := range tenants {
		namespaces = append(namespaces, tenants[idx].Namespace)
	}

	usageMap := pgh.usageSvc.CollectAll(req.Context(), usr.Username, usr.Groups, namespaces)

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

	schema, schemaErr := pgh.schemaSvc.Get(req.Context(), usr.Username, usr.Groups, tenantSchemaKind)
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
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	form, ok := pgh.parseTenantForm(writer, req)
	if !ok {
		return
	}

	if !validTenantName(form.Name) {
		pgh.renderErrorToast(writer, req, "Invalid name: must be lowercase DNS label (letters, digits, hyphens)")

		return
	}

	if k8s.IsRootTenant(form.Name) {
		pgh.renderErrorToast(writer, req, "Cannot create tenant named 'root' — reserved")

		return
	}

	spec := pgh.tenantSpec(req, usr)

	_, err := pgh.tenantSvc.Create(req.Context(), usr.Username, usr.Groups, k8s.CreateTenantRequest{
		Name:   form.Name,
		Parent: form.Parent,
		Spec:   spec,
	})
	if err != nil {
		pgh.log.Error("creating tenant", "name", form.Name, "error", err)
		pgh.renderErrorToast(writer, req, "Failed to create tenant: "+err.Error())

		return
	}

	pgh.log.Info("tenant created", "name", form.Name, "parent", form.Parent)
	writer.Header().Set("Hx-Redirect", "/tenants")
	writer.WriteHeader(http.StatusCreated)
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
	schema, schemaErr := pgh.schemaSvc.Get(req.Context(), usr.Username, usr.Groups, tenantSchemaKind)

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
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

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

	spec := pgh.tenantSpec(req, usr)
	if spec == nil {
		pgh.renderErrorToast(writer, req, "Nothing to update: no form fields recognized against the tenant schema")

		return
	}

	_, err := pgh.tenantSvc.Update(req.Context(), usr.Username, usr.Groups, namespace, name, spec)
	if err != nil {
		pgh.log.Error("updating tenant", "ns", namespace, "name", name, "error", err)
		pgh.renderErrorToast(writer, req, "Failed to update tenant: "+err.Error())

		return
	}

	pgh.log.Info("tenant updated", "ns", namespace, "name", name, "keys", len(spec))
	writer.Header().Set("Hx-Redirect", "/tenants")
	writer.WriteHeader(http.StatusOK)
}

// DeleteTenant handles DELETE /tenants/{name}?ns=... to remove a tenant.
// Namespace disambiguates same-named tenants under different parents.
func (pgh *PageHandler) DeleteTenant(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	name := req.PathValue("name")
	namespace := req.URL.Query().Get("ns")

	if name == "" || namespace == "" {
		http.Error(writer, "name and ns required", http.StatusBadRequest)

		return
	}

	err := pgh.tenantSvc.Delete(req.Context(), usr.Username, usr.Groups, namespace, name)
	if err != nil {
		if errors.Is(err, k8s.ErrProtectedTenant) {
			pgh.renderErrorToast(writer, req, "Root tenant is protected and cannot be deleted")

			return
		}

		pgh.log.Error("deleting tenant", "ns", namespace, "name", name, "error", err)
		pgh.renderErrorToast(writer, req, "Failed to delete tenant: "+err.Error())

		return
	}

	pgh.log.Info("tenant deleted", "ns", namespace, "name", name)
	writer.WriteHeader(http.StatusOK)
}

func extractTenantSpec(req *http.Request, fieldTypes map[string]string) map[string]any {
	spec := map[string]any{}

	for key, values := range req.Form {
		if key == formFieldName || key == "parent" {
			continue
		}

		if len(values) == 0 || values[0] == "" {
			continue
		}

		spec[key] = convertValue(values[0], fieldTypes[key])
	}

	if len(spec) == 0 {
		return nil
	}

	return spec
}
