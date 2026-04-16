package k8s

import "time"

// Tenant represents a Cozystack tenant (apps.cozystack.io/v1alpha1 Tenant).
type Tenant struct {
	Name            string   `json:"name"`
	Namespace       string   `json:"namespace"`       // Child namespace (status.namespace, where workloads run)
	ParentNamespace string   `json:"parentNamespace"` // Metadata namespace — where the CR lives (for delete/update)
	DisplayName     string   `json:"displayName"`
	Parent          string   `json:"parent,omitempty"` // Parent namespace, derived from status.namespace hierarchy
	Children        []string `json:"children,omitempty"`
	ChildCount      int      `json:"childCount"`
	AppCount        int      `json:"appCount"`
	Status          string   `json:"status"`
	Version         string   `json:"version,omitempty"`
}

// Application represents a Cozystack application (apps.cozystack.io/v1alpha1).
type Application struct {
	Name           string            `json:"name"`
	Kind           string            `json:"kind"`
	Tenant         string            `json:"tenant"`
	Status         AppStatus         `json:"status"`
	Conditions     []Condition       `json:"conditions,omitempty"`
	Spec           map[string]any    `json:"spec,omitempty"`
	ConnectionInfo map[string]string `json:"connectionInfo,omitempty"`
	Services       []ServiceInfo     `json:"services,omitempty"`
	CreatedAt      time.Time         `json:"createdAt"`
}

// ServiceInfo holds connection endpoint details.
type ServiceInfo struct {
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// AppStatus represents the current status of an application.
type AppStatus string

// Application status values.
const (
	AppStatusReady       AppStatus = "Ready"
	AppStatusReconciling AppStatus = "Reconciling"
	AppStatusFailed      AppStatus = "Failed"
	AppStatusUnknown     AppStatus = "Unknown"
)

// Condition represents a Kubernetes-style condition on a resource.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// AppSchema represents an application type from ApplicationDefinition.
type AppSchema struct {
	Kind             string   `json:"kind"`
	Plural           string   `json:"plural"`
	DisplayName      string   `json:"displayName"`
	Description      string   `json:"description"`
	Category         string   `json:"category,omitempty"`
	Icon             string   `json:"icon,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	JSONSchema       any      `json:"jsonSchema"`
	SecretTemplates  []string `json:"secretTemplates,omitempty"`
	ServiceTemplates []string `json:"serviceTemplates,omitempty"`
}

// CreateApplicationRequest is the payload for creating a new application.
type CreateApplicationRequest struct {
	Name string         `json:"name"`
	Kind string         `json:"kind"`
	Spec map[string]any `json:"spec,omitempty"`
}

// UpdateApplicationRequest is the payload for updating an application.
type UpdateApplicationRequest struct {
	Spec map[string]any `json:"spec"`
	// ResourceVersion carries the resourceVersion the caller
	// observed when reading the edit form. Update passes it
	// to the API server so a concurrent write by another user
	// produces a 409 Conflict (k8s.ErrConflict) instead of a
	// silent overwrite. An empty string disables optimistic
	// locking and keeps the historic last-write-wins behaviour.
	ResourceVersion string `json:"-"`
}

// CreateTenantRequest is the payload for creating a new tenant.
type CreateTenantRequest struct {
	Name   string         `json:"name"`
	Parent string         `json:"parent,omitempty"`
	Spec   map[string]any `json:"spec,omitempty"`
}

// SSEEvent represents a server-sent event for real-time updates.
type SSEEvent struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Data any    `json:"data"`
}

// ResourceQuotaEntry is one row of a namespace's aggregated ResourceQuota
// state, flattened across all ResourceQuota objects in the namespace.
type ResourceQuotaEntry struct {
	// QuotaName is the metadata.name of the ResourceQuota this row came
	// from — useful when a namespace has multiple quotas.
	QuotaName string `json:"quotaName"`
	// Resource is the resource key, e.g. "requests.cpu", "pods",
	// "limits.memory".
	Resource string `json:"resource"`
	// Hard is the configured limit as a K8s quantity string.
	Hard string `json:"hard"`
	// Used is the current usage as a K8s quantity string.
	Used string `json:"used"`
}

// Event is a simplified view of a core/v1 Event rendered for the UI.
//
// Name is the Kubernetes resource name (metadata.name, e.g.
// "myvm.1811ab4c012345") — opaque to the user but required by the
// SSE watch proxy as the stable DOM row id. Populated by toEvent.
type Event struct {
	Name      string    `json:"name"`
	Type      string    `json:"type"` // Normal / Warning
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Source    string    `json:"source"`
	Object    string    `json:"object"` // involvedObject kind/name
	Count     int64     `json:"count"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}
