package k8s

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestIsConflictErrorTypedStatus covers the canonical path: a real
// k8s StatusError with code 409 and reason "Conflict" must be
// recognised as a conflict.
func TestIsConflictErrorTypedStatus(t *testing.T) {
	t.Parallel()

	statusErr := apierrors.NewConflict(
		schema.GroupResource{Group: "apps.cozystack.io", Resource: "tenants"},
		"foo",
		errors.New("resourceVersion mismatch"),
	)

	if !isConflictError(statusErr) {
		t.Errorf("isConflictError(typed 409) = false, want true")
	}
}

// TestIsConflictErrorWrappedMessage covers the cozystack path: the
// admission webhook rewraps the underlying 409 in a plain %v error,
// dropping the StatusError metadata. The message fragment "the
// object has been modified" is still present and that's what we
// pattern-match on.
func TestIsConflictErrorWrappedMessage(t *testing.T) {
	t.Parallel()

	// Simulate how cozystack's webhook reports the nested HelmRelease
	// conflict — a plain errors.New, no StatusError metadata.
	wrapped := fmt.Errorf("failed to update HelmRelease: " +
		"Operation cannot be fulfilled on helmreleases.helm.toolkit.fluxcd.io " +
		"\"tenant-root\": the object has been modified; please apply your " +
		"changes to the latest version and try again")

	if !isConflictError(wrapped) {
		t.Errorf("isConflictError(wrapped message) = false, want true")
	}
}

// TestIsConflictErrorRejectsUnrelated covers the negative case so a
// future refactor can't accidentally over-match.
func TestIsConflictErrorRejectsUnrelated(t *testing.T) {
	t.Parallel()

	tests := []error{
		nil,
		errors.New("some other failure"),
		apierrors.NewNotFound(
			schema.GroupResource{Group: "apps.cozystack.io", Resource: "tenants"},
			"missing",
		),
		&apierrors.StatusError{
			ErrStatus: metav1.Status{
				Code:   http.StatusBadRequest,
				Reason: metav1.StatusReasonBadRequest,
			},
		},
	}

	for _, err := range tests {
		if isConflictError(err) {
			t.Errorf("isConflictError(%v) = true, want false", err)
		}
	}
}
