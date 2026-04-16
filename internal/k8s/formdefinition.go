package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
)

// ErrInvalidFormDefinition is returned when a FormDefinition
// object cannot be parsed into the typed struct below.
var ErrInvalidFormDefinition = errors.New("invalid FormDefinition")

// formDefGVR mirrors the CRD shipped at
// deploy/helm/cozytempl/crds/formdefinition.yaml. Cluster-
// scoped, v1alpha1 so we can iterate the shape without a
// migration burden — a future v1 promotion would go through
// a conversion webhook or a parallel-version read.
func formDefGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "cozytempl.cozystack.io",
		Version:  "v1alpha1",
		Resource: "formdefinitions",
	}
}

// FormDefinition is the typed projection of the CRD. It
// describes UI-level overlays on top of the generic
// openAPISchema-driven form generator: per-field label, hint,
// placeholder, ordering, and visibility. Everything is
// optional; an absent FormDefinition means the form renders
// straight from the schema, which is the pre-existing
// behaviour.
//
// The type is intentionally narrow on purpose. Widget types,
// conditional visibility, enum labelling, default injection —
// all can be added in later versions without breaking the
// v1alpha1 contract, which is spec.kind + spec.fields[] only.
type FormDefinition struct {
	// Kind is the ApplicationDefinition kind this FormDefinition
	// overlays. Matching is exact — "Postgres" does not match a
	// FormDefinition with kind "postgres". Operators who want
	// wildcard / prefix matching should ship multiple
	// FormDefinitions or roll their own controller.
	Kind string `json:"kind"`

	// Fields carries the per-field overrides. The path syntax
	// mirrors the form walker in internal/view/fragment/
	// schema_fields.templ: a bare key for top-level fields
	// ("replicas"), and dot-separated for nested fields one
	// level deep ("backup.enabled"). The walker only descends
	// to maxNestedDepth (currently 2), so deeper paths in a
	// FormDefinition have no effect.
	Fields []FormFieldOverride `json:"fields,omitempty"`
}

// FormFieldOverride is the payload for a single field entry.
// All non-Path fields are optional; a zero value means "leave
// the schema-derived value in place". The merge algorithm
// layers these onto the generated form, it does not replace
// the schema.
type FormFieldOverride struct {
	// Path is the dot-path that addresses a field in the
	// schema-walked tree. Required.
	Path string `json:"path"`

	// Label replaces the schema's title= for this field
	// when non-empty. An empty Label leaves the schema title
	// intact.
	Label string `json:"label,omitempty"`

	// Hint replaces the schema's description= for this field
	// when non-empty. An empty Hint leaves the schema
	// description intact.
	Hint string `json:"hint,omitempty"`

	// Placeholder sets the HTML placeholder= on the rendered
	// input when non-empty. The base schema does not emit a
	// placeholder today, so an empty value means "no
	// placeholder attribute".
	Placeholder string `json:"placeholder,omitempty"`

	// Order rewrites the render order for grouped fields. A
	// lower number renders earlier. Fields without an Order
	// fall back to the schema's natural (alphabetical) sort
	// and render after all ordered fields. Zero is a valid
	// explicit order value.
	//
	// Pointer so "unset" and "set to 0" are distinguishable;
	// an Order=nil field sorts differently from an Order=0
	// field.
	Order *int `json:"order,omitempty"`

	// Hidden removes the field from the rendered form
	// entirely. Pre-existing values survive through edit
	// because the raw spec snapshot is still merged by the
	// Apply-to-Form path; the UI just does not expose the
	// knob.
	Hidden bool `json:"hidden,omitempty"`
}

// FormDefinitionService lists FormDefinition resources using
// the same impersonated-dynamic-client stack SchemaService
// uses. Cluster-scoped reads go through the caller's
// Kubernetes identity so the RBAC the operator configures on
// FormDefinitions is what actually decides what the user sees.
//
// The service is deliberately cacheless. An earlier revision
// shared a single map[kind]overrides across every caller,
// which meant the first viewer to trigger a refresh filled
// the cache and every subsequent caller read that viewer's
// RBAC-filtered list — a cross-user leak. Per-render list
// calls are cheap (FormDefinitions are small, few, and the
// apiserver's etcd Get is fast) and keep the "user identity
// drives visibility" contract honest. If telemetry later
// shows the list call is a real bottleneck, the fix is a
// per-user cache keyed on UserContext.Username, NOT a return
// to the shared bucket.
type FormDefinitionService struct {
	baseCfg *rest.Config
	mode    config.AuthMode
}

// NewFormDefinitionService constructs the service.
func NewFormDefinitionService(baseCfg *rest.Config, mode config.AuthMode) *FormDefinitionService {
	return &FormDefinitionService{
		baseCfg: baseCfg,
		mode:    mode,
	}
}

