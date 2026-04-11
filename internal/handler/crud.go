package handler

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

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

	// maxAppNameLength mirrors Helm's 53-character cap on release
	// names. Cozystack applications are materialised as HelmReleases,
	// so anything longer is rejected downstream with an opaque error.
	// We catch it here and give the user a precise message instead.
	maxAppNameLength = 53
)

// appNameRegex is the DNS-1123 label regex that Kubernetes enforces on
// object names. We validate client-side (via the <input pattern=...>)
// and again server-side so that non-browser clients don't slip past.
var appNameRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// validAppName reports whether s is a valid application name. Matches
// the tenant.templ create form's client-side pattern so the two
// validators never diverge.
func validAppName(s string) bool {
	if s == "" || len(s) > maxAppNameLength {
		return false
	}

	return appNameRegex.MatchString(s)
}

// CreateApp handles POST to create a new application.
func (pgh *PageHandler) CreateApp(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
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

	if !validAppName(appName) {
		pgh.renderErrorToast(writer, req,
			"Invalid name: lowercase letters, digits and hyphens only, must "+
				"start and end with a letter or digit, max 53 characters.")

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
		pgh.renderErrorToast(writer, req, "Failed to load schema for "+appKind)

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
		// Log full error context server-side; show the user a generic
		// message so Kubernetes RBAC denials don't leak resource names
		// or tenant metadata of things they can't see.
		pgh.log.Error("creating app", "tenant", tenantNS, "name", appName, "error", err)
		pgh.renderErrorToast(writer, req,
			"Failed to create "+appName+". Check that the name is unique and you have permission.")

		return
	}

	pgh.log.Info("app created", "tenant", tenantNS, "name", appName, "kind", appKind)
	pgh.emitSuccessToast(writer, req, "Application created: "+appName)
	// Re-render the tenant page so the new row appears in the app table
	// and the create modal closes (it's inside the tenant template, so
	// the swap replaces it with a fresh, closed copy).
	pgh.TenantPage(writer, req)
}

// extractFieldTypes walks the JSON schema and returns a map from
// dot-path field key to JSON-schema type. Mirrors the recursive
// walker in view/fragment/schema_fields.templ so the coercion in
// convertValue() matches the form field rendered to the user —
// without it, nested integer / boolean fields would submit as raw
// strings and the downstream CRD would reject them.
func extractFieldTypes(schema *k8s.AppSchema) map[string]string {
	types := map[string]string{}

	if schema == nil || schema.JSONSchema == nil {
		return types
	}

	obj, ok := schema.JSONSchema.(map[string]any)
	if !ok {
		return types
	}

	walkFieldTypes(obj, "", 0, types)

	return types
}

// maxFieldTypeDepth matches the schema-field walker in the view
// layer. Kept in the handler package to avoid a cross-package import
// just for a constant.
const maxFieldTypeDepth = 2

// walkFieldTypes recursively flattens a JSON Schema `properties` map
// into dot-path → type entries. Object children are descended into
// up to maxFieldTypeDepth; arrays and deeper objects are skipped,
// which matches the form renderer so every form field has a
// matching entry in the map.
func walkFieldTypes(obj map[string]any, prefix string, depth int, out map[string]string) {
	rawProps, ok := obj["properties"].(map[string]any)
	if !ok {
		return
	}

	for key, raw := range rawProps {
		prop, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}

		fieldType, _ := prop["type"].(string)

		if fieldType == "object" {
			if depth >= maxFieldTypeDepth-1 {
				continue
			}

			walkFieldTypes(prop, fullKey, depth+1, out)

			continue
		}

		if fieldType == "array" {
			continue
		}

		out[fullKey] = fieldType
	}
}

// UpdateApp handles PUT /tenants/{tenant}/apps/{name} — merges form
// fields into the existing application's spec. The request body is the
// same schema-driven form used by create, minus the name + kind fields
// which are fixed at creation time.
func (pgh *PageHandler) UpdateApp(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenantNS := req.PathValue("tenant")
	appName := req.PathValue("name")

	req.Body = http.MaxBytesReader(writer, req.Body, maxFormBytes)

	parseErr := req.ParseForm()
	if parseErr != nil {
		http.Error(writer, "bad request", http.StatusBadRequest)

		return
	}

	pgh.doUpdateApp(writer, req, usr, tenantNS, appName)
}

