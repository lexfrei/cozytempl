package actions

import (
	"context"
	"fmt"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// allowedFn is the package-level test seam for Allowed. Production
// wires it to liveAllowed, which issues a real SSAR; tests that need
// deterministic allow/deny decisions overwrite it inline. NOT
// parallel-safe — matches probeTokenFn in internal/auth (intentional
// — test hygiene beats a mutex on a startup-time variable).
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
func SwapAllowedFnForTest(fn func(context.Context, *rest.Config, Capability, string) (bool, error)) func() {
	original := allowedFn
	allowedFn = fn

	return func() { allowedFn = original }
}

// FilterAllowed returns the subset of list whose Capability the
// caller is permitted to invoke, preserving order. Probe errors on
// individual actions drop them from the list (safer to omit a button
// than to show one that 403s) but the probeErr return captures the
// first such error so the caller can log it. A successful run for
// every action returns probeErr=nil.
func FilterAllowed(ctx context.Context, userCfg *rest.Config, list []Action, namespace string) ([]Action, error) {
	if len(list) == 0 {
		return nil, nil
	}

	kept := make([]Action, 0, len(list))

	var probeErr error

	for i := range list {
		allowed, err := Allowed(ctx, userCfg, list[i].Capability, namespace)
		if err != nil && probeErr == nil {
			probeErr = err
		}

		if allowed {
			kept = append(kept, list[i])
		}
	}

	return kept, probeErr
}
