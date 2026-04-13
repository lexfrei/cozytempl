package page

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view"
)

// TestTenantsShowsHintBannerWhenKindPreselected is the user-facing
// entry point to the marketplace flow: landing on /tenants?kind=Etcd
// must render a hint banner so the user knows what clicking a tenant
// row will do. Absence of the banner on a plain visit is equally
// important — we check both paths.
func TestTenantsShowsHintBannerWhenKindPreselected(t *testing.T) {
	t.Parallel()

	with := view.TenantsPageData{PreselectedKind: "Etcd"}

	var buf bytes.Buffer
	if err := Tenants(with).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, `banner banner-info`) {
		t.Errorf("hint banner class missing when kind preselected; output:\n%s", got)
	}
	// The banner copy is rendered through the i18n bundle, which is
	// not wired in the render context here — partial.Tc returns the
	// bracketed key. Confirming the right key is what reaches the
	// renderer is enough to lock in the wiring.
	if !strings.Contains(got, `[page.tenants.pickForKind]`) {
		t.Errorf("hint banner should call the pickForKind i18n key; output:\n%s", got)
	}
}

func TestTenantsHidesHintBannerWithoutKind(t *testing.T) {
	t.Parallel()

	without := view.TenantsPageData{}

	var buf bytes.Buffer
	if err := Tenants(without).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}

	got := buf.String()
	if strings.Contains(got, `banner banner-info`) {
		t.Errorf("hint banner must not render without a preselected kind; output:\n%s", got)
	}
}

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

// TestTenantRowURL covers the small pure helper that builds the link
// target. Unknown / injection-crafted kinds are filtered upstream by
// selectKnownKind, but the helper still URL-escapes its input so any
// leak downstream emits a well-formed query string rather than a
// parameter-injected one.
func TestTenantRowURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, ns, kind, want string
	}{
		{"no kind", "tenant-demo", "", "/tenants/tenant-demo"},
		{"simple kind", "tenant-demo", "Etcd", "/tenants/tenant-demo?createKind=Etcd"},
		{"kind with ampersand", "tenant-demo", "Etcd&evil=1", "/tenants/tenant-demo?createKind=Etcd%26evil%3D1"},
		{"kind with space", "tenant-demo", "Et cd", "/tenants/tenant-demo?createKind=Et+cd"},
		// Defensive: namespace always comes from the cluster today, but
		// lock in a stable output if something upstream ever passes an
		// empty value instead of panicking or yielding "/tenants?".
		{"empty namespace", "", "Etcd", "/tenants/?createKind=Etcd"},
		{"empty namespace no kind", "", "", "/tenants/"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tenantRowURL(tc.ns, tc.kind)
			if got != tc.want {
				t.Errorf("tenantRowURL(%q, %q) = %q, want %q", tc.ns, tc.kind, got, tc.want)
			}
		})
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
