package k8s

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

const (
	schemaCacheTTL     = 5 * time.Minute
	configMapsResource = "configmaps"
)

// SchemaService provides operations on Cozystack application schemas.
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

// List returns all available application schemas by discovering ConfigMaps.
func (ssv *SchemaService) List(ctx context.Context, username string, groups []string) ([]AppSchema, error) {
	client, err := NewImpersonatingClient(ssv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	cmGVR := NamespaceGVR()
	cmGVR.Resource = configMapsResource

	cmList, listErr := client.Resource(cmGVR).Namespace("cozy-system").List(ctx, metav1.ListOptions{
		LabelSelector: "apps.cozystack.io/schema=true",
	})
	if listErr != nil {
		slog.Debug("failed to list schema configmaps, using defaults", "error", listErr)

		return defaultSchemaList(), nil
	}

	schemas := make([]AppSchema, 0, len(cmList.Items))

	for idx := range cmList.Items {
		schema := schemaFromConfigMap(&cmList.Items[idx])
		if schema != nil {
			schemas = append(schemas, *schema)
		}
	}

	if len(schemas) == 0 {
		return defaultSchemaList(), nil
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

	cmGVR := NamespaceGVR()
	cmGVR.Resource = configMapsResource
	cmName := "schema-" + toLowerKind(kind)

	obj, getErr := client.Resource(cmGVR).Namespace("cozy-system").Get(ctx, cmName, metav1.GetOptions{})
	if getErr != nil {
		slog.Debug("schema configmap not found, using minimal", "kind", kind, "error", getErr)

		return minimalSchema(kind), nil
	}

	schema := schemaFromConfigMap(obj)
	if schema == nil {
		schema = minimalSchema(kind)
	}

	ssv.mu.Lock()
	ssv.cache[kind] = schemaCacheEntry{schema: schema, fetchedAt: time.Now()}
	ssv.mu.Unlock()

	return schema, nil
}

func schemaFromConfigMap(obj *unstructured.Unstructured) *AppSchema {
	data, found, err := unstructured.NestedStringMap(obj.Object, "data")
	if err != nil || !found {
		return nil
	}

	schemaJSON, ok := data["values.schema.json"]
	if !ok {
		return nil
	}

	var jsonSchema any

	err = json.Unmarshal([]byte(schemaJSON), &jsonSchema)
	if err != nil {
		return nil
	}

	labels := obj.GetLabels()

	return &AppSchema{
		Kind:        labels["apps.cozystack.io/application.kind"],
		DisplayName: labels["apps.cozystack.io/display-name"],
		Description: data["description"],
		JSONSchema:  jsonSchema,
	}
}

func minimalSchema(kind string) *AppSchema {
	return &AppSchema{
		Kind:        kind,
		DisplayName: kind,
		Description: kind + " application",
		JSONSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func defaultSchemaList() []AppSchema {
	kinds := []struct {
		kind        string
		displayName string
		description string
	}{
		{"Postgres", "PostgreSQL", "Managed PostgreSQL database"},
		{"MySQL", "MySQL", "Managed MySQL database"},
		{"Redis", "Redis", "Managed Redis cache"},
		{"Kafka", "Kafka", "Managed Kafka message broker"},
		{"RabbitMQ", "RabbitMQ", "Managed RabbitMQ message broker"},
		{"NATS", "NATS", "Managed NATS messaging"},
		{"ClickHouse", "ClickHouse", "Managed ClickHouse analytics database"},
		{"MongoDB", "MongoDB", "Managed MongoDB (via FerretDB)"},
		{"Kubernetes", "Kubernetes", "Managed Kubernetes cluster"},
		{"VirtualMachine", "Virtual Machine", "KubeVirt virtual machine"},
		{"Bucket", "Object Storage", "S3-compatible storage bucket"},
		{"Ingress", "Ingress", "HTTP ingress"},
		{"TCPBalancer", "TCP Balancer", "TCP load balancer"},
		{"HTTPCache", "HTTP Cache", "HTTP caching proxy"},
		{"Monitoring", "Monitoring", "Observability stack"},
		{"Tenant", "Tenant", "Sub-tenant namespace"},
	}

	schemas := make([]AppSchema, 0, len(kinds))

	for _, kind := range kinds {
		schemas = append(schemas, AppSchema{
			Kind:        kind.kind,
			DisplayName: kind.displayName,
			Description: kind.description,
		})
	}

	return schemas
}
