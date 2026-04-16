// Package view provides templ view data types and helpers.
package view

import (
	"github.com/lexfrei/cozytempl/internal/actions"
	"github.com/lexfrei/cozytempl/internal/k8s"
)

// DashboardData holds data for the dashboard page.
type DashboardData struct {
	Tenants    int
	Apps       int
	Ready      int
	Failed     int
	RecentApps []k8s.Application
}

// TenantsPageData holds data for the tenants management page.
type TenantsPageData struct {
	Tenants        []TenantWithUsage
	TenantSchema   *k8s.AppSchema
	MetricsEnabled bool
	// PreselectedKind is non-empty when the user arrived on the
	// tenants page from a marketplace card click. The template
	// renders a hint banner telling them to pick a tenant to
	// create the chosen kind in. Empty = no hint, normal list.
	PreselectedKind string
}

// TenantWithUsage combines tenant metadata and resource usage.
type TenantWithUsage struct {
	Tenant k8s.Tenant
	Usage  k8s.TenantUsage
}

// TenantPageData holds data for the tenant detail page.
type TenantPageData struct {
	Tenant   k8s.Tenant
	Usage    k8s.TenantUsage          // Aggregated resource usage for the workload namespace.
	Quotas   []k8s.ResourceQuotaEntry // Flattened ResourceQuota entries; empty if none configured.
	Children []k8s.Tenant             // Direct child tenants, filtered to those visible to the user.
	Apps     []k8s.Application
	// AppsTruncated is true when ApplicationService.List hit its
	// hard cap and the API server returned a continue token. The
	// template renders a warning so the user understands the
	// client-side filter/sort is running over a bounded window.
	AppsTruncated bool
	Schemas       []k8s.AppSchema
	Events        []k8s.Event // Recent k8s events in the tenant's workload namespace.
	Query         string
	KindFilter    string
	SortBy        string
	// CreateKind is non-empty when the page is opened with
	// ?createKind=X (the marketplace→tenants→tenant redirect chain).
	// The template auto-opens the create-app modal pre-populated
	// with this kind so the user lands directly on the form.
	CreateKind string
}

// AppDetailData holds data for the application detail page.
type AppDetailData struct {
	App    k8s.Application
	Tenant string
	Tab    string
	// Events is populated only when the Events tab is active. Empty on
	// other tabs so the JSON/HTML rendering does not lug a big slice
	// around for every navigation.
	Events []k8s.Event
	// Pods is populated only when the Logs tab is active. Contains the
	// list of pods for this app; the handler separately fetches the
	// current tail for the selected pod.
	Pods []k8s.PodInfo
	// SelectedPod / SelectedContainer / LogTail feed the Logs tab UI.
	SelectedPod       string
	SelectedContainer string
	LogTail           string
	LogError          string
	// AllowedActions is the per-resource actions registered for
	// this application's Kind, filtered down to the ones the current
	// user is permitted to invoke (via a SelfSubjectAccessReview
	// probe). Empty slice means "register none or all denied" — the
	// UI renders no action bar in either case, which is the right
	// collapse for a user who can't act.
	AllowedActions []actions.Action
	// NodeGroups is populated only for applications of Kind "Kubernetes"
	// (Cluster API-managed Kubernetes clusters). Holds the spec /
	// status replica counts of every MachineDeployment owned by this
	// cluster so the overview tab can surface "3 desired, 2 ready"
	// while the node group is scaling up or draining.
	NodeGroups []k8s.NodeGroup
}

// MarketplaceData holds data for the marketplace page.
type MarketplaceData struct {
	Schemas        []k8s.AppSchema
	Categories     []CategoryGroup
	AllTags        []string
	Query          string
	CategoryFilter string
	TagFilter      string
}

// CategoryGroup groups schemas by category.
type CategoryGroup struct {
	Name    string
	Schemas []k8s.AppSchema
}
