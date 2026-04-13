package page

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view"
)

func sampleTenantRow() view.TenantWithUsage {
	return view.TenantWithUsage{
		Tenant: k8s.Tenant{
			Name:            "demo",
			Namespace:       "tenant-demo",
			ParentNamespace: "tenant-root",
			Parent:          "tenant-root",
			DisplayName:     "demo",
			Status:          "Active",
		},
	}
}

// TestTenantRowCarriesCreateKind verifies that when a preselected kind is
// threaded through, the tenant link carries it as a ?createKind=X query
// param so the tenant detail page can auto-open the create-app modal.
func TestTenantRowCarriesCreateKind(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := tenantRow(sampleTenantRow(), false, "Etcd").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "/tenants/tenant-demo?createKind=Etcd") {
		t.Errorf("tenant link missing ?createKind=Etcd; got:\n%s", got)
	}
}

// TestTenantRowWithoutPreselectedKind makes sure the default case (no
// createKind) still produces the bare tenant URL — no stray query param
// leaks into the link.
func TestTenantRowWithoutPreselectedKind(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := tenantRow(sampleTenantRow(), false, "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}

	got := buf.String()
	if strings.Contains(got, "createKind") {
		t.Errorf("tenant link should not contain createKind; got:\n%s", got)
	}
	if !strings.Contains(got, "/tenants/tenant-demo") {
		t.Errorf("tenant link missing base URL; got:\n%s", got)
	}
}
