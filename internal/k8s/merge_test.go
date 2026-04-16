package k8s

import (
	"reflect"
	"testing"
)

// TestPickNextSpecReplaceDeletesAbsentKeys pins the
// kubectl-edit semantics for YAML-mode updates. A user who
// deletes a key from the YAML textarea and saves with
// _tabmode=yaml expects the cluster to lose that key;
// deep-merge would quietly preserve it and the diff the
// user saw locally would silently not apply.
func TestPickNextSpecReplaceDeletesAbsentKeys(t *testing.T) {
	t.Parallel()

	existing := map[string]any{
		"replicas": int64(3),
		"backup": map[string]any{
			"enabled":  true,
			"schedule": "0 2 * * *",
		},
		"postgresql": map[string]any{
			"parameters": map[string]any{
				"max_connections": int64(200),
			},
		},
	}

	incoming := map[string]any{
		"backup": map[string]any{
			"enabled": false,
		},
	}

	got := pickNextSpec(existing, incoming, true)

	if !reflect.DeepEqual(got, incoming) {
		t.Errorf("replace path did not return incoming verbatim: got %+v, want %+v", got, incoming)
	}

	if _, stillThere := got["replicas"]; stillThere {
		t.Error("replicas survived replace; deep-merge semantics leaked through")
	}

	if _, stillThere := got["postgresql"]; stillThere {
		t.Error("postgresql survived replace; deep fields must be deleted when absent from incoming")
	}
}

// TestPickNextSpecMergePreservesExisting pins the form-mode
// contract: a partial form submit must not drop fields the
// user didn't touch. Pair with the test above so the two
// modes are exercised as a unit — a future refactor that
// hard-codes either branch would break one of the two.
func TestPickNextSpecMergePreservesExisting(t *testing.T) {
	t.Parallel()

	existing := map[string]any{
		"replicas": int64(3),
		"backup": map[string]any{
			"enabled":  true,
			"schedule": "0 2 * * *",
		},
	}

	incoming := map[string]any{
		"backup": map[string]any{
			"schedule": "*/30 * * * *",
		},
	}

	got := pickNextSpec(existing, incoming, false)

	if got["replicas"] != int64(3) {
		t.Errorf("replicas = %v, want 3 preserved through merge", got["replicas"])
	}

	backup, ok := got["backup"].(map[string]any)
	if !ok {
		t.Fatalf("backup wrong type: %T", got["backup"])
	}

	if backup["enabled"] != true {
		t.Errorf("backup.enabled = %v, want true preserved through merge", backup["enabled"])
	}

	if backup["schedule"] != "*/30 * * * *" {
		t.Errorf("backup.schedule = %v, want new cron from incoming", backup["schedule"])
	}
}

// TestDeepMergeSpecPreservesUnTouchedFields is the data-loss regression
// guard: if the UI only edits spec.backup.schedule, the existing
// spec.backup.s3SecretKey must survive. Shallow merge was the bug
// until ApplicationService.Update and TenantService.Update switched
// to deepMergeSpec.
func TestDeepMergeSpecPreservesUnTouchedFields(t *testing.T) {
	t.Parallel()

	base := map[string]any{
		"replicas": int64(2),
		"backup": map[string]any{
			"enabled":     true,
			"schedule":    "0 2 * * *",
			"s3SecretKey": "supersekret",
			"s3AccessKey": "realkey",
		},
		"postgresql": map[string]any{
			"parameters": map[string]any{
				"max_connections": int64(100),
			},
		},
	}

	incoming := map[string]any{
		"backup": map[string]any{
			"schedule": "*/30 * * * *",
		},
	}

	merged := deepMergeSpec(base, incoming)

	backup, ok := merged["backup"].(map[string]any)
	if !ok {
		t.Fatalf("merged.backup not a map: %T", merged["backup"])
	}

	if backup["schedule"] != "*/30 * * * *" {
		t.Errorf("backup.schedule = %v, want new value", backup["schedule"])
	}
	if backup["s3SecretKey"] != "supersekret" {
		t.Errorf("backup.s3SecretKey = %v, want preserved", backup["s3SecretKey"])
	}
	if backup["s3AccessKey"] != "realkey" {
		t.Errorf("backup.s3AccessKey = %v, want preserved", backup["s3AccessKey"])
	}
	if backup["enabled"] != true {
		t.Errorf("backup.enabled = %v, want preserved true", backup["enabled"])
	}

	// Nested objects the UI doesn't render must survive untouched.
	pg, ok := merged["postgresql"].(map[string]any)
	if !ok {
		t.Fatalf("merged.postgresql not a map")
	}
	params, ok := pg["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("merged.postgresql.parameters not a map")
	}
	if params["max_connections"] != int64(100) {
		t.Errorf("postgresql.parameters.max_connections wiped: %v", params["max_connections"])
	}

	// Top-level keys not touched by the incoming map stay put.
	if merged["replicas"] != int64(2) {
		t.Errorf("replicas wiped: %v", merged["replicas"])
	}
}

// TestDeepMergeSpecScalarOverMap verifies that a type change from map
// to scalar replaces cleanly — the user picked a different shape and
// that's what they get.
func TestDeepMergeSpecScalarOverMap(t *testing.T) {
	t.Parallel()

	base := map[string]any{
		"auth": map[string]any{"enabled": true, "method": "basic"},
	}
	incoming := map[string]any{"auth": "none"}

	merged := deepMergeSpec(base, incoming)

	if merged["auth"] != "none" {
		t.Errorf("auth = %v, want scalar 'none'", merged["auth"])
	}
}

// TestDeepMergeSpecAddsNewKey makes sure incoming-only keys land.
func TestDeepMergeSpecAddsNewKey(t *testing.T) {
	t.Parallel()

	base := map[string]any{"a": 1}
	incoming := map[string]any{"b": 2}

	merged := deepMergeSpec(base, incoming)

	want := map[string]any{"a": 1, "b": 2}
	if !reflect.DeepEqual(merged, want) {
		t.Errorf("merged = %v, want %v", merged, want)
	}
}

// TestDeepMergeSpecNilBase survives a nil base (Tenant CRs created
// with empty spec), returning a new map seeded from incoming.
func TestDeepMergeSpecNilBase(t *testing.T) {
	t.Parallel()

	incoming := map[string]any{"a": 1}

	merged := deepMergeSpec(nil, incoming)

	if merged["a"] != 1 {
		t.Errorf("merged.a = %v, want 1", merged["a"])
	}
}
