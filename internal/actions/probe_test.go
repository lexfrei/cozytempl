package actions

import (
	"context"
	"errors"
	"testing"

	"k8s.io/client-go/rest"
)

// TestAllowedShortCircuitsOnEmptyCapability confirms the opt-out
// behaviour: an Action that deliberately registers an empty
// Capability — the "authorisation isn't expressible as one SSAR"
// escape hatch — bypasses the probe entirely and is always allowed.
func TestAllowedShortCircuitsOnEmptyCapability(t *testing.T) {
	// Not parallel — mutates allowedFn via the test seam below.
	stub := stubAllowedFn(t, func(context.Context, *rest.Config, Capability, string) (bool, error) {
		t.Fatal("allowedFn should not fire for empty Capability")

		return false, nil
	})
	defer stub()

	ok, err := Allowed(context.Background(), &rest.Config{}, Capability{}, "ns")
	if err != nil {
		t.Fatalf("Allowed with empty capability returned error: %v", err)
	}

	if !ok {
		t.Errorf("Allowed with empty capability = false, want true (short-circuit)")
	}
}

// TestAllowedMirrorsSeamDecision walks both positive and negative
// responses through the allowedFn seam. Without this test a future
// refactor that hard-codes Status.Allowed = true or swallows the
// error would leave every user seeing every button.
func TestAllowedMirrorsSeamDecision(t *testing.T) {
	cases := []struct {
		name    string
		allowed bool
		err     error
	}{
		{"allow", true, nil},
		{"deny", false, nil},
		{
			"probe error propagates as false+err",
			false, errors.New("virt-api down"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := stubAllowedFn(t, func(context.Context, *rest.Config, Capability, string) (bool, error) {
				return tc.allowed, tc.err
			})
			defer stub()

			capability := Capability{
				Group: "g", Resource: "r", Subresource: "s", Verb: "update",
			}

			ok, err := Allowed(context.Background(), &rest.Config{}, capability, "ns")
			if ok != tc.allowed {
				t.Errorf("Allowed = %v, want %v", ok, tc.allowed)
			}

			if (err != nil) != (tc.err != nil) {
				t.Errorf("Allowed err = %v, want %v", err, tc.err)
			}
		})
	}
}

// TestFilterAllowedPreservesOrderAndDropsDenied covers the main
// FilterAllowed contract: keep the allowed actions in their
// registration order, drop the denied ones, never reorder. Order
// matters because the UI renders the buttons left-to-right.
func TestFilterAllowedPreservesOrderAndDropsDenied(t *testing.T) {
	stub := stubAllowedFn(t, func(_ context.Context, _ *rest.Config, capability Capability, _ string) (bool, error) {
		return capability.Subresource != "denied", nil
	})
	defer stub()

	list := []Action{
		{ID: "first", Capability: Capability{Group: "g", Resource: "r", Subresource: "first-ok", Verb: "update"}},
		{ID: "middle", Capability: Capability{Group: "g", Resource: "r", Subresource: "denied", Verb: "update"}},
		{ID: "last", Capability: Capability{Group: "g", Resource: "r", Subresource: "last-ok", Verb: "update"}},
	}

	kept, errCount, err := FilterAllowed(context.Background(), &rest.Config{}, list, "ns")
	if err != nil {
		t.Fatalf("FilterAllowed returned error: %v", err)
	}

	if errCount != 0 {
		t.Errorf("FilterAllowed errCount = %d, want 0 when no probes failed", errCount)
	}

	if len(kept) != 2 {
		t.Fatalf("FilterAllowed returned %d, want 2", len(kept))
	}

	if kept[0].ID != "first" || kept[1].ID != "last" {
		t.Errorf("FilterAllowed order = %v, want [first last]",
			[]string{kept[0].ID, kept[1].ID})
	}
}

