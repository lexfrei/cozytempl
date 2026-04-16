package handler

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/lexfrei/cozytempl/internal/audit"
	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view/partial"
)

const (
	maxFormBytes = 1 << 20 // 1 MB
	// formFieldName, formFieldKind and formFieldResourceVersion
	// are the only reserved form fields. Everything else is a
	// schema-driven spec field and flows through extractSpecFromForm.
	formFieldName            = "name"
	formFieldKind            = "kind"
	formFieldResourceVersion = "_resource_version"
	sortByName               = "name"
	sortByKind               = "kind"

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
		pgh.recordAudit(req, usr, audit.ActionAppCreate, appName, tenantNS,
			audit.OutcomeDenied, map[string]any{"reason": "invalid_name", "kind": appKind})
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.app.invalidName"))

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
	spec, _, specErr := pgh.buildSpecFromRequest(req, usr, appKind)
	if specErr != nil {
		pgh.reportSpecBuildError(writer, req, usr, audit.ActionAppCreate, appName, appKind, tenantNS, specErr)

		return
	}

	createReq := k8s.CreateApplicationRequest{
		Name: appName,
		Kind: appKind,
		Spec: spec,
	}

	_, err := pgh.appSvc.Create(req.Context(), usr, tenantNS, createReq)
	if err != nil {
		// Log full error context server-side; show the user a generic
		// message so Kubernetes RBAC denials don't leak resource names
		// or tenant metadata of things they can't see.
		pgh.log.Error("creating app", "tenant", tenantNS, "name", appName, "error", err)
		pgh.recordAudit(req, usr, audit.ActionAppCreate, appName, tenantNS,
			audit.OutcomeError, map[string]any{"kind": appKind, "error": err.Error()})
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.app.create", map[string]any{"Name": appName}))

		return
	}

	pgh.log.Info("app created", "tenant", tenantNS, "name", appName, "kind", appKind)
	pgh.recordAudit(req, usr, audit.ActionAppCreate, appName, tenantNS,
		audit.OutcomeSuccess, map[string]any{"kind": appKind})
	pgh.emitSuccessToast(writer, req, pgh.t(req, "toast.app.created", map[string]any{"Name": appName}))
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
	snap, specErr := pgh.appSvc.GetSpecSnapshot(req.Context(), usr, tenantNS, appName)
	if specErr != nil {
		pgh.log.Error("loading app for update", "tenant", tenantNS, "name", appName, "error", specErr)
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.app.load", map[string]any{"Name": appName}))

		return
	}

	kind := snap.Kind

	// buildSpecFromRequest returns (spec, replace, err) as a
	// single unit so the spec source and the merge-vs-replace
	// policy can never disagree. Every branch inside sets the
	// replace bool explicitly: yaml tab → true, legacy
	// non-empty yaml fallback → true, form → false.
	newSpec, replaceSpec, specBuildErr := pgh.buildSpecFromRequest(req, usr, kind)
	if specBuildErr != nil {
		pgh.reportSpecBuildError(writer, req, usr, audit.ActionAppUpdate, appName, kind, tenantNS, specBuildErr)

		return
	}
	// The edit form echoes the resourceVersion it observed as a
	// hidden input so the Update can pin optimistic-lock semantics.
	// An empty value falls back to last-write-wins behaviour for
	// any caller who hasn't been migrated yet.
	// Safe to call req.FormValue without an explicit MaxBytesReader
	// check here: the outer UpdateApp handler wrapped req.Body and
	// already called ParseForm, so the body size cap is in effect.
	resourceVersion := req.FormValue(formFieldResourceVersion) //nolint:gosec // body already capped by caller

	_, err := pgh.appSvc.Update(req.Context(), usr, tenantNS, appName,
		k8s.UpdateApplicationRequest{
			Spec:            newSpec,
			ResourceVersion: resourceVersion,
			ReplaceSpec:     replaceSpec,
		})
	if err != nil {
		if errors.Is(err, k8s.ErrConflict) {
			pgh.log.Info("conflict updating app", "tenant", tenantNS, "name", appName)
			pgh.recordAudit(req, usr, audit.ActionAppUpdate, appName, tenantNS,
				audit.OutcomeError, map[string]any{"reason": "conflict", "kind": kind})
			pgh.renderErrorToast(writer, req, pgh.t(req, "error.app.conflict", map[string]any{"Name": appName}))

			return
		}

		pgh.log.Error("updating app", "tenant", tenantNS, "name", appName, "error", err)
		pgh.recordAudit(req, usr, audit.ActionAppUpdate, appName, tenantNS,
			audit.OutcomeError, map[string]any{"kind": kind, "error": err.Error()})
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.app.update", map[string]any{"Name": appName}))

		return
	}

	pgh.log.Info("app updated", "tenant", tenantNS, "name", appName, "kind", kind)
	pgh.recordAudit(req, usr, audit.ActionAppUpdate, appName, tenantNS,
		audit.OutcomeSuccess, map[string]any{"kind": kind, "keys": len(newSpec)})
	pgh.emitSuccessToast(writer, req, pgh.t(req, "toast.app.updated", map[string]any{"Name": appName}))
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

	err := pgh.appSvc.Delete(req.Context(), usr, tenantNS, appName)
	if err != nil {
		pgh.log.Error("deleting app", "tenant", tenantNS, "name", appName, "error", err)
		pgh.recordAudit(req, usr, audit.ActionAppDelete, appName, tenantNS,
			audit.OutcomeError, map[string]any{"error": err.Error()})
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.app.delete", map[string]any{"Name": appName}))

		return
	}

	pgh.log.Info("app deleted", "tenant", tenantNS, "name", appName)
	pgh.recordAudit(req, usr, audit.ActionAppDelete, appName, tenantNS,
		audit.OutcomeSuccess, nil)
	// Delete is hx-swap="delete swap:500ms" so the triggering row is
	// removed client-side regardless of the response body. Sending
	// *only* an OOB toast keeps the row-delete animation intact while
	// still giving the user explicit confirmation.
	pgh.emitSuccessToast(writer, req, pgh.t(req, "toast.app.deleted", map[string]any{"Name": appName}))
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

// formFieldSpecYAML is the textarea name the create / update
// modal submits when the user chose the YAML tab. The field is
// always in the DOM (hidden when the form tab is active) so
// request parsing is uniform; whether to use its value is a
// non-empty check on the server.
const formFieldSpecYAML = "spec_yaml"

// formFieldTabMode is the radio group name that drives the
// Form / YAML tab switch in the UI. Sent by the browser with
// every submit; server-side it is the primary signal that
// chooses between form-mode and yaml-mode spec extraction.
const formFieldTabMode = "_tabmode"

// tabModeYAML is the _tabmode value the YAML tab emits.
// Centralised so the spec-source selection and the
// merge-vs-replace selection cannot drift.
const tabModeYAML = "yaml"

// ErrInvalidYAMLSpec is returned by buildSpecFromRequest when
// the user-supplied YAML in spec_yaml fails to parse. Exposed
// as a sentinel so handlers can show the "invalid spec" toast
// without misclassifying unrelated schema-fetch errors (which
// have their own error.schema.load copy).
var ErrInvalidYAMLSpec = errors.New("invalid yaml spec")

// ErrEmptyYAMLSpec is returned when the user is explicitly on
// the YAML tab (_tabmode=yaml) but left the textarea empty.
// Falling through to the form pane in that case would apply
// hidden form-field values the user never saw on screen —
// classic silent-data-loss trap, so the handler prefers a
// loud error over a surprise write.
var ErrEmptyYAMLSpec = errors.New("empty yaml spec on yaml tab")

// isReservedFormField returns true for form fields the spec
// extractor must never lift into the CRD spec map. The
// reserved set includes the name/kind/resourceVersion
// handshake fields, the YAML textarea (used as the spec source
// in YAML mode), and the tab-switcher radio group (pure UI
// state). Hidden from the spec writer so a future addition to
// the form only has to add its name here, not go hunt every
// call site.
func isReservedFormField(key string) bool {
	switch key {
	case formFieldName, formFieldKind, formFieldResourceVersion,
		formFieldSpecYAML, formFieldTabMode:
		return true
	}

	return false
}

// reportSpecBuildError surfaces a buildSpecFromRequest failure
// to the user with the right copy + audit shape. Invalid YAML
// the user typed is distinguished from a schema-fetch error —
// the first is "fix the YAML", the second is "cluster problem"
// and the two should not share a toast string.
func (pgh *PageHandler) reportSpecBuildError(
	writer http.ResponseWriter, req *http.Request, usr *auth.UserContext,
	action audit.Action, appName, kind, tenantNS string, err error,
) {
	pgh.log.Warn("building spec for mutation",
		"action", action, "tenant", tenantNS, "name", appName, "kind", kind, "error", err)

	if errors.Is(err, ErrInvalidYAMLSpec) || errors.Is(err, ErrEmptyYAMLSpec) {
		reason := "invalid_yaml"
		if errors.Is(err, ErrEmptyYAMLSpec) {
			reason = "empty_yaml"
		}

		pgh.recordAudit(req, usr, action, appName, tenantNS,
			audit.OutcomeDenied,
			map[string]any{"kind": kind, "reason": reason, "error": err.Error()})
		pgh.renderErrorToast(writer, req, pgh.t(req, "error.app.invalidSpec"))

		return
	}

	pgh.recordAudit(req, usr, action, appName, tenantNS,
		audit.OutcomeError,
		map[string]any{"kind": kind, "reason": "spec_build_failed", "error": err.Error()})
	pgh.renderErrorToast(writer, req, pgh.t(req, "error.schema.load", map[string]any{"Kind": kind}))
}

// buildSpecFromRequest picks a spec source — form fields or a
// YAML payload — and returns the resulting map plus the
// merge-vs-replace policy that matches that source. Folding the
// two into a single return value keeps them from drifting: a
// spec parsed from YAML always travels with replace=true, a
// spec extracted from the form fields always with replace=false.
// Earlier revisions computed replace off a second read of
// _tabmode at the caller, which silently disagreed with the
// source-selection branch in the legacy fallback path.
//
// Fetches the schema on the form-mode branch so the
// kind-specific type coercion (booleans, integers) in
// extractSpecFromForm stays accurate; the YAML branches skip
// schema entirely because sigs.k8s.io/yaml already decodes
// native types.
//
// The UI radio _tabmode is the source of truth: the user
// explicitly chose YAML if the value is "yaml", otherwise the
// form pane wins. Older clients or direct API consumers that
// never set _tabmode fall back to "yaml wins if non-empty" so
// cozytempl's /api/.../apps POST with a raw spec_yaml still
// works without the radio in the payload. That fallback path
// also maps to replace=true — a user or API client sending raw
// YAML has kubectl-edit expectations, never deep-merge.
//
//nolint:nonamedreturns // names document the (spec, replace, err) shape so callers read the policy alongside the value.
func (pgh *PageHandler) buildSpecFromRequest(
	req *http.Request, usr *auth.UserContext, appKind string,
) (spec map[string]any, replace bool, err error) {
	tabMode := req.FormValue(formFieldTabMode)
	yamlRaw := strings.TrimSpace(req.FormValue(formFieldSpecYAML))

	if tabMode == tabModeYAML {
		return parseYAMLSpecOrEmptyError(yamlRaw)
	}

	// Legacy fallback for API clients / older browsers that
	// don't send _tabmode but do paste into spec_yaml: a
	// non-empty textarea still wins AND carries replace
	// semantics — a caller who pastes raw YAML expects
	// kubectl-edit behaviour, and silently deep-merging that
	// into the existing spec is the exact UX trap the radio
	// was introduced to avoid. The empty-parsed-spec guard
	// runs on this path too: a legacy client that pastes
	// "{}" or "null" must not wipe cluster state any more
	// than a browser client on the YAML tab.
	if tabMode == "" && yamlRaw != "" {
		return parseYAMLSpecOrEmptyError(yamlRaw)
	}

	schema, err := pgh.schemaSvc.Get(req.Context(), usr, appKind)
	if err != nil {
		return nil, false, fmt.Errorf("fetching schema: %w", err)
	}

	return extractSpecFromForm(req, extractFieldTypes(schema)), false, nil
}

// parseYAMLSpecOrEmptyError is the shared guard for both YAML
// code paths (explicit _tabmode=yaml and the legacy
// non-empty-textarea fallback). It rejects an empty textarea
// AND a textarea whose content parses to an empty map: "{}",
// "null", "~", and comment-only inputs all parse cleanly to
// a zero-key map[string]any under sigs.k8s.io/yaml. Without
// this guard a user with replace semantics who accidentally
// typed "{}" or a stray "# comment" would silently wipe every
// key from the cluster spec — the exact surprise the YAML =
// replace invariant was supposed to make impossible.
//
//nolint:nonamedreturns // names mirror buildSpecFromRequest so the shared (spec, replace, err) shape is obvious at call sites.
func parseYAMLSpecOrEmptyError(raw string) (spec map[string]any, replace bool, err error) {
	if raw == "" {
		return nil, false, ErrEmptyYAMLSpec
	}

	parsed, parseErr := parseSpecYAML(raw)
	if parseErr != nil {
		return nil, false, parseErr
	}

	if len(parsed) == 0 {
		return nil, false, ErrEmptyYAMLSpec
	}

	return parsed, true, nil
}

// parseSpecYAML unmarshals the textarea content into the
// spec map. sigs.k8s.io/yaml is preferred over gopkg.in/yaml.v3
// because the former already goes through encoding/json —
// integers, floats, booleans end up as native types matching
// what the Kubernetes apiserver accepts, without the
// float64/int coercion dance yaml.v3 requires.
func parseSpecYAML(raw string) (map[string]any, error) {
	spec := map[string]any{}

	err := yaml.Unmarshal([]byte(raw), &spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidYAMLSpec, err)
	}

	return spec, nil
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
		if isReservedFormField(key) {
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
// ["enabled"]. A non-dotted key assigns at the top level.
//
// If an intermediate key already holds a non-map value, the nested
// assignment is silently skipped rather than overwriting the existing
// scalar with a fresh map. Rationale: the two collision paths are
// "overwrite the scalar with {new: val}" or "drop the nested write".
// The former destroys data the form previously wrote; the latter
// preserves it and the loss is limited to the one dotted key. Neither
// is great, but silent-preserve beats silent-destroy because the
// schema-driven form never generates colliding keys under normal
// operation — a collision is either a malformed POST or a schema bug,
// and keeping the earlier scalar leaves more of the original intent
// intact.
func setNestedSpec(spec map[string]any, key string, value any) {
	parts := strings.Split(key, ".")

	cur := spec

	for idx := range len(parts) - 1 {
		part := parts[idx]

		existing, present := cur[part]
		if !present {
			fresh := map[string]any{}
			cur[part] = fresh
			cur = fresh

			continue
		}

		child, ok := existing.(map[string]any)
		if !ok {
			// Intermediate key is already a scalar; a form with a
			// colliding nested key is either malformed input or a
			// schema bug. Drop this dot-path write rather than
			// silently discarding the scalar.
			return
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
