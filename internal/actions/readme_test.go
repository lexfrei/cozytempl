package actions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadmeAggregationLabel pins the RBAC aggregation label in the
// README's YAML example to CozystackTenantAdminAggregationLabel.
// Cycle-6 review surfaced a mistake where the README shipped with
// the upstream Kubernetes label (rbac.authorization.k8s.io/aggregate-to-admin)
// which aggregates into the stock admin role, NOT cozy:tenant:admin.
// An operator copy-pasting the wrong label got a ClusterRole that
// silently didn't grant the VM buttons to tenant admins.
func TestReadmeAggregationLabel(t *testing.T) {
	t.Parallel()

	readmePath := filepath.Join("..", "..", "README.md")

	bytes, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}

	body := string(bytes)

	if !strings.Contains(body, CozystackTenantAdminAggregationLabel) {
		t.Errorf("README.md does not reference the Cozystack aggregation label %q — the RBAC example is broken",
			CozystackTenantAdminAggregationLabel)
	}

	// The upstream k8s label pointed at the wrong role; belt-and-
	// braces check to catch a future editor who re-introduces the
	// wrong label while keeping the correct one in a comment.
	if strings.Contains(body,
		`rbac.authorization.k8s.io/aggregate-to-admin: "true"`) {
		t.Errorf("README.md still references upstream admin aggregation label; " +
			"cozy:tenant:admin does not pick it up. Use CozystackTenantAdminAggregationLabel.")
	}
}
