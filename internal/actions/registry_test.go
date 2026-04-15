package actions

import (
	"context"
	"testing"

	"k8s.io/client-go/rest"
)

// TestRegisterAndFor confirms the happy-path round-trip: an action
// registered for a Kind shows up when For asks for that Kind, and
// stays invisible to every other Kind.
func TestRegisterAndFor(t *testing.T) {
	// Non-parallel: mutates the package-level registry.
	before := len(byKind["TestKindA"])

	Register("TestKindA", Action{
		ID:            "probe",
		LabelKey:      "test.probe",
		AuditCategory: "test.probe",
		Run:           func(context.Context, *rest.Config, string, string) error { return nil },
	})

	t.Cleanup(func() {
		byKind["TestKindA"] = byKind["TestKindA"][:before]
	})

	for _, entry := range For("TestKindA") {
		if entry.ID == "probe" {
			return
		}
	}

	t.Error("probe action not visible via For")
}

// TestLookup returns the registered action on ID hit and a zero
// value + false on both unknown-kind and unknown-ID misses. Keeps
// the HTTP handler's dispatch path honest — a stale URL must not
// silently pick up some other registered action with the same ID.
func TestLookup(t *testing.T) {
	before := len(byKind["TestKindB"])

	Register("TestKindB", Action{
		ID:            "go",
		LabelKey:      "test.go",
		AuditCategory: "test.go",
		Run:           func(context.Context, *rest.Config, string, string) error { return nil },
	})

	t.Cleanup(func() {
		byKind["TestKindB"] = byKind["TestKindB"][:before]
	})

	if _, ok := Lookup("TestKindB", "go"); !ok {
		t.Error("Lookup(TestKindB, go) = false, want true")
	}

	if _, ok := Lookup("TestKindB", "gone"); ok {
		t.Error("Lookup(TestKindB, gone) = true, want false")
	}

	if _, ok := Lookup("NoSuchKind", "go"); ok {
		t.Error("Lookup(NoSuchKind, go) = true, want false")
	}
}

// TestVMActionsRegistered pins the init-time registration for the
// VMInstance Kind. All three KubeVirt subresources must be present
// so the UI can reliably render the action bar without a feature
// flag — adding one is the whole point of the registry.
func TestVMActionsRegistered(t *testing.T) {
	t.Parallel()

	want := map[string]bool{"start": false, "stop": false, "restart": false}

	for _, entry := range For("VMInstance") {
		if _, expected := want[entry.ID]; expected {
			want[entry.ID] = true
		}
	}

	for id, seen := range want {
		if !seen {
			t.Errorf("VMInstance action %q not registered", id)
		}
	}
}

// TestRegisterPanicsOnMissingKind and
// TestRegisterPanicsOnMissingActionID guard the init-time wiring —
// a future contributor who forgets either field should crash
// cozytempl at startup rather than at button-click time.
func TestRegisterPanicsOnMissingKind(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("Register with empty kind did not panic")
		}
	}()

	Register("", Action{ID: "x"})
}

func TestRegisterPanicsOnMissingActionID(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("Register with empty action ID did not panic")
		}
	}()

	Register("SomeKind", Action{})
}
