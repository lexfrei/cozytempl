package page

import "testing"

// TestMarketplaceCardURL covers the small URL builder for marketplace
// card click targets. Kind values come from operator-controlled CRDs
// today, so the typical input is plain PascalCase. The escape is
// defence-in-depth — assert it actually fires when a value contains
// reserved characters so future operators can't accidentally inject
// extra query params.
func TestMarketplaceCardURL(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"Etcd":        "/tenants?kind=Etcd",
		"Etcd&evil=1": "/tenants?kind=Etcd%26evil%3D1",
		"Et cd":       "/tenants?kind=Et+cd",
	}

	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			got := marketplaceCardURL(input)
			if got != want {
				t.Errorf("marketplaceCardURL(%q) = %q, want %q", input, got, want)
			}
		})
	}
}
