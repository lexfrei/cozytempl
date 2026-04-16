package k8s

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestToNodeGroupFullFields walks every field the UI reads. A
// fully-populated MachineDeployment is the happy-path shape CAPI
// emits once the controller has observed the resource — the
// simplified NodeGroup must mirror it without renaming or dropping
// counters.
func TestToNodeGroupFullFields(t *testing.T) {
	t.Parallel()

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"metadata": map[string]any{
				"name": "worker-pool",
			},
			"spec": map[string]any{
				"replicas": int64(5),
			},
			"status": map[string]any{
				"replicas":        int64(5),
				"readyReplicas":   int64(3),
				"updatedReplicas": int64(5),
				"phase":           "ScalingUp",
			},
		},
	}

	got := toNodeGroup(obj)

	if got.Name != "worker-pool" {
		t.Errorf("Name = %q, want worker-pool", got.Name)
	}

	if got.DesiredReplicas != 5 {
		t.Errorf("DesiredReplicas = %d, want 5", got.DesiredReplicas)
	}

	if got.ReadyReplicas != 3 {
		t.Errorf("ReadyReplicas = %d, want 3", got.ReadyReplicas)
	}

	if got.Phase != "ScalingUp" {
		t.Errorf("Phase = %q, want ScalingUp", got.Phase)
	}
}

// TestToNodeGroupMissingStatus covers the brand-new MachineDeployment
// the CAPI controller hasn't touched yet. Every status counter must
// collapse to zero rather than leaking Go default-uninitialised
// sentinels — the template prints the numbers verbatim and "0 ready"
// is the accurate reading, not "missing".
func TestToNodeGroupMissingStatus(t *testing.T) {
	t.Parallel()

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"metadata": map[string]any{
				"name": "fresh-pool",
			},
			"spec": map[string]any{
				"replicas": int64(3),
			},
		},
	}

	got := toNodeGroup(obj)

	if got.Name != "fresh-pool" {
		t.Errorf("Name = %q, want fresh-pool", got.Name)
	}

	if got.DesiredReplicas != 3 {
		t.Errorf("DesiredReplicas = %d, want 3", got.DesiredReplicas)
	}

	if got.ReadyReplicas != 0 {
		t.Errorf("ReadyReplicas = %d, want 0", got.ReadyReplicas)
	}

	if got.Phase != "" {
		t.Errorf("Phase = %q, want empty string", got.Phase)
	}
}

// TestToSortedNodeGroupsSorts confirms the public ordering contract.
// The UI renders pools as rows and must not shuffle them between
// renders — deterministic Name sort is the tie-breaker.
func TestToSortedNodeGroupsSorts(t *testing.T) {
	t.Parallel()

	items := []unstructured.Unstructured{
		{Object: map[string]any{"metadata": map[string]any{"name": "zulu"}}},
		{Object: map[string]any{"metadata": map[string]any{"name": "alpha"}}},
		{Object: map[string]any{"metadata": map[string]any{"name": "mike"}}},
	}

	got := toSortedNodeGroups(items)

	if len(got) != 3 {
		t.Fatalf("got %d groups, want 3", len(got))
	}

	if got[0].Name != "alpha" || got[1].Name != "mike" || got[2].Name != "zulu" {
		t.Errorf("sort order: %v, want [alpha mike zulu]",
			[]string{got[0].Name, got[1].Name, got[2].Name})
	}
}
