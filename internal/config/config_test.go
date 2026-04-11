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

	if cfg.DevMode {
		t.Error("DevMode should be false when OIDC vars are set")
	}
}

func TestLoad_DevMode(t *testing.T) {
	clearEnvVars(t)
	t.Setenv("COZYTEMPL_DEV_MODE", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error in dev mode: %v", err)
	}

	if !cfg.DevMode {
		t.Error("DevMode should be true when COZYTEMPL_DEV_MODE=true is set")
	}

	if cfg.SessionSecret == "" || cfg.SessionSecret == devSessionSecretPlaceholder {
		t.Error("dev mode should generate a random session secret, got placeholder or empty")
	}
}

func TestLoad_DevModeRequiresOptIn(t *testing.T) {
	clearEnvVars(t)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when OIDC_ISSUER_URL is unset without COZYTEMPL_DEV_MODE opt-in")
	}
}

func TestLoad_ProductionRejectsPlaceholderSecret(t *testing.T) {
	setRequiredEnvVars(t)
	t.Setenv("SESSION_SECRET", devSessionSecretPlaceholder)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when production mode uses placeholder SESSION_SECRET")
	}
}

func TestLoad_ProductionRequiresSessionSecret(t *testing.T) {
	setRequiredEnvVars(t)
	t.Setenv("SESSION_SECRET", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when production mode has empty SESSION_SECRET")
	}
}

func TestLoad_PartialOIDC_MissingClientID(t *testing.T) {
	clearEnvVars(t)
	t.Setenv("OIDC_ISSUER_URL", "https://keycloak.example.com/realms/test")
	// OIDC_CLIENT_ID not set — should error because issuer is set but client ID is missing

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when OIDC_ISSUER_URL is set but OIDC_CLIENT_ID is missing")
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
