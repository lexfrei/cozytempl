package k8s

import "time"

// Tenant represents a Cozystack tenant (a Kubernetes namespace with tenant-* prefix).
type Tenant struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Parent      string   `json:"parent,omitempty"`
	Children    []string `json:"children,omitempty"`
	ChildCount  int      `json:"childCount"`
	AppCount    int      `json:"appCount"`
	Status      string   `json:"status"`
}

// Application represents a Cozystack application (a FluxCD HelmRelease).
type Application struct {
	Name           string            `json:"name"`
	Kind           string            `json:"kind"`
	Tenant         string            `json:"tenant"`
	Status         AppStatus         `json:"status"`
	Conditions     []Condition       `json:"conditions,omitempty"`
	Values         map[string]any    `json:"values,omitempty"`
	ConnectionInfo map[string]string `json:"connectionInfo,omitempty"`
	CreatedAt      time.Time         `json:"createdAt"`
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

// AppSchema represents the JSON Schema for an application type.
type AppSchema struct {
	Kind        string `json:"kind"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Icon        string `json:"icon,omitempty"`
	JSONSchema  any    `json:"jsonSchema"`
	Defaults    any    `json:"defaults,omitempty"`
}

// CreateApplicationRequest is the payload for creating a new application.
type CreateApplicationRequest struct {
	Name   string         `json:"name"`
	Kind   string         `json:"kind"`
	Values map[string]any `json:"values,omitempty"`
}

// UpdateApplicationRequest is the payload for updating an application.
type UpdateApplicationRequest struct {
	Values map[string]any `json:"values"`
}

// CreateTenantRequest is the payload for creating a new tenant.
type CreateTenantRequest struct {
	Name   string `json:"name"`
	Parent string `json:"parent,omitempty"`
}

// SSEEvent represents a server-sent event for real-time updates.
type SSEEvent struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Data any    `json:"data"`
}

// UserInfo holds the authenticated user's identity extracted from OIDC.
type UserInfo struct {
	Username string
	Groups   []string
}
