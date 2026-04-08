// Package view provides templ view data types and helpers.
package view

import "github.com/lexfrei/cozytempl/internal/k8s"

// DashboardData holds data for the dashboard page.
type DashboardData struct {
	Tenants    int
	Apps       int
	Ready      int
	Failed     int
	RecentApps []k8s.Application
}

// TenantPageData holds data for the tenant detail page.
type TenantPageData struct {
	Tenant     k8s.Tenant
	Apps       []k8s.Application
	Schemas    []k8s.AppSchema
	Query      string
	KindFilter string
	SortBy     string
}

// AppDetailData holds data for the application detail page.
type AppDetailData struct {
	App    k8s.Application
	Tenant string
	Tab    string
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