// GetOverridesForKind returns the merged FormFieldOverride
// slice for a given ApplicationDefinition kind. Multiple
// FormDefinitions pointing at the same kind are folded in
// name order; the last write wins for any path conflict.
// Operators who layer "base" + "tenant-a" overlays can rely
// on alphabetical ordering to pick the winner.
//
// Returns nil (not an error) when no FormDefinition targets
// the kind, when the CRD is not installed, or when RBAC
// forbids the list. An absent overlay is the pre-existing
// behaviour and callers fall through to schema-only rendering
// — a render path must not break because the FormDefinition
// machinery failed.
func (fds *FormDefinitionService) GetOverridesForKind(
	ctx context.Context, usr *auth.UserContext, kind string,
) ([]FormFieldOverride, error) {
	if kind == "" {
		return nil, nil
	}

	client, err := NewUserClient(fds.baseCfg, usr, fds.mode)
	if err != nil {
		return nil, err
	}

	defs, listErr := listFormDefinitions(ctx, client)
	if listErr != nil {
		slog.Debug("listing FormDefinitions failed; falling back to schema-only render",
			"kind", kind, "error", listErr)

		return nil, nil
	}

	var merged []FormFieldOverride

	for _, def := range defs {
		if def.Kind != kind {
			continue
		}

		merged = append(merged, def.Fields...)
	}

	return merged, nil
}

// List returns every FormDefinition visible to the user.
// Exposed for diagnostic surfaces (e.g. an admin page that
// wants to enumerate overlays) and for tests.
func (fds *FormDefinitionService) List(
	ctx context.Context, usr *auth.UserContext,
) ([]FormDefinition, error) {
	client, err := NewUserClient(fds.baseCfg, usr, fds.mode)
	if err != nil {
		return nil, err
	}

	return listFormDefinitions(ctx, client)
}

// listFormDefinitions is the raw fetch-and-parse helper,
// split out so the service can call it without holding the
// cache lock and the test layer can drive it directly.
func listFormDefinitions(ctx context.Context, client dynamic.Interface) ([]FormDefinition, error) {
	list, err := client.Resource(formDefGVR()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing FormDefinitions: %w", err)
	}

	return formDefinitionsFromList(list.Items), nil
}

// formDefinitionsFromList is the pure sort-and-parse core of
// listFormDefinitions, split out so merge-by-name precedence
// can be tested without a fake dynamic client. Invariants:
//
//   - Items are sorted by metadata.name before parsing so the
//     returned slice has a stable, operator-visible order that
//     GetOverridesForKind then folds into a last-write-wins
//     overlay.
//   - Malformed entries (missing spec, missing spec.kind) are
//     logged and skipped; one bad FormDefinition does not hide
//     the rest.
func formDefinitionsFromList(items []unstructured.Unstructured) []FormDefinition {
	defs := make([]FormDefinition, 0, len(items))

	names := make([]string, 0, len(items))
	byName := map[string]*unstructured.Unstructured{}

	for i := range items {
		obj := &items[i]
		name := obj.GetName()
		names = append(names, name)
		byName[name] = obj
	}

	// Sort by name so a "last-write-wins" merge between two
	// FormDefinitions targeting the same kind has a stable,
	// operator-visible precedence.
	sort.Strings(names)

	for _, name := range names {
		def, parseErr := parseFormDefinition(byName[name])
		if parseErr != nil {
			slog.Warn("skipping malformed FormDefinition",
				"name", name, "error", parseErr)

			continue
		}

		defs = append(defs, *def)
	}

	return defs
}

func parseFormDefinition(obj *unstructured.Unstructured) (*FormDefinition, error) {
	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidFormDefinition, obj.GetName(), err)
	}

	if !found {
		return nil, fmt.Errorf("%w: %s: missing spec", ErrInvalidFormDefinition, obj.GetName())
	}

	kind, _ := spec["kind"].(string)
	if kind == "" {
		return nil, fmt.Errorf("%w: %s: missing spec.kind", ErrInvalidFormDefinition, obj.GetName())
	}

	def := &FormDefinition{Kind: kind}

	rawFields, _ := spec["fields"].([]any)
	for _, rawEntry := range rawFields {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			continue
		}

		override := parseFieldOverride(entry)
		if override == nil {
			continue
		}

		def.Fields = append(def.Fields, *override)
	}

	return def, nil
}

func parseFieldOverride(entry map[string]any) *FormFieldOverride {
	path, _ := entry["path"].(string)
	if path == "" {
		return nil
	}

	override := FormFieldOverride{Path: path}

	if v, ok := entry["label"].(string); ok {
		override.Label = v
	}

	if v, ok := entry["hint"].(string); ok {
		override.Hint = v
	}

	if v, ok := entry["placeholder"].(string); ok {
		override.Placeholder = v
	}

	if v, ok := entry["hidden"].(bool); ok {
		override.Hidden = v
	}

	// Unstructured decodes YAML integers into int64 via
	// sigs.k8s.io/yaml; float64 is also possible if the user
	// wrote "5.0". Accept both and pin to int so the render
	// layer sorts on a concrete type.
	switch raw := entry["order"].(type) {
	case int64:
		n := int(raw)
		override.Order = &n
	case float64:
		n := int(raw)
		override.Order = &n
	case int:
		n := raw
		override.Order = &n
	}

	return &override
}
