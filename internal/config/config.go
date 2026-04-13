// Package config provides environment-based application configuration.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// Config holds the application configuration loaded from environment variables.
type Config struct {
	ListenAddr            string
	OIDCIssuerURL         string
	OIDCInternalIssuerURL string
	OIDCClientID          string
	OIDCClientSecret      string
	OIDCRedirectURL       string
	SessionSecret         string
	LogLevel              string
	// DebugPprofAddr, when non-empty, mounts net/http/pprof on a
	// dedicated listener bound to this address. Typically
	// "localhost:6060" for port-forward-only access. The public
	// ListenAddr never exposes pprof. Empty disables the
	// endpoint entirely. Set via COZYTEMPL_DEBUG_PPROF_ADDR.
	DebugPprofAddr string
	AuthMode       AuthMode
	DevMode        bool

	// TrustForwardedHeaders gates whether the IP rate limiter on the
	// pre-auth upload endpoints (/auth/kubeconfig, /auth/token)
	// honours X-Forwarded-For. Only enable it when cozytempl runs
	// behind a trusted reverse proxy (Ingress, Cloudflare Tunnel)
	// that strips client-supplied XFF values. When false (the
	// default), the limiter keys strictly on req.RemoteAddr so an
	// attacker cannot spoof XFF to bypass the bucket. Controlled
	// by COZYTEMPL_TRUST_FORWARDED_HEADERS=true.
	TrustForwardedHeaders bool
}

// ErrMissingEnvVar is returned when a required environment variable is not set.
var ErrMissingEnvVar = errors.New("required environment variable is not set")

// ErrDevModeNotEnabled is returned when OIDC configuration is missing but the
// explicit COZYTEMPL_DEV_MODE=true opt-in is not set. We refuse to silently
// enable dev mode in production so a misconfigured deployment fails loudly
// instead of exposing an unauthenticated dev-admin session to the network.
var ErrDevModeNotEnabled = errors.New("OIDC_ISSUER_URL is required; set COZYTEMPL_DEV_MODE=true to run with authentication disabled")

// ErrWeakSessionSecret is returned when production mode is requested but the
// session secret is still at its placeholder value. A known signing key means
// an attacker could forge cookies and impersonate any user.
var ErrWeakSessionSecret = errors.New("SESSION_SECRET is required in production and must not be the placeholder dev value")

// ErrAuthModeEmpty signals that no COZYTEMPL_AUTH_MODE env var was provided;
// the caller decides the default (typically AuthModePassthrough).
var ErrAuthModeEmpty = errors.New("auth mode is empty")

// ErrAuthModeUnknown is returned when COZYTEMPL_AUTH_MODE holds a value that
// is not one of the five recognised modes.
var ErrAuthModeUnknown = errors.New("unknown auth mode")

const (
	// devModeEnv is the explicit opt-in toggle that allows running without
	// OIDC. We deliberately refuse the old "OIDC_ISSUER_URL unset" heuristic.
	devModeEnv = "COZYTEMPL_DEV_MODE"

	// authModeEnv selects one of the five supported authentication modes
	// at startup. Empty defaults to passthrough when OIDC is configured.
	authModeEnv = "COZYTEMPL_AUTH_MODE"

	// devSessionSecretPlaceholder is the sentinel value stamped in dev mode;
	// production configs must override it. It is intentionally recognisable
	// so Load can refuse to start in prod mode with it still in place.
	devSessionSecretPlaceholder = "dev-secret-not-for-production!!" //nolint:gosec // sentinel value, not a real credential

	// devSessionSecretRandomBytes is the per-process random-secret length
	// used as a dev-mode fallback so sessions survive a single run without
	// being predictable. Picked for crypto-rand output length.
	devSessionSecretRandomBytes = 32

	// envTrue is the single accepted affirmative value for cozytempl's
	// bool env vars. Non-matching values (including "TRUE", "yes", "1")
	// are treated as false — we do not try to be clever about
	// alternative encodings; operators get one well-known string.
	envTrue = "true"
)

