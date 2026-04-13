// Package k8s provides Kubernetes client operations for Cozystack resources.
package k8s

import (
	"errors"
	"fmt"
	"time"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// clientTimeout caps every k8s API call at 10 seconds. Without this,
// a slow or hung control plane keeps the goroutine parked until the
// request-level 15 s context timeout fires, which is too close to the
// edge — once the ctx is cancelled the client still has to unwind a
// potentially-inflight TCP read. A hard transport timeout below the
// request ceiling keeps us strictly within budget and surfaces slow
// API calls as explicit errors instead of lock-ups.
const clientTimeout = 10 * time.Second

// ErrUnknownAuthMode is returned when buildUserRESTConfig sees an
// AuthMode value it was not told how to handle. Indicates a wiring
// error in the caller rather than a user-actionable condition.
var ErrUnknownAuthMode = errors.New("unknown auth mode")

// NewUserClient builds a dynamic Kubernetes client that acts on
// behalf of the user described by usr. The exact authentication
// vehicle depends on mode:
//
//   - passthrough:        Bearer = usr.IDToken (k8s API validates via OIDC).
//   - byok:               rest.Config derived from usr.KubeconfigBytes.
//   - token:              Bearer = usr.BearerToken (apiserver URL/CA from baseCfg).
//   - impersonation-legacy: Impersonate headers from Username / Groups.
//   - dev:                baseCfg unchanged (effectively cozytempl's own SA).
//
// Every resulting config has its transport Timeout pinned to
// clientTimeout so a hung control plane cannot starve a goroutine.
func NewUserClient(baseCfg *rest.Config, usr *auth.UserContext, mode config.AuthMode) (dynamic.Interface, error) {
	cfg, err := buildUserRESTConfig(baseCfg, usr, mode)
	if err != nil {
		return nil, err
	}

	cfg.Timeout = clientTimeout

	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating user client: %w", err)
	}

	return client, nil
}

// buildUserRESTConfig returns a *rest.Config that carries the right
// authentication data for mode. Callers that need more than a dynamic
// client (e.g. the log streaming endpoint which needs a kubernetes
// clientset) can reuse this helper directly and layer their own
// concrete client on top.
func buildUserRESTConfig(baseCfg *rest.Config, usr *auth.UserContext, mode config.AuthMode) (*rest.Config, error) {
	switch mode {
	case config.AuthModePassthrough:
		cfg := rest.CopyConfig(baseCfg)
		cfg.BearerToken = usr.IDToken
		cfg.BearerTokenFile = ""
		cfg.Impersonate = rest.ImpersonationConfig{}

		return cfg, nil

	case config.AuthModeBYOK:
		// The uploaded kubeconfig IS the identity — cozytempl's own
		// baseCfg is not involved. Reject exec-plugin kubeconfigs at
		// upload time (see internal/auth/handler.go) so this path
		// only ever sees bearer / cert auth.
		cfg, err := clientcmd.RESTConfigFromKubeConfig(usr.KubeconfigBytes)
		if err != nil {
			return nil, fmt.Errorf("building rest config from uploaded kubeconfig: %w", err)
		}

		return cfg, nil

	case config.AuthModeToken:
		// The pasted Bearer token IS the identity. Apiserver URL and
		// CA come from baseCfg (in-cluster); BearerTokenFile is
		// cleared explicitly so client-go does not fall back to the
		// pod's mounted SA token.
		cfg := rest.CopyConfig(baseCfg)
		cfg.BearerToken = usr.BearerToken
		cfg.BearerTokenFile = ""
		cfg.Impersonate = rest.ImpersonationConfig{}

		return cfg, nil

	case config.AuthModeImpersonationLegacy:
		cfg := rest.CopyConfig(baseCfg)
		cfg.Impersonate = rest.ImpersonationConfig{
			UserName: usr.Username,
			Groups:   usr.Groups,
		}

		return cfg, nil

	case config.AuthModeDev:
		// Dev: cozytempl's own process credentials. No impersonation,
		// no token injection — whatever baseCfg carries (InCluster SA,
		// ~/.kube/config) is what the k8s apiserver sees.
		return rest.CopyConfig(baseCfg), nil
	}

	return nil, fmt.Errorf("%w: %q", ErrUnknownAuthMode, mode)
}
