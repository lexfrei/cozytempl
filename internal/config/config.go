// Package config provides environment-based application configuration.
package config

import (
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

// Load reads configuration from environment variables.
// If OIDC variables are not set, enables dev mode (no auth).
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:       envOrDefault("LISTEN_ADDR", ":8080"),
		LogLevel:         envOrDefault("LOG_LEVEL", "info"),
		OIDCIssuerURL:    os.Getenv("OIDC_ISSUER_URL"),
		OIDCClientID:     os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:  os.Getenv("OIDC_REDIRECT_URL"),
		SessionSecret:    envOrDefault("SESSION_SECRET", "dev-secret-not-for-production!!"),
	}

	if cfg.OIDCIssuerURL == "" {
		cfg.DevMode = true

		return cfg, nil
	}

	// In production mode, all OIDC vars are required
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

	return cfg, nil
}

func envOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}

	return defaultVal
}