// Load reads configuration from environment variables.
//
// Authentication mode is selected by COZYTEMPL_AUTH_MODE; if unset the
// default is passthrough when OIDC is configured, dev when COZYTEMPL_DEV_MODE
// is explicitly set, or an error otherwise. The legacy COZYTEMPL_DEV_MODE=true
// opt-in is still honoured and wins over COZYTEMPL_AUTH_MODE to preserve
// backwards compatibility for existing quickstart setups. A missing
// SESSION_SECRET in non-dev modes is a fatal error — dev mode uses a
// per-process random secret so multiple runs do not share signing keys.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:            envOrDefault("LISTEN_ADDR", ":8080"),
		LogLevel:              envOrDefault("LOG_LEVEL", "info"),
		OIDCIssuerURL:         os.Getenv("OIDC_ISSUER_URL"),
		OIDCInternalIssuerURL: os.Getenv("OIDC_INTERNAL_ISSUER_URL"),
		OIDCClientID:          os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:      os.Getenv("OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:       os.Getenv("OIDC_REDIRECT_URL"),
		SessionSecret:         os.Getenv("SESSION_SECRET"),
		DebugPprofAddr:        os.Getenv("COZYTEMPL_DEBUG_PPROF_ADDR"),
		TrustForwardedHeaders: os.Getenv("COZYTEMPL_TRUST_FORWARDED_HEADERS") == envTrue,
	}

	mode, err := resolveAuthMode(os.Getenv(authModeEnv), os.Getenv(devModeEnv) == envTrue, cfg.OIDCIssuerURL != "")
	if err != nil {
		return nil, err
	}

	cfg.AuthMode = mode
	cfg.DevMode = mode == AuthModeDev

	err = validateForMode(cfg)
	if err != nil {
		return nil, err
	}

	// Dev mode with no SESSION_SECRET → generate a per-process one so
	// a forgotten env var doesn't silently reuse the placeholder.
	if cfg.AuthMode == AuthModeDev && (cfg.SessionSecret == "" || cfg.SessionSecret == devSessionSecretPlaceholder) {
		secret, secretErr := randomSecret(devSessionSecretRandomBytes)
		if secretErr != nil {
			return nil, fmt.Errorf("generating dev session secret: %w", secretErr)
		}

		cfg.SessionSecret = secret
	}

	return cfg, nil
}

// resolveAuthMode picks the effective AuthMode from the explicit
// COZYTEMPL_AUTH_MODE value, the legacy COZYTEMPL_DEV_MODE=true flag, and
// whether OIDC is configured. COZYTEMPL_DEV_MODE=true wins over any
// COZYTEMPL_AUTH_MODE value — this preserves behaviour for operators who
// upgraded without touching their env vars.
func resolveAuthMode(authModeRaw string, devRequested, oidcConfigured bool) (AuthMode, error) {
	if devRequested {
		return AuthModeDev, nil
	}

	if authModeRaw != "" {
		mode, err := ParseAuthMode(authModeRaw)
		if err != nil {
			return "", fmt.Errorf("%s: %w", authModeEnv, err)
		}

		return mode, nil
	}

	if oidcConfigured {
		return AuthModePassthrough, nil
	}

	return "", ErrDevModeNotEnabled
}

// validateForMode checks that the configuration has the env vars the chosen
// AuthMode actually needs. Each mode fails loudly and early rather than
// silently booting into a half-configured state.
func validateForMode(cfg *Config) error {
	switch cfg.AuthMode {
	case AuthModeDev:
		return nil

	case AuthModeBYOK, AuthModeToken:
		// Neither mode talks to an IdP; only the session secret (used to
		// encrypt the stored kubeconfig or bearer token) is required.
		if cfg.SessionSecret == "" || cfg.SessionSecret == devSessionSecretPlaceholder {
			return ErrWeakSessionSecret
		}

		return nil

	case AuthModePassthrough, AuthModeImpersonationLegacy:
		return validateOIDCConfig(cfg)
	}

	return fmt.Errorf("%w: %q", ErrAuthModeUnknown, cfg.AuthMode)
}

// validateOIDCConfig enforces that every OIDC-dependent mode has the full
// set of OIDC env vars and a real session secret.
func validateOIDCConfig(cfg *Config) error {
	if cfg.OIDCIssuerURL == "" {
		return fmt.Errorf("%w: %s", ErrMissingEnvVar, "OIDC_ISSUER_URL")
	}

	required := map[string]string{
		"OIDC_CLIENT_ID":     cfg.OIDCClientID,
		"OIDC_CLIENT_SECRET": cfg.OIDCClientSecret,
		"OIDC_REDIRECT_URL":  cfg.OIDCRedirectURL,
	}

	for name, val := range required {
		if val == "" {
			return fmt.Errorf("%w: %s", ErrMissingEnvVar, name)
		}
	}

	if cfg.SessionSecret == "" || cfg.SessionSecret == devSessionSecretPlaceholder {
		return ErrWeakSessionSecret
	}

	return nil
}

// InternalIssuerURL returns the backend-to-Keycloak URL used for token
// endpoint and JWKS calls. When OIDC_INTERNAL_ISSUER_URL is unset, the
// user-facing OIDC_ISSUER_URL is used for both redirect and token exchange.
func (c *Config) InternalIssuerURL() string {
	if c.OIDCInternalIssuerURL != "" {
		return c.OIDCInternalIssuerURL
	}

	return c.OIDCIssuerURL
}

func envOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}

	return defaultVal
}

func randomSecret(byteLen int) (string, error) {
	buf := make([]byte, byteLen)

	_, err := rand.Read(buf)
	if err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}

	return hex.EncodeToString(buf), nil
}
