package handler

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestApiserverErrorLabelClassifies walks the four cases the audit
// pipeline needs to distinguish: RBAC-forbidden, NotFound, apiserver
// Unauthorized, and everything else. Each is wrapped through the
// handler's own fmt.Errorf(%w) so the test also confirms errors.As
// unwraps correctly.
//
// Regression coverage: a prior revision used a made-up interface
// (Status() interface{ GetCode() int32 }) that no k8s type satisfied,
// so every error silently collapsed to "other". This test fails at
// compile time (wrong type) or runtime (wrong label) if anyone
// reintroduces that interface-match pattern.
func TestApiserverErrorLabelClassifies(t *testing.T) {
	t.Parallel()

	gr := schema.GroupResource{Group: "", Resource: "virtualmachines"}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			"forbidden wrapped",
			fmt.Errorf("running stop action: %w",
				apierrors.NewForbidden(gr, "vm-42", errors.New("no RBAC"))),
			errLabelForbidden,
		},
		{
			"unauthorized wrapped twice",
			fmt.Errorf("outer: %w",
				fmt.Errorf("inner: %w", apierrors.NewUnauthorized("token expired"))),
			errLabelUnauthorized,
		},
		{
			"not found wrapped twice",
			fmt.Errorf("outer: %w",
				fmt.Errorf("inner: %w", apierrors.NewNotFound(gr, "vm-42"))),
			errLabelNotFound,
		},
		{
			"unauthorized",
			apierrors.NewUnauthorized("token expired"),
			errLabelUnauthorized,
		},
		{
			"plain error falls through to other",
			errors.New("dial tcp: connection refused"),
			errLabelOther,
		},
		{
			"nil is safe and classified as other",
			nil,
			errLabelOther,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := apiserverErrorLabel(tc.err)
			if got != tc.want {
				t.Errorf("apiserverErrorLabel = %q, want %q (err=%v)", got, tc.want, tc.err)
			}
		})
	}
}