// doUpdateApp is the UpdateApp work after form parsing. Split out so the
// public handler stays under the function-length linter limit and the
// error-branch plumbing reads cleanly.
func (pgh *PageHandler) doUpdateApp(
	writer http.ResponseWriter, req *http.Request, usr *auth.UserContext, tenantNS, appName string,
) {
	// Kind is looked up via the service, not supplied by the client, so
	// the user cannot change it mid-edit.
	_, kind, specErr := pgh.appSvc.GetSpec(req.Context(), usr.Username, usr.Groups, tenantNS, appName)
	if specErr != nil {
		pgh.log.Error("loading app for update", "tenant", tenantNS, "name", appName, "error", specErr)
		pgh.renderErrorToast(writer, req,
			"Failed to load "+appName+". It may not exist or you lack permission.")

		return
	}

	schema, schemaErr := pgh.schemaSvc.Get(req.Context(), usr.Username, usr.Groups, kind)
	if schemaErr != nil {
		pgh.log.Error("loading schema for update", "kind", kind, "error", schemaErr)
		pgh.renderErrorToast(writer, req, "Failed to load schema for "+kind)

		return
	}

	newSpec := extractSpecFromForm(req, extractFieldTypes(schema))

	_, err := pgh.appSvc.Update(req.Context(), usr.Username, usr.Groups, tenantNS, appName,
		k8s.UpdateApplicationRequest{Spec: newSpec})
	if err != nil {
		pgh.log.Error("updating app", "tenant", tenantNS, "name", appName, "error", err)
		pgh.renderErrorToast(writer, req, "Failed to update "+appName+". Check that you have permission.")

		return
	}

	pgh.log.Info("app updated", "tenant", tenantNS, "name", appName, "kind", kind)
	pgh.emitSuccessToast(writer, req, "Application updated: "+appName)
	// Re-render so the changed spec values show up immediately in the
	// app row and the edit slot collapses.
	pgh.TenantPage(writer, req)
}

// DeleteApp handles DELETE to remove an application.
func (pgh *PageHandler) DeleteApp(writer http.ResponseWriter, req *http.Request) {
	usr := pgh.requireUser(writer, req)
	if usr == nil {
		return
	}

	tenantNS := req.PathValue("tenant")
	appName := req.PathValue("name")

	err := pgh.appSvc.Delete(req.Context(), usr.Username, usr.Groups, tenantNS, appName)
	if err != nil {
		pgh.log.Error("deleting app", "tenant", tenantNS, "name", appName, "error", err)
		pgh.renderErrorToast(writer, req, "Failed to delete "+appName)

		return
	}

	pgh.log.Info("app deleted", "tenant", tenantNS, "name", appName)
	// Delete is hx-swap="delete swap:500ms" so the triggering row is
	// removed client-side regardless of the response body. Sending
	// *only* an OOB toast keeps the row-delete animation intact while
	// still giving the user explicit confirmation.
	pgh.emitSuccessToast(writer, req, "Application deleted: "+appName)
}

// renderErrorToast writes an OOB error toast without touching the htmx target.
// HX-Reswap: none keeps the original target (main-content, tr, etc.) intact so
// a failed mutation doesn't blank the page or remove a live row.
func (pgh *PageHandler) renderErrorToast(writer http.ResponseWriter, req *http.Request, msg string) {
	writer.Header().Set("Hx-Reswap", "none")
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	renderErr := partial.Toast("error", msg).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering toast", "error", renderErr)
	}
}

// emitSuccessToast writes an OOB success toast directly to the response
// body. The caller typically follows this with a page-render call so
// the response carries both the toast (OOB swap target =
// #toast-container) and the fresh main content in a single round-trip.
//
// This is intentionally NOT a drop-in replacement for renderErrorToast:
// error toasts use Hx-Reswap: none because the failing mutation should
// leave the current DOM untouched, whereas success toasts go out
// alongside a fresh content swap that the caller produces.
func (pgh *PageHandler) emitSuccessToast(writer http.ResponseWriter, req *http.Request, msg string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")

	renderErr := partial.Toast("success", msg).Render(req.Context(), writer)
	if renderErr != nil {
		pgh.log.Error("rendering success toast", "error", renderErr)
	}
}

// extractSpecFromForm pulls known schema fields out of the submitted form.
// Dot-path keys ("backup.enabled", "backup.schedule") are un-flattened
// into nested maps so the CRD sees {backup: {enabled: true, schedule:
// "..."}} instead of two string keys with dots in them.
// Always returns a non-nil map so downstream CRD validation that expects a
// spec object succeeds even when the user submits only name + kind.
func extractSpecFromForm(req *http.Request, fieldTypes map[string]string) map[string]any {
	spec := map[string]any{}

	for key, values := range req.Form {
		if key == formFieldName || key == formFieldKind {
			continue
		}

		if len(values) == 0 || values[0] == "" {
			continue
		}

		setNestedSpec(spec, key, convertValue(values[0], fieldTypes[key]))
	}

	return spec
}

// setNestedSpec assigns a value at a dot-path inside a map, creating
// intermediate sub-maps as needed. "backup.enabled" → spec["backup"]
// ["enabled"]. A non-dotted key assigns at the top level. If an
// intermediate key already holds a non-map value, setNestedSpec leaves
// it alone — the form cannot silently overwrite a scalar with a map.
func setNestedSpec(spec map[string]any, key string, value any) {
	parts := strings.Split(key, ".")

	cur := spec

	for idx := range len(parts) - 1 {
		part := parts[idx]

		child, ok := cur[part].(map[string]any)
		if !ok {
			child = map[string]any{}
			cur[part] = child
		}

		cur = child
	}

	cur[parts[len(parts)-1]] = value
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
