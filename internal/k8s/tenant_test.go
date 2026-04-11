package k8s

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestCrdToTenantFlatNamespace locks in the cozystack 1.2 flat-namespace
// behavior: a child tenant's workload namespace is NOT "tenant-<parent>-
// <name>" anymore, it's just "tenant-<name>". Parent is derived from the
// CR's metadata.namespace, which IS the parent's workload namespace by
// cozystack construction.
func TestCrdToTenantFlatNamespace(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		metaNS       string // metadata.namespace (the CR's host)
		metaName     string // metadata.name     (the tenant's short name)
		statusNS     string // status.namespace  (the workload namespace)
		wantParent   string
		wantWorkload string
	}{
		{
			name:         "root tenant",
			metaNS:       "tenant-root",
			metaName:     "root",
			statusNS:     "tenant-root",
			wantParent:   "", // parent == workload → root
			wantWorkload: "tenant-root",
		},
		{
			name:         "child of root",
			metaNS:       "tenant-root",
			metaName:     "demo",
			statusNS:     "tenant-demo",
			wantParent:   "tenant-root",
			wantWorkload: "tenant-demo",
		},
		{
			name:         "grandchild",
			metaNS:       "tenant-demo",
			metaName:     "sub",
			statusNS:     "tenant-sub",
			wantParent:   "tenant-demo",
			wantWorkload: "tenant-sub",
		},
		{
			name:         "no status namespace yet",
			metaNS:       "tenant-root",
			metaName:     "new-one",
			statusNS:     "",
			wantParent:   "", // falls back to parentNamespace, == namespace → root
			wantWorkload: "tenant-root",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			obj := &unstructured.Unstructured{
				Object: map[string]any{
					"metadata": map[string]any{
						"name":      tc.metaName,
						"namespace": tc.metaNS,
					},
					"status": map[string]any{
						"namespace": tc.statusNS,
					},
				},
			}

			tenant := crdToTenant(obj)

			if tenant.Name != tc.metaName {
				t.Errorf("Name = %q, want %q", tenant.Name, tc.metaName)
			}
			if tenant.Namespace != tc.wantWorkload {
				t.Errorf("Namespace = %q, want %q", tenant.Namespace, tc.wantWorkload)
			}
			if tenant.Parent != tc.wantParent {
				t.Errorf("Parent = %q, want %q", tenant.Parent, tc.wantParent)
			}
			if tenant.ParentNamespace != tc.metaNS {
				t.Errorf("ParentNamespace = %q, want %q", tenant.ParentNamespace, tc.metaNS)
			}
		})
	}
}

// TestFindTenantObjPrefersExactName makes sure that siblings sharing the
// same metadata.namespace (the 1.2 flat-namespace default) don't collide:
// asking for "tenant-root" should resolve to the root CR, not a sibling
// child CR that also happens to live in tenant-root.
func TestFindTenantObjPrefersExactName(t *testing.T) {
	t.Parallel()

	const demoName = "demo"

	items := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"name": demoName, "namespace": tenantNamespacePrefix + rootTenantName},
			"status":   map[string]any{"namespace": tenantNamespacePrefix + demoName},
		}},
		{Object: map[string]any{
			"metadata": map[string]any{"name": rootTenantName, "namespace": tenantNamespacePrefix + rootTenantName},
			"status":   map[string]any{"namespace": tenantNamespacePrefix + rootTenantName},
		}},
	}

	// Exact short-name lookup should return root, not demo.
	obj := findTenantObj(items, rootTenantName)
	if obj == nil || obj.GetName() != rootTenantName {
		t.Fatalf("findTenantObj(root) = %v, want 'root' CR", obj)
	}

	// Workload-namespace lookup should also resolve root.
	obj = findTenantObj(items, tenantNamespacePrefix+rootTenantName)
	if obj == nil || obj.GetName() != rootTenantName {
		t.Fatalf("findTenantObj(tenant-root) = %v, want 'root' CR", obj)
	}

	// Short-name demo should still work.
	obj = findTenantObj(items, demoName)
	if obj == nil || obj.GetName() != demoName {
		t.Fatalf("findTenantObj(demo) = %v, want 'demo' CR", obj)
	}

	// Workload-namespace demo should also work.
	obj = findTenantObj(items, tenantNamespacePrefix+demoName)
	if obj == nil || obj.GetName() != demoName {
		t.Fatalf("findTenantObj(tenant-demo) = %v, want 'demo' CR", obj)
	}
}

// TestFindChildrenFlatNamespaces ensures the "children == tenants whose CR
// lives in my workload namespace" rule catches flat-namespace siblings and
// skips the root CR's self-reference.
func TestFindChildrenFlatNamespaces(t *testing.T) {
	t.Parallel()

	items := []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"name": "root", "namespace": "tenant-root"},
			"status":   map[string]any{"namespace": "tenant-root"},
		}},
		{Object: map[string]any{
			"metadata": map[string]any{"name": "demo", "namespace": "tenant-root"},
			"status":   map[string]any{"namespace": "tenant-demo"},
		}},
		{Object: map[string]any{
			"metadata": map[string]any{"name": "other", "namespace": "tenant-root"},
			"status":   map[string]any{"namespace": "tenant-other"},
		}},
		{Object: map[string]any{
			"metadata": map[string]any{"name": "grand", "namespace": "tenant-demo"},
			"status":   map[string]any{"namespace": "tenant-grand"},
		}},
	}

	roots := findChildren(items, "tenant-root")
	if len(roots) != 2 {
		t.Errorf("children of tenant-root: got %d, want 2 (demo, other); %v", len(roots), roots)
	}

	for _, name := range roots {
		if name == rootTenantName {
			t.Errorf("findChildren must not include root itself; got %v", roots)
		}
	}

	grandchildren := findChildren(items, "tenant-demo")
	if len(grandchildren) != 1 || grandchildren[0] != "grand" {
		t.Errorf("children of tenant-demo = %v, want [grand]", grandchildren)
	}
}

func TestIsValidLabelValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"a", true},
		{"my-app", true},
		{"my_app", true},
		{"my.app", true},
		{"My-App-1", true},
		{"foo,apps.cozystack.io/application.kind=Postgres", false},
		{"foo bar", false},
		{"foo/bar", false},
		{"foo=bar", false},
		{"-leading", false},
		{"trailing-", false},
		{"0123456789012345678901234567890123456789012345678901234567890123", false}, // 64 chars
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()

			got := isValidLabelValue(tc.in)
			if got != tc.want {
				t.Errorf("isValidLabelValue(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsRootTenant(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"root":        true,
		"tenant-root": true,
		"demo":        false,
		"tenant-demo": false,
		"":            false,
	}

	for in, want := range cases {
		if got := IsRootTenant(in); got != want {
			t.Errorf("IsRootTenant(%q) = %v, want %v", in, got, want)
		}
	}
}
