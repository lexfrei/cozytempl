package k8s

import (
	"errors"
	"testing"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	"k8s.io/client-go/rest"
)

// validKubeconfig is the smallest self-contained kubeconfig that
// clientcmd.RESTConfigFromKubeConfig will accept. Used to exercise the
// byok branch without touching the filesystem or a real cluster.
const validKubeconfig = `apiVersion: v1
kind: Config
current-context: test
contexts:
- name: test
  context:
    cluster: test
    user: test
clusters:
- name: test
  cluster:
    server: https://k8s.example.com
users:
- name: test
  user:
    token: abc123
`

func TestBuildUserRESTConfig_Passthrough(t *testing.T) {
	t.Parallel()

	base := &rest.Config{Host: "https://base.example.com"}
	usr := &auth.UserContext{
		Username: "alice",
		Groups:   []string{"grp"},
		IDToken:  "eyJfake.token",
	}

	cfg, err := buildUserRESTConfig(base, usr, config.AuthModePassthrough)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Host != base.Host {
		t.Errorf("Host = %q, want %q", cfg.Host, base.Host)
	}

	if cfg.BearerToken != usr.IDToken {
		t.Errorf("BearerToken = %q, want %q", cfg.BearerToken, usr.IDToken)
	}

	if cfg.Impersonate.UserName != "" {
		t.Errorf("Impersonate.UserName = %q, want empty", cfg.Impersonate.UserName)
	}

	if len(cfg.Impersonate.Groups) != 0 {
		t.Errorf("Impersonate.Groups = %v, want empty", cfg.Impersonate.Groups)
	}
}

func TestBuildUserRESTConfig_ImpersonationLegacy(t *testing.T) {
	t.Parallel()

	base := &rest.Config{Host: "https://base.example.com"}
	usr := &auth.UserContext{
		Username: "alice",
		Groups:   []string{"tenant-root-admin", "dev"},
	}

	cfg, err := buildUserRESTConfig(base, usr, config.AuthModeImpersonationLegacy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Impersonate.UserName != usr.Username {
		t.Errorf("Impersonate.UserName = %q, want %q", cfg.Impersonate.UserName, usr.Username)
	}

	if len(cfg.Impersonate.Groups) != len(usr.Groups) {
		t.Fatalf("Impersonate.Groups = %v, want %v", cfg.Impersonate.Groups, usr.Groups)
	}

	for i := range usr.Groups {
		if cfg.Impersonate.Groups[i] != usr.Groups[i] {
			t.Errorf("Impersonate.Groups[%d] = %q, want %q", i, cfg.Impersonate.Groups[i], usr.Groups[i])
		}
	}

	if cfg.BearerToken != "" {
		t.Errorf("BearerToken = %q, want empty in legacy mode", cfg.BearerToken)
	}
}

func TestBuildUserRESTConfig_BYOK(t *testing.T) {
	t.Parallel()

	// baseCfg is intentionally bogus — byok must NOT read from it.
	base := &rest.Config{Host: "https://should-not-be-used"}
	usr := &auth.UserContext{
		Username:        "byok-user",
		KubeconfigBytes: []byte(validKubeconfig),
	}

	cfg, err := buildUserRESTConfig(base, usr, config.AuthModeBYOK)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Host != "https://k8s.example.com" {
		t.Errorf("Host = %q, want %q (from uploaded kubeconfig, not baseCfg)", cfg.Host, "https://k8s.example.com")
	}

	if cfg.BearerToken != "abc123" {
		t.Errorf("BearerToken = %q, want %q (from uploaded kubeconfig)", cfg.BearerToken, "abc123")
	}
}

func TestBuildUserRESTConfig_BYOK_Invalid(t *testing.T) {
	t.Parallel()

	base := &rest.Config{}
	usr := &auth.UserContext{KubeconfigBytes: []byte("not a kubeconfig at all")}

	_, err := buildUserRESTConfig(base, usr, config.AuthModeBYOK)
	if err == nil {
		t.Fatal("expected error for invalid kubeconfig")
	}
}

func TestBuildUserRESTConfig_Dev(t *testing.T) {
	t.Parallel()

	base := &rest.Config{Host: "https://dev.example.com", BearerToken: "dev-token"}
	usr := &auth.UserContext{Username: "dev-admin"}

	cfg, err := buildUserRESTConfig(base, usr, config.AuthModeDev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Host != base.Host {
		t.Errorf("Host = %q, want %q", cfg.Host, base.Host)
	}

	if cfg.BearerToken != base.BearerToken {
		t.Errorf("BearerToken = %q, want %q (dev mode preserves baseCfg)", cfg.BearerToken, base.BearerToken)
	}
}

func TestBuildUserRESTConfig_Token(t *testing.T) {
	t.Parallel()

	base := &rest.Config{
		Host:            "https://k.example.com",
		BearerToken:     "sa-token",
		BearerTokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
	}
	usr := &auth.UserContext{
		Username:    "token-user",
		BearerToken: "user-pasted-token",
	}

	cfg, err := buildUserRESTConfig(base, usr, config.AuthModeToken)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Host != base.Host {
		t.Errorf("Host = %q, want %q (token mode keeps base host)", cfg.Host, base.Host)
	}

	if cfg.BearerToken != "user-pasted-token" {
		t.Errorf("BearerToken = %q, want %q", cfg.BearerToken, "user-pasted-token")
	}

	if cfg.BearerTokenFile != "" {
		t.Errorf("BearerTokenFile = %q, want empty (must not fall back to SA token file)", cfg.BearerTokenFile)
	}

	// Mutating the returned config must not touch baseCfg — buildUserRESTConfig
	// owes the caller an isolated copy.
	if base.BearerToken != "sa-token" {
		t.Errorf("baseCfg mutated: BearerToken = %q", base.BearerToken)
	}
}

func TestBuildUserRESTConfig_Unknown(t *testing.T) {
	t.Parallel()

	base := &rest.Config{}
	usr := &auth.UserContext{}

	_, err := buildUserRESTConfig(base, usr, config.AuthMode("made-up"))
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}

	if !errors.Is(err, ErrUnknownAuthMode) {
		t.Errorf("error = %v, want wrapping ErrUnknownAuthMode", err)
	}
}
