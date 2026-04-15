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

	kept, err := FilterAllowed(context.Background(), &rest.Config{}, list, "ns")
	if err != nil {
		t.Fatalf("FilterAllowed returned error: %v", err)
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

	kept, err := FilterAllowed(context.Background(), &rest.Config{}, list, "ns")

	if !errors.Is(err, probeErr) {
		t.Errorf("FilterAllowed err = %v, want wrap of %v", err, probeErr)
	}

	if len(kept) != 1 || kept[0].ID != "good" {
		t.Errorf("FilterAllowed kept = %+v, want [good]", kept)
	}
}

// stubAllowedFn swaps the package-level allowedFn seam and returns
// a restore closure the caller defers. Tests using this helper MUST
// NOT be t.Parallel — the seam is a package global.
func stubAllowedFn(t *testing.T, fn func(context.Context, *rest.Config, Capability, string) (bool, error)) func() {
	t.Helper()

	allowedFn = fn

	return func() { allowedFn = liveAllowed }
}
