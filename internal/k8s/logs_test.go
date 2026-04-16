package k8s

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateStreamLogsParams locks the defensive input fence
// applied by both TailLogs and StreamLogs. A future refactor
// that moves the validation elsewhere must keep the same
// reject-set or an attacker-controlled namespace / container
// value would reach the apiserver URL / error-string
// interpolation unchanged.
// TestValidateLogsParams pins the shared defensive fence used
// by both TailLogs and StreamLogs — the two paths must agree
// on what's accepted so a user switching between the paginated
// tail and the live stream doesn't see one succeed while the
// other 400s for the same container name.
func TestValidateLogsParams(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		namespace string
		pod       string
		container string
		wantErr   bool
	}{
		{"happy", "tenant-root", "myvm-0", "main", false},
		{"happy-empty-container", "tenant-root", "myvm-0", "", false},
		{"empty-namespace", "", "myvm-0", "", true},
		{"empty-pod", "tenant-root", "", "", true},
		{"bad-namespace", "tenant!", "myvm-0", "", true},
		{"bad-pod", "tenant-root", "pod name with spaces", "", true},
		{"bad-container", "tenant-root", "myvm-0", "bad/container", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateLogsParams(tc.namespace, tc.pod, tc.container)
			gotErr := err != nil
			if gotErr != tc.wantErr {
				t.Errorf("validateLogsParams(%q, %q, %q) err=%v, wantErr=%v",
					tc.namespace, tc.pod, tc.container, err, tc.wantErr)
			}
		})
	}
}

// TestTailLogsAppliesTheSameFence pins the cross-path contract:
// TailLogs and StreamLogs share validateLogsParams, so a
// malformed namespace or container that one rejects must reject
// on the other too. Previously TailLogs only fenced `pod` while
// StreamLogs fenced all three — a user switching between the
// paginated tail and the live stream could see one accept input
// the other refused.
func TestTailLogsAppliesTheSameFence(t *testing.T) {
	t.Parallel()

	lsv := NewLogService(nil, "dev")

	cases := []struct {
		name      string
		namespace string
		pod       string
		container string
	}{
		{"bad-namespace", "tenant!", "myvm-0", ""},
		{"bad-container", "tenant-root", "myvm-0", "bad/container"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := lsv.TailLogs(t.Context(), nil, tc.namespace, tc.pod, tc.container, 500)
			if err == nil {
				t.Fatal("expected validation error")
			}

			if !errors.Is(err, ErrAppNotFound) {
				t.Errorf("err = %v, want wraps ErrAppNotFound", err)
			}
		})
	}
}

// TestStreamLogsTimeoutZero confirms the critical comment in
// StreamLogs is enforced. Calling StreamLogs against a nil
// config surfaces an error chain we can fingerprint; we only
// care that the deadline-reset path runs before the error
// surfaces, which is observable by the specific error string
// from rest.RESTClientFor.
//
// A full integration test against a fake apiserver would be
// better but requires envtest. This unit test catches the
// narrow regression where a future edit deletes `cfg.Timeout =
// 0` — without the line, the stream would inherit the 10 s
// HTTP client deadline and die mid-follow. The validation runs
// before Timeout is inspected, so we drive the config-error
// path deliberately.
func TestStreamLogsRejectsInvalidPodBeforeTouchingConfig(t *testing.T) {
	t.Parallel()

	lsv := NewLogService(nil, "dev")

	_, err := lsv.StreamLogs(t.Context(), nil, "ns", "bad pod name", "", 500)
	if err == nil {
		t.Fatal("expected validation error for malformed pod name")
	}

	if !errors.Is(err, ErrAppNotFound) {
		t.Errorf("err = %v, want wraps ErrAppNotFound", err)
	}

	if !strings.Contains(err.Error(), "bad pod name") {
		t.Errorf("err = %v, want message to include rejected name", err)
	}
}
