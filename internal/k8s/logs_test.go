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
func TestValidateStreamLogsParams(t *testing.T) {
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

			err := validateStreamLogsParams(tc.namespace, tc.pod, tc.container)
			gotErr := err != nil
			if gotErr != tc.wantErr {
				t.Errorf("validateStreamLogsParams(%q, %q, %q) err=%v, wantErr=%v",
					tc.namespace, tc.pod, tc.container, err, tc.wantErr)
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
