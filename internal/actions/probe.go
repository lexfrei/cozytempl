package actions

import (
	"context"
	"fmt"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// allowedFn is the package-level test seam for Allowed. Production
// wires it to liveAllowed, which issues a real SSAR; tests that need
// deterministic allow/deny decisions overwrite it via
// SwapAllowedFnForTest. NOT parallel-safe: allowedFn is read from
// every HTTP handler (capabilityProbedActions on every detail-page
// render, InvokeAction on every POST), so live-swapping while
// requests are in flight is a race. Tests calling the swap helper
// MUST NOT use t.Parallel; that is why SwapAllowedFnForTest panics
// when called from a non-test binary.
//
//nolint:gochecknoglobals // intentional test seam
var allowedFn = liveAllowed

// Allowed returns true when the caller whose credentials are in
// userCfg is permitted to exercise capability on namespace.
// Production defers to liveAllowed (a single SelfSubjectAccessReview
// round-trip). Actions with an empty Capability.Resource are always
// allowed — the probe short-circuits to true. This covers actions
// whose authorisation isn't expressible as a single SSAR, e.g.
// multi-step backend operations. Every shipped action should
// populate the tuple; opt-out is for unusual cases.
//
// Probe failures (network error, virt-api down) return false +
// error. Callers typically treat false-with-error the same as
// false-no-error — hiding the button is the safe default — but
// the distinction is preserved so handlers can log the probe
// error separately from "legitimately denied".
func Allowed(ctx context.Context, userCfg *rest.Config, capability Capability, namespace string) (bool, error) {
	if !capability.HasResource() {
		return true, nil
	}

	return allowedFn(ctx, userCfg, capability, namespace)
}

// liveAllowed is the production probe — one SSAR against userCfg's
// apiserver. Split from Allowed so tests can swap allowedFn without
// standing up a fake http.Server and reverse-engineering client-go's
// request shape.
func liveAllowed(ctx context.Context, userCfg *rest.Config, capability Capability, namespace string) (bool, error) {
	client, err := kubernetes.NewForConfig(userCfg)
	if err != nil {
		return false, fmt.Errorf("building clientset for SSAR: %w", err)
	}

	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Namespace:   namespace,
				Verb:        capability.Verb,
				Group:       capability.Group,
				Resource:    capability.Resource,
				Subresource: capability.Subresource,
			},
		},
	}

	result, err := client.AuthorizationV1().
		SelfSubjectAccessReviews().
		Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("SSAR for %s/%s/%s verb=%s: %w",
			capability.Group, capability.Resource, capability.Subresource, capability.Verb, err)
	}

	return result.Status.Allowed, nil
}

// SwapAllowedFnForTest exposes the internal allowedFn seam to
// external test packages (the handler tests need to stub the
// probe without importing the unexported variable). Returns a
// restore closure. Tests calling this MUST NOT use t.Parallel.
//
// Panics when called outside a test binary: the seam exists for
// test-only stubbing, and a production caller flipping the allow/
// deny decision of every probe is always a bug. testing.Testing()
// returns true only when the Go test harness linked the binary.
func SwapAllowedFnForTest(stub func(context.Context, *rest.Config, Capability, string) (bool, error)) func() {
	if !testing.Testing() {
		panic("actions.SwapAllowedFnForTest called outside a test binary")
	}

	original := allowedFn
	allowedFn = stub

	return func() { allowedFn = original }
}

// FilterAllowed returns the subset of list whose Capability the
// caller is permitted to invoke, preserving order. Probe errors on
// individual actions drop them from the list (safer to omit a button
// than to show one that 403s). errCount is the total number of
// probes that errored; firstErr is the first error encountered —
// together they let the caller log "saw N probes fail" rather than
// silently dropping most of the button set based on a single logged
// error. A successful run for every action returns (kept, 0, nil).
func FilterAllowed(
	ctx context.Context, userCfg *rest.Config, list []Action, namespace string,
) ([]Action, int, error) {
	if len(list) == 0 {
		return nil, 0, nil
	}

	kept := make([]Action, 0, len(list))

	var (
		errCount int
		firstErr error
	)

	for i := range list {
		allowed, err := Allowed(ctx, userCfg, list[i].Capability, namespace)
		if err != nil {
			errCount++

			if firstErr == nil {
				firstErr = err
			}
		}

		if allowed {
			kept = append(kept, list[i])
		}
	}

	return kept, errCount, firstErr
}
