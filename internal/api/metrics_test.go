package api

import "testing"

// TestNormalisePathForMetrics locks in the cardinality-bounding rules
// for Prometheus labels. Any change that allows tenant or app names
// to slip through as raw label values must fail this test — a 10k
// tenant cluster with unbounded labels would explode a /metrics
// scrape and OOM Prometheus.
func TestNormalisePathForMetrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"root empty", "", "/"},
		{"root slash", "/", "/"},
		{"marketplace exact", "/marketplace", "/marketplace"},
		{"tenants exact", "/tenants", "/tenants"},
		{"healthz exact", "/healthz", "/healthz"},
		{"readyz exact", "/readyz", "/readyz"},
		{"tenant detail collapses name", "/tenants/tenant-foo", "/tenants/:ns"},
		{"tenant detail collapses long name", "/tenants/tenant-root-subtenant-xyz", "/tenants/:ns"},
		{"app list collapses ns", "/tenants/tenant-foo/apps", "/tenants/:ns/apps"},
		{"app detail collapses ns+name", "/tenants/tenant-foo/apps/redis-prod", "/tenants/:ns/apps/:name"},
		{"api tenant list", "/api/tenants", "/api/tenants"},
		{"api tenant detail collapses", "/api/tenants/tenant-foo", "/api/tenants/:ns"},
		{"api app detail collapses", "/api/tenants/tenant-foo/apps/bar", "/api/tenants/:ns/apps/:name"},
		{"api schemas collapse kind", "/api/schemas/Redis", "/api/schemas/:kind"},
		{"fragments collapse", "/fragments/marketplace", "/fragments/*"},
		{"static collapse", "/static/css/styles.css", "/static/*"},
		{"auth login preserved", "/auth/login", "/auth/login"},
		{"auth callback preserved", "/auth/callback", "/auth/callback"},
		{"unknown falls to other", "/random-garbage", "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := normalisePathForMetrics(tt.in)
			if got != tt.want {
				t.Errorf("normalisePathForMetrics(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
