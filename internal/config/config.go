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
}

// ErrMissingEnvVar is returned when a required environment variable is not set.
var ErrMissingEnvVar = errors.New("required environment variable is not set")

// Load reads configuration from environment variables.
// Required variables must be set; optional ones have defaults.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr: envOrDefault("LISTEN_ADDR", ":8080"),
		LogLevel:   envOrDefault("LOG_LEVEL", "info"),
	}

	required := map[string]*string{
		"OIDC_ISSUER_URL":    &cfg.OIDCIssuerURL,
		"OIDC_CLIENT_ID":     &cfg.OIDCClientID,
		"OIDC_CLIENT_SECRET": &cfg.OIDCClientSecret,
		"OIDC_REDIRECT_URL":  &cfg.OIDCRedirectURL,
		"SESSION_SECRET":     &cfg.SessionSecret,
	}

	for name, ptr := range required {
		val := os.Getenv(name)
		if val == "" {
			return nil, fmt.Errorf("%w: %s", ErrMissingEnvVar, name)
		}

		*ptr = val
	}

	return cfg, nil
}

func envOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}

	return defaultVal
}
