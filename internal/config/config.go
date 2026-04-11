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
	ListenAddr       string
	OIDCIssuerURL    string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCRedirectURL  string
	SessionSecret    string
	LogLevel         string
	DevMode          bool
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

const (
	// devModeEnv is the explicit opt-in toggle that allows running without
	// OIDC. We deliberately refuse the old "OIDC_ISSUER_URL unset" heuristic.
	devModeEnv = "COZYTEMPL_DEV_MODE"

	// devSessionSecretPlaceholder is the sentinel value stamped in dev mode;
	// production configs must override it. It is intentionally recognisable
	// so Load can refuse to start in prod mode with it still in place.
	devSessionSecretPlaceholder = "dev-secret-not-for-production!!" //nolint:gosec // sentinel value, not a real credential

	// devSessionSecretRandomBytes is the per-process random-secret length
	// used as a dev-mode fallback so sessions survive a single run without
	// being predictable. Picked for crypto-rand output length.
	devSessionSecretRandomBytes = 32
)

// Load reads configuration from environment variables.
// Dev mode requires an explicit COZYTEMPL_DEV_MODE=true opt-in; just leaving
// OIDC unset will NOT silently disable auth. A missing SESSION_SECRET in
// production mode is a fatal error — dev mode uses a per-process random
// secret so multiple runs do not share signing keys.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:       envOrDefault("LISTEN_ADDR", ":8080"),
		LogLevel:         envOrDefault("LOG_LEVEL", "info"),
		OIDCIssuerURL:    os.Getenv("OIDC_ISSUER_URL"),
		OIDCClientID:     os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:  os.Getenv("OIDC_REDIRECT_URL"),
		SessionSecret:    os.Getenv("SESSION_SECRET"),
	}

	devRequested := os.Getenv(devModeEnv) == "true"

	if cfg.OIDCIssuerURL == "" {
		if !devRequested {
			return nil, ErrDevModeNotEnabled
		}

		cfg.DevMode = true

		// Dev mode with no SESSION_SECRET → generate a per-process one so
		// a forgotten env var doesn't silently reuse the placeholder.
		if cfg.SessionSecret == "" || cfg.SessionSecret == devSessionSecretPlaceholder {
			secret, err := randomSecret(devSessionSecretRandomBytes)
			if err != nil {
				return nil, fmt.Errorf("generating dev session secret: %w", err)
			}

			cfg.SessionSecret = secret
		}

		return cfg, nil
	}

	// Production: OIDC configured. All OIDC vars and a real session secret
	// are required. The placeholder value is treated as if unset.
	required := map[string]string{
		"OIDC_CLIENT_ID":     cfg.OIDCClientID,
		"OIDC_CLIENT_SECRET": cfg.OIDCClientSecret,
		"OIDC_REDIRECT_URL":  cfg.OIDCRedirectURL,
	}

	for name, val := range required {
		if val == "" {
			return nil, fmt.Errorf("%w: %s", ErrMissingEnvVar, name)
		}
	}

	if cfg.SessionSecret == "" || cfg.SessionSecret == devSessionSecretPlaceholder {
		return nil, ErrWeakSessionSecret
	}

	return cfg, nil
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
