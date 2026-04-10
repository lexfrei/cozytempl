package handler

import (
	"errors"
	"net/http"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view"
	"github.com/lexfrei/cozytempl/internal/view/page"
)

const tenantSchemaKind = "Tenant"

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

// DeleteTenant handles DELETE /tenants/{name}.
func (pgh *PageHandler) DeleteTenant(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	name := req.PathValue("name")
	if name == "" {
		http.Error(writer, "name required", http.StatusBadRequest)

		return
	}

	err := pgh.tenantSvc.Delete(req.Context(), usr.Username, usr.Groups, name)
	if err != nil {
		if errors.Is(err, k8s.ErrProtectedTenant) {
			pgh.renderErrorToast(writer, req, "Root tenant is protected and cannot be deleted")

			return
		}

		pgh.log.Error("deleting tenant", "name", name, "error", err)
		pgh.renderErrorToast(writer, req, "Failed to delete tenant: "+err.Error())

		return
	}

	pgh.log.Info("tenant deleted", "name", name)
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