// TestFilterAllowedReturnsProbeError captures the second half of
// FilterAllowed's contract — drop the error-returning action, keep
// the rest, and surface the first error for logging. The handler
// uses this exact distinction to tell "user doesn't have RBAC"
// (some probes return false+nil) from "apiserver is on fire" (they
// return false+err).
func TestFilterAllowedReturnsProbeError(t *testing.T) {
	probeErr := errors.New("virt-api down")

	stub := stubAllowedFn(t, func(_ context.Context, _ *rest.Config, capability Capability, _ string) (bool, error) {
		if capability.Subresource == "broken" {
			return false, probeErr
		}

		return true, nil
	})
	defer stub()

	list := []Action{
		{ID: "good", Capability: Capability{Group: "g", Resource: "r", Subresource: "ok", Verb: "update"}},
		{ID: "bad", Capability: Capability{Group: "g", Resource: "r", Subresource: "broken", Verb: "update"}},
	}

	kept, errCount, err := FilterAllowed(context.Background(), &rest.Config{}, list, "ns")

	if !errors.Is(err, probeErr) {
		t.Errorf("FilterAllowed err = %v, want wrap of %v", err, probeErr)
	}

	if errCount != 1 {
		t.Errorf("FilterAllowed errCount = %d, want 1 (single broken probe)", errCount)
	}

	if len(kept) != 1 || kept[0].ID != "good" {
		t.Errorf("FilterAllowed kept = %+v, want [good]", kept)
	}
}

// TestFilterAllowedProbesOncePerAction pins the fan-out: exactly
// one probe per registered action, never more. A loop-bug that
// double-probes or re-probes on every render would 2N the apiserver
// load; catching it here is cheaper than catching it in prod logs.
func TestFilterAllowedProbesOncePerAction(t *testing.T) {
	var probes int

	stub := stubAllowedFn(t, func(context.Context, *rest.Config, Capability, string) (bool, error) {
		probes++

		return true, nil
	})
	defer stub()

	list := []Action{
		{ID: "a", Capability: Capability{Group: "g", Resource: "r", Subresource: "a", Verb: "update"}},
		{ID: "b", Capability: Capability{Group: "g", Resource: "r", Subresource: "b", Verb: "update"}},
		{ID: "c", Capability: Capability{Group: "g", Resource: "r", Subresource: "c", Verb: "update"}},
	}

	if _, _, err := FilterAllowed(context.Background(), &rest.Config{}, list, "ns"); err != nil {
		t.Fatalf("FilterAllowed: %v", err)
	}

	if probes != len(list) {
		t.Errorf("probes = %d, want %d (one per action, no more, no less)", probes, len(list))
	}
}

// TestFilterAllowedCountsAllErrors pins the contract the cycle-5
// review asked for: when multiple probes fail with distinct errors,
// errCount must reflect the full count so the caller can log
// "saw N probes fail" rather than silently dropping the majority of
// the button set based on a single logged error.
func TestFilterAllowedCountsAllErrors(t *testing.T) {
	stub := stubAllowedFn(t, func(_ context.Context, _ *rest.Config, capability Capability, _ string) (bool, error) {
		return false, errors.New("probe " + capability.Subresource + " failed")
	})
	defer stub()

	list := []Action{
		{ID: "a", Capability: Capability{Group: "g", Resource: "r", Subresource: "a", Verb: "update"}},
		{ID: "b", Capability: Capability{Group: "g", Resource: "r", Subresource: "b", Verb: "update"}},
		{ID: "c", Capability: Capability{Group: "g", Resource: "r", Subresource: "c", Verb: "update"}},
	}

	kept, errCount, err := FilterAllowed(context.Background(), &rest.Config{}, list, "ns")

	if err == nil {
		t.Fatal("FilterAllowed err = nil, want first of three probe errors")
	}

	if errCount != 3 {
		t.Errorf("FilterAllowed errCount = %d, want 3", errCount)
	}

	if len(kept) != 0 {
		t.Errorf("FilterAllowed kept = %+v, want empty (all probes failed)", kept)
	}
}

// stubAllowedFn swaps the package-level allowedFn seam and returns
// a restore closure the caller defers. Tests using this helper MUST
// NOT be t.Parallel — the seam is a package global.
//
// Captures the prior allowedFn (rather than hardcoding liveAllowed)
// so nested stubs unwind in LIFO order: an outer test may install a
// counting wrapper and call a helper that installs a deny-all stub;
// the helper's restore must snap back to the counting wrapper, not
// to liveAllowed.
func stubAllowedFn(t *testing.T, fn func(context.Context, *rest.Config, Capability, string) (bool, error)) func() {
	t.Helper()

	original := allowedFn
	allowedFn = fn

	return func() { allowedFn = original }
}
