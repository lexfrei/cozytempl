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

// Get returns the full schema for a specific application kind.
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

	// ApplicationDefinition name is the lowercase kind (e.g. "postgres", "kubernetes")
	defName := toLowerKind(kind)

	obj, getErr := client.Resource(appDefGVR()).Get(ctx, defName, metav1.GetOptions{})
	if getErr != nil {
		return nil, fmt.Errorf("getting ApplicationDefinition %s: %w", defName, getErr)
	}

	appSchema := appDefToSchema(obj)
	if appSchema == nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidAppDef, defName)
	}

	ssv.mu.Lock()
	ssv.cache[kind] = schemaCacheEntry{schema: appSchema, fetchedAt: time.Now()}
	ssv.mu.Unlock()

	return appSchema, nil
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
