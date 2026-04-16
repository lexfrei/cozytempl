package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

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

const formDefCacheTTL = 5 * time.Minute

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

// FormDefinitionService lists and caches FormDefinition
// resources using the same impersonated-dynamic-client stack
// SchemaService uses. Cluster-scoped reads go through the
// caller's Kubernetes identity so operators can scope
// visibility with RBAC just like they do for
// ApplicationDefinitions.
type FormDefinitionService struct {
	baseCfg *restConfigLike
	mode    config.AuthMode
	cache   map[string][]FormFieldOverride
	mu      sync.RWMutex
	fetched time.Time
}

// restConfigLike is the narrow interface the service needs
// from rest.Config: enough to build a user client through
// NewUserClient. Kept local so the file does not need to
// import "k8s.io/client-go/rest" directly for the typed
// signature; NewUserClient already pulls the real type in
// through the same package.
type restConfigLike = rest.Config

// NewFormDefinitionService constructs the service.
func NewFormDefinitionService(baseCfg *rest.Config, mode config.AuthMode) *FormDefinitionService {
	return &FormDefinitionService{
		baseCfg: baseCfg,
		mode:    mode,
		cache:   map[string][]FormFieldOverride{},
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
// the kind — an absent CRD is the pre-existing behaviour and
// callers fall through to schema-only rendering.
//
// The cache is a single all-kinds slice refreshed every
// formDefCacheTTL; we refetch every kind's list on the same
// cadence so a newly-applied FormDefinition becomes visible
// within the TTL without a cozytempl restart.
func (fds *FormDefinitionService) GetOverridesForKind(
	ctx context.Context, usr *auth.UserContext, kind string,
) ([]FormFieldOverride, error) {
	if kind == "" {
		return nil, nil
	}

	fds.mu.RLock()

	fresh := time.Since(fds.fetched) < formDefCacheTTL
	cached, have := fds.cache[kind]

	fds.mu.RUnlock()

	if fresh && have {
		return cached, nil
	}

	return fds.refreshAndLookup(ctx, usr, kind)
}

// List returns every FormDefinition visible to the user.
// Exposed for diagnostic surfaces (e.g. an admin page that
// wants to enumerate overlays) and for tests; the render
// path uses GetOverridesForKind, which is cache-backed.
func (fds *FormDefinitionService) List(
	ctx context.Context, usr *auth.UserContext,
) ([]FormDefinition, error) {
	client, err := NewUserClient(fds.baseCfg, usr, fds.mode)
	if err != nil {
		return nil, err
	}

	return listFormDefinitions(ctx, client)
}

func (fds *FormDefinitionService) refreshAndLookup(
	ctx context.Context, usr *auth.UserContext, kind string,
) ([]FormFieldOverride, error) {
	client, err := NewUserClient(fds.baseCfg, usr, fds.mode)
	if err != nil {
		return nil, err
	}

	defs, err := listFormDefinitions(ctx, client)
	if err != nil {
		// Cache miss + fetch error: return nil, no error —
		// a render path failing because the FormDefinition
		// CRD is not installed, or because RBAC forbids the
		// list, should not break the form. Log and fall
		// through to schema-only behaviour.
		slog.Debug("listing FormDefinitions failed; falling back to schema-only render",
			"kind", kind, "error", err)

		return nil, nil
	}

	byKind := map[string][]FormFieldOverride{}

	for _, def := range defs {
		byKind[def.Kind] = append(byKind[def.Kind], def.Fields...)
	}

	fds.mu.Lock()
	fds.cache = byKind
	fds.fetched = time.Now()
	fds.mu.Unlock()

	return byKind[kind], nil
}

// listFormDefinitions is the raw fetch-and-parse helper,
// split out so the service can call it without holding the
// cache lock and the test layer can drive it directly.
func listFormDefinitions(ctx context.Context, client dynamic.Interface) ([]FormDefinition, error) {
	list, err := client.Resource(formDefGVR()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing FormDefinitions: %w", err)
	}

	defs := make([]FormDefinition, 0, len(list.Items))

	names := make([]string, 0, len(list.Items))
	byName := map[string]*unstructured.Unstructured{}

	for i := range list.Items {
		obj := &list.Items[i]
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

	return defs, nil
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
