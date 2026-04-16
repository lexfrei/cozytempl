package handler

import (
	"testing"
	"time"

	"github.com/lexfrei/cozytempl/internal/k8s"
)

// TestBuildOverviewDataGroupsByKind pins the core contract: apps
// from different tenants sharing a Kind land in the same KindGroup;
// groups are alpha-sorted so renders stay deterministic across
// visits.
func TestBuildOverviewDataGroupsByKind(t *testing.T) {
	t.Parallel()

	now := time.Now()
	apps := []k8s.Application{
		{Name: "db-a", Kind: "Postgres", Tenant: "tenant-alpha", CreatedAt: now},
		{Name: "db-b", Kind: "Postgres", Tenant: "tenant-beta", CreatedAt: now},
		{Name: "vm-1", Kind: "VMInstance", Tenant: "tenant-alpha", CreatedAt: now},
	}

	got := buildOverviewData(apps, 2, "")

	if got.TotalApps != 3 {
		t.Errorf("TotalApps = %d, want 3", got.TotalApps)
	}

	if got.TotalTenants != 2 {
		t.Errorf("TotalTenants = %d, want 2", got.TotalTenants)
	}

	if len(got.Groups) != 2 {
		t.Fatalf("Groups = %d, want 2 (Postgres, VMInstance)", len(got.Groups))
	}

	// Alpha sort: Postgres before VMInstance.
	if got.Groups[0].Kind != "Postgres" || got.Groups[1].Kind != "VMInstance" {
		t.Errorf("Group order = %q/%q, want Postgres/VMInstance",
			got.Groups[0].Kind, got.Groups[1].Kind)
	}

	if len(got.Groups[0].Apps) != 2 {
		t.Errorf("Postgres group has %d apps, want 2", len(got.Groups[0].Apps))
	}
}

// TestBuildOverviewDataSortsAppsByTenantAndName confirms the
// in-group sort is (tenant, name) — the two keys operators scan
// when looking for a specific workload. A stable order is what
// makes the page feel clean across reloads.
func TestBuildOverviewDataSortsAppsByTenantAndName(t *testing.T) {
	t.Parallel()

	apps := []k8s.Application{
		{Name: "z-app", Kind: "Postgres", Tenant: "tenant-alpha"},
		{Name: "a-app", Kind: "Postgres", Tenant: "tenant-beta"},
		{Name: "m-app", Kind: "Postgres", Tenant: "tenant-alpha"},
	}

	got := buildOverviewData(apps, 2, "")

	if len(got.Groups) != 1 {
		t.Fatalf("Groups = %d, want 1", len(got.Groups))
	}

	names := make([]string, 0, len(got.Groups[0].Apps))
	for _, app := range got.Groups[0].Apps {
		names = append(names, app.Tenant+"/"+app.Name)
	}

	want := []string{"tenant-alpha/m-app", "tenant-alpha/z-app", "tenant-beta/a-app"}
	for i, got := range names {
		if got != want[i] {
			t.Errorf("Apps[%d] = %q, want %q", i, got, want[i])
		}
	}
}

// TestBuildOverviewDataPreservesQuery echoes the raw search term so
// a page reload keeps the filter visible in the input. The filter
// itself is client-side — the value travels with the render solely
// to populate the input's value attribute.
func TestBuildOverviewDataPreservesQuery(t *testing.T) {
	t.Parallel()

	got := buildOverviewData(nil, 0, "prod-")
	if got.Query != "prod-" {
		t.Errorf("Query = %q, want %q", got.Query, "prod-")
	}
}

// TestBuildOverviewDataEmpty covers the zero-app edge case. The
// empty-state UI keys off TotalApps=0, so the handler must still
// return a valid OverviewData (nil Groups, zero counters) rather
// than nilling the whole struct.
func TestBuildOverviewDataEmpty(t *testing.T) {
	t.Parallel()

	got := buildOverviewData(nil, 3, "")

	if got.TotalApps != 0 {
		t.Errorf("TotalApps = %d, want 0", got.TotalApps)
	}

	if got.TotalTenants != 3 {
		t.Errorf("TotalTenants = %d, want 3", got.TotalTenants)
	}

	if len(got.Groups) != 0 {
		t.Errorf("Groups = %+v, want empty", got.Groups)
	}
}
