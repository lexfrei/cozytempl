package config

import "fmt"

// AuthMode selects how cozytempl conveys user identity to the Kubernetes API
// server. The mode is a deployment-time decision that shapes both the
// middleware pipeline and the RBAC the cozytempl ServiceAccount needs.
type AuthMode string

const (
	// AuthModePassthrough forwards the user's OIDC ID token as a Bearer
	// credential to the Kubernetes API. Cozytempl's own ServiceAccount has
	// zero RBAC in this mode — the k8s API server validates the token
	// directly against the same Keycloak issuer the UI uses. This is the
	// recommended production default for internet-facing deployments.
	AuthModePassthrough AuthMode = "passthrough"

	// AuthModeBYOK ("bring your own kubeconfig") lets the user upload a
	// kubeconfig, which cozytempl stores encrypted in the session cookie
	// and uses for all k8s calls. Suited to standalone / laptop / MSP
	// engineer scenarios where there is no shared OIDC IdP.
	AuthModeBYOK AuthMode = "byok"

	// AuthModeDev disables authentication entirely. Every request is
	// treated as dev-admin with system:masters. NEVER use in production —
	// the UI renders a loud red banner whenever this mode is active.
	AuthModeDev AuthMode = "dev"

	// AuthModeImpersonationLegacy preserves the original impersonation
	// model: cozytempl's ServiceAccount holds cluster-wide
	// impersonate permissions and every k8s call sets Impersonate-User
	// and Impersonate-Groups. Deprecated; kept for operators whose k8s
	// API server is not OIDC-configured yet. Will be removed two minor
	// releases after the passthrough release.
	AuthModeImpersonationLegacy AuthMode = "impersonation-legacy"

	// AuthModeToken accepts a Kubernetes Bearer token pasted by the
	// user, encrypts it into the session cookie, and uses it as the
	// Bearer credential on every k8s call. Cozytempl's own
	// ServiceAccount carries zero RBAC; all permissions come from the
	// pasted token. Suited to clusters without an OIDC IdP where
	// operators want a one-paste login flow rather than the full
	// kubeconfig upload BYOK requires.
	AuthModeToken AuthMode = "token"
)

// String satisfies fmt.Stringer so the value renders cleanly in logs.
func (m AuthMode) String() string { return string(m) }

// Valid reports whether m is one of the four recognised auth modes.
func (m AuthMode) Valid() bool {
	switch m {
	case AuthModePassthrough, AuthModeBYOK, AuthModeDev, AuthModeImpersonationLegacy, AuthModeToken:
		return true
	}

	return false
}

// ParseAuthMode converts a raw env var value into an AuthMode, returning an
// error if the value does not match a recognised mode. Empty input is
// reported as ErrAuthModeEmpty so callers can apply their own default.
func ParseAuthMode(raw string) (AuthMode, error) {
	if raw == "" {
		return "", ErrAuthModeEmpty
	}

	mode := AuthMode(raw)
	if !mode.Valid() {
		return "", fmt.Errorf("%w: %q", ErrAuthModeUnknown, raw)
	}

	return mode, nil
}
