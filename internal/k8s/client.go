// Package k8s provides Kubernetes client operations for Cozystack resources.
package k8s

import (
	"fmt"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// clientTimeout caps every k8s API call at 10 seconds. Without this,
// a slow or hung control plane keeps the goroutine parked until the
// request-level 15 s context timeout fires, which is too close to the
// edge — once the ctx is cancelled the client still has to unwind a
// potentially-inflight TCP read. A hard transport timeout below the
// request ceiling keeps us strictly within budget and surfaces slow
// API calls as explicit errors instead of lock-ups.
const clientTimeout = 10 * time.Second

// NewImpersonatingClient creates a dynamic Kubernetes client that impersonates
// the given user and groups. This ensures K8s RBAC is enforced server-side.
func NewImpersonatingClient(baseCfg *rest.Config, username string, groups []string) (dynamic.Interface, error) {
	cfg := rest.CopyConfig(baseCfg)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: username,
		Groups:   groups,
	}
	cfg.Timeout = clientTimeout

	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating impersonating client: %w", err)
	}

	return client, nil
}
