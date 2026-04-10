package handler

import (
	"net/http"
	"strconv"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view/partial"
)

const (
	maxFormBytes  = 1 << 20 // 1 MB
	formFieldName = "name"
	formFieldKind = "kind"
	sortByName    = "name"
	sortByKind    = "kind"
)

// CreateApp handles POST to create a new application.
func (pgh *PageHandler) CreateApp(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	tenantNS := req.PathValue("tenant")

	req.Body = http.MaxBytesReader(writer, req.Body, maxFormBytes)

	parseErr := req.ParseForm()
	if parseErr != nil {
		http.Error(writer, "bad request", http.StatusBadRequest)

		return
	}

	appName := req.FormValue(formFieldName)
	appKind := req.FormValue(formFieldKind)

	if appName == "" || appKind == "" {
		http.Error(writer, "name and kind required", http.StatusBadRequest)

		return
	}

	pgh.doCreateApp(writer, req, usr, tenantNS, appName, appKind)
}

func (pgh *PageHandler) doCreateApp(
	writer http.ResponseWriter,
	req *http.Request,
	usr *auth.UserContext,
	tenantNS, appName, appKind string,
) {
	// Fetch schema to know field types for correct JSON encoding
	schema, schemaErr := pgh.schemaSvc.Get(req.Context(), usr.Username, usr.Groups, appKind)
	if schemaErr != nil {
		pgh.log.Error("fetching schema for create", "kind", appKind, "error", schemaErr)
		pgh.renderToast(writer, req, "error", "Failed to load schema for "+appKind)

		return
	}

	fieldTypes := extractFieldTypes(schema)

	createReq := k8s.CreateApplicationRequest{
		Name: appName,
		Kind: appKind,
		Spec: extractSpecFromForm(req, fieldTypes),
	}

	_, err := pgh.appSvc.Create(req.Context(), usr.Username, usr.Groups, tenantNS, createReq)
	if err != nil {
		pgh.log.Error("creating app", "tenant", tenantNS, "name", appName, "error", err)
		pgh.renderToast(writer, req, "error", "Failed to create "+appName+": "+err.Error())

		return
	}

	pgh.log.Info("app created", "tenant", tenantNS, "name", appName, "kind", appKind)

	writer.Header().Set("Hx-Redirect", "/tenants/"+tenantNS)
	writer.WriteHeader(http.StatusCreated)
}

func extractFieldTypes(schema *k8s.AppSchema) map[string]string {
	types := map[string]string{}

	if schema == nil || schema.JSONSchema == nil {
		return types
	}

	obj, ok := schema.JSONSchema.(map[string]any)
	if !ok {
		return types
	}

	props, ok := obj["properties"].(map[string]any)
	if !ok {
		return types
	}

	for key, raw := range props {
		prop, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		if t, ok := prop["type"].(string); ok {
			types[key] = t
		}
	}

	return types
}

// DeleteApp handles DELETE to remove an application.
func (pgh *PageHandler) DeleteApp(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)

		return
	}

	tenantNS := req.PathValue("tenant")
	appName := req.PathValue("name")

	err := pgh.appSvc.Delete(req.Context(), usr.Username, usr.Groups, tenantNS, appName)
	if err != nil {
		pgh.log.Error("deleting app", "tenant", tenantNS, "name", appName, "error", err)
		pgh.renderToast(writer, req, "error", "Failed to delete "+appName)

		return
	}

	pgh.log.Info("app deleted", "tenant", tenantNS, "name", appName)

	writer.WriteHeader(http.StatusOK)
}

func (pgh *PageHandler) renderToast(writer http.ResponseWriter, req *http.Request, toastType, msg string) {
	// HX-Reswap: none prevents htmx from swapping the (empty) main response body
	// into the original target. OOB swaps inside the body still apply, so the toast
	// is delivered without blanking the page or removing table rows.
	writer.Header().Set("Hx-Reswap", "none")
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	renderErr := partial.Toast(toastType, msg).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering toast", "error", renderErr)
	}
}

func extractSpecFromForm(req *http.Request, fieldTypes map[string]string) map[string]any {
	spec := map[string]any{}

	for key, values := range req.Form {
		if key == formFieldName || key == formFieldKind {
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

func convertValue(raw, fieldType string) any {
	switch fieldType {
	case "boolean":
		return raw == "true"
	case "integer":
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return raw
		}

		return n
	case "number":
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return raw
		}

		return f
	default:
		return raw
	}
}
