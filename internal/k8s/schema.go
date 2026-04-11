package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

const schemaCacheTTL = 5 * time.Minute

// ErrInvalidAppDef is returned when an ApplicationDefinition cannot be parsed.
var ErrInvalidAppDef = errors.New("invalid ApplicationDefinition")

// ApplicationDefinition GVR.
func appDefGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "cozystack.io",
		Version:  "v1alpha1",
		Resource: "applicationdefinitions",
	}
}

// SchemaService provides operations on Cozystack application schemas
// by reading ApplicationDefinition resources.
type SchemaService struct {
	baseCfg *rest.Config
	cache   map[string]schemaCacheEntry
	mu      sync.RWMutex
}

type schemaCacheEntry struct {
	schema    *AppSchema
	fetchedAt time.Time
}

// NewSchemaService creates a new schema service.
func NewSchemaService(baseCfg *rest.Config) *SchemaService {
	return &SchemaService{
		baseCfg: baseCfg,
		cache:   make(map[string]schemaCacheEntry),
	}
}

// List returns all available application schemas from ApplicationDefinitions.
func (ssv *SchemaService) List(ctx context.Context, username string, groups []string) ([]AppSchema, error) {
	client, err := NewImpersonatingClient(ssv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	defList, listErr := client.Resource(appDefGVR()).List(ctx, metav1.ListOptions{})
	if listErr != nil {
		slog.Debug("failed to list ApplicationDefinitions", "error", listErr)

		return nil, fmt.Errorf("listing ApplicationDefinitions: %w", listErr)
	}

	schemas := make([]AppSchema, 0, len(defList.Items))

	for idx := range defList.Items {
		appSchema := appDefToSchema(&defList.Items[idx])
		if appSchema != nil {
			schemas = append(schemas, *appSchema)
		}
	}

	return schemas, nil
}

// Get returns the full schema for a specific application kind. Lookup is
// tolerant of the CRD naming irregularity (ApplicationDefinition names use
// hyphens, e.g. "minecraft-server", while the Cozystack kind is camelCase
// "MinecraftServer"): if the lowercase short-name lookup fails, we fall
// back to listing every ApplicationDefinition and matching by
// spec.application.kind. Results are cached either way.
func (ssv *SchemaService) Get(ctx context.Context, username string, groups []string, kind string) (*AppSchema, error) {
	ssv.mu.RLock()
	entry, exists := ssv.cache[kind]
	ssv.mu.RUnlock()

	if exists && time.Since(entry.fetchedAt) < schemaCacheTTL {
		return entry.schema, nil
	}

	client, err := NewImpersonatingClient(ssv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	// Fast path: ApplicationDefinition name is the lowercase kind for
	// single-word kinds (postgres, kubernetes, info, etc.).
	defName := toLowerKind(kind)

	obj, getErr := client.Resource(appDefGVR()).Get(ctx, defName, metav1.GetOptions{})
	if getErr == nil {
		if parsed := appDefToSchema(obj); parsed != nil {
			ssv.cacheSet(kind, parsed)

			return parsed, nil
		}
	}

	// Fallback: scan the full list and match by spec.application.kind.
	// Used for camelCase kinds whose ApplicationDefinition name is
	// hyphenated (MinecraftServer -> minecraft-server).
	parsed, findErr := ssv.findByKind(ctx, client, kind)
	if findErr != nil {
		return nil, findErr
	}

	ssv.cacheSet(kind, parsed)

	return parsed, nil
}

// findByKind scans every ApplicationDefinition and returns the one whose
// spec.application.kind matches the requested kind. Slower than a direct
// Get by name but tolerates the hyphenated-vs-camelCase naming mismatch
// in Cozystack's ApplicationDefinition resource names.
func (ssv *SchemaService) findByKind(
	ctx context.Context, client dynamic.Interface, kind string,
) (*AppSchema, error) {
	defList, listErr := client.Resource(appDefGVR()).List(ctx, metav1.ListOptions{})
	if listErr != nil {
		return nil, fmt.Errorf("listing ApplicationDefinitions: %w", listErr)
	}

	for idx := range defList.Items {
		candidate := appDefToSchema(&defList.Items[idx])
		if candidate != nil && candidate.Kind == kind {
			return candidate, nil
		}
	}

	return nil, fmt.Errorf("%w: kind %s", ErrInvalidAppDef, kind)
}

// cacheSet stores a schema under its Cozystack kind key. Takes the write
// lock so concurrent Get calls that resolve the same kind don't thrash
// the cache entry.
func (ssv *SchemaService) cacheSet(kind string, parsed *AppSchema) {
	ssv.mu.Lock()
	ssv.cache[kind] = schemaCacheEntry{schema: parsed, fetchedAt: time.Now()}
	ssv.mu.Unlock()
}

func appDefToSchema(obj *unstructured.Unstructured) *AppSchema {
	// spec.application.kind
	kind := nestedString(obj.Object, "spec", "application", "kind")
	if kind == "" {
		return nil
	}

	plural := nestedString(obj.Object, "spec", "application", "plural")
	displaySingular := nestedString(obj.Object, "spec", "dashboard", "plural")

	if displaySingular == "" {
		displaySingular = nestedString(obj.Object, "spec", "dashboard", "singular")
	}

	if displaySingular == "" {
		displaySingular = kind
	}

	description := nestedString(obj.Object, "spec", "dashboard", "description")
	category := nestedString(obj.Object, "spec", "dashboard", "category")
	icon := nestedString(obj.Object, "spec", "dashboard", "icon")

	// Parse tags
	rawTags, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "dashboard", "tags")

	// Parse openAPISchema JSON string
	schemaStr := nestedString(obj.Object, "spec", "application", "openAPISchema")

	var jsonSchema any
	if schemaStr != "" {
		err := json.Unmarshal([]byte(schemaStr), &jsonSchema)
		if err != nil {
			slog.Debug("failed to parse openAPISchema", "kind", kind, "error", err)

			jsonSchema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
	}

	secretTemplates := extractResourceNameTemplates(obj, "spec", "secrets", "include")
	serviceTemplates := extractResourceNameTemplates(obj, "spec", "services", "include")

	return &AppSchema{
		Kind:             kind,
		Plural:           plural,
		DisplayName:      displaySingular,
		Description:      description,
		Category:         category,
		Icon:             icon,
		Tags:             rawTags,
		JSONSchema:       jsonSchema,
		SecretTemplates:  secretTemplates,
		ServiceTemplates: serviceTemplates,
	}
}

func extractResourceNameTemplates(obj *unstructured.Unstructured, fields ...string) []string {
	includes, found, err := unstructured.NestedSlice(obj.Object, fields...)
	if err != nil || !found {
		return nil
	}

	var templates []string

	for _, inc := range includes {
		incMap, ok := inc.(map[string]any)
		if !ok {
			continue
		}

		names, ok := incMap["resourceNames"].([]any)
		if !ok {
			continue
		}

		for _, name := range names {
			if str, ok := name.(string); ok {
				templates = append(templates, str)
			}
		}
	}

	return templates
}

func nestedString(obj map[string]any, fields ...string) string {
	val, found, err := unstructured.NestedString(obj, fields...)
	if err != nil || !found {
		return ""
	}

	return val
}
