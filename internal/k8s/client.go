// Package k8s provides Kubernetes client operations for Cozystack resources.
package k8s

import (
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// NewImpersonatingClient creates a dynamic Kubernetes client that impersonates
// the given user and groups. This ensures K8s RBAC is enforced server-side.
func NewImpersonatingClient(baseCfg *rest.Config, username string, groups []string) (dynamic.Interface, error) {
	cfg := rest.CopyConfig(baseCfg)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: username,
		Groups:   groups,
	}

	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating impersonating client: %w", err)
	}

	return client, nil
}
