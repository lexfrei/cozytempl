package config

import (
	"os"
	"testing"
)

func TestLoad_AllRequiredSet(t *testing.T) {
	setRequiredEnvVars(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.OIDCIssuerURL != "https://keycloak.example.com/realms/test" {
		t.Errorf("OIDCIssuerURL = %q, want %q", cfg.OIDCIssuerURL, "https://keycloak.example.com/realms/test")
	}

	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, ":8080")
	}
}

func TestLoad_MissingRequired(t *testing.T) {
	clearEnvVars(t)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing required vars, got nil")
	}
}

func TestLoad_CustomListenAddr(t *testing.T) {
	setRequiredEnvVars(t)
	t.Setenv("LISTEN_ADDR", ":9090")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, ":9090")
	}
}

func setRequiredEnvVars(t *testing.T) {
	t.Helper()
	clearEnvVars(t)
	t.Setenv("OIDC_ISSUER_URL", "https://keycloak.example.com/realms/test")
	t.Setenv("OIDC_CLIENT_ID", "cozytempl")
	t.Setenv("OIDC_CLIENT_SECRET", "test-secret")
	t.Setenv("OIDC_REDIRECT_URL", "http://localhost:8080/auth/callback")
	t.Setenv("SESSION_SECRET", "test-session-secret-32bytes-long!")
}

func clearEnvVars(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"OIDC_ISSUER_URL", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET",
		"OIDC_REDIRECT_URL", "SESSION_SECRET", "LISTEN_ADDR", "LOG_LEVEL",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}
