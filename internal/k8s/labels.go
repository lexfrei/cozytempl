package k8s

import "regexp"

// Label and annotation keys used by Cozystack. Centralised so a rename in
// upstream Cozystack hits one place instead of six scattered string literals.
const (
	cozyAppKindLabel = "apps.cozystack.io/application.kind"
	cozyAppNameLabel = "apps.cozystack.io/application.name"
)

// labelValueRegex is a conservative subset of what Kubernetes accepts for
// label values: alphanumerics plus dash, underscore, and dot, 1..63 chars,
// starting and ending with alphanumeric. This is strict enough to block
// label-selector injection via names that embed a comma + another label
// clause (the CRITICAL-4 finding from the security audit).
var labelValueRegex = regexp.MustCompile(`^[a-z0-9A-Z]([-a-z0-9A-Z_.]*[a-z0-9A-Z])?$`)

// maxLabelValueLength matches the Kubernetes limit for label values.
const maxLabelValueLength = 63

// isValidLabelValue reports whether s can be safely interpolated into a
// Kubernetes label selector without escaping. Callers must reject invalid
// values with an auth / not-found error — do NOT sanitize and continue,
// because any character the regex does not permit could change the meaning
// of the selector.
func isValidLabelValue(s string) bool {
	if s == "" || len(s) > maxLabelValueLength {
		return false
	}

	return labelValueRegex.MatchString(s)
}
