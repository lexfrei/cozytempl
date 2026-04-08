package k8s

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

const (
	tenantNamespacePrefix = "tenant-"
	minTenantParts        = 2
)

// NamespaceGVR returns the GVR for core namespaces.
func NamespaceGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "namespaces",
	}
}

// HelmReleaseGVR returns the GVR for FluxCD HelmReleases.
func HelmReleaseGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "helm.toolkit.fluxcd.io",
		Version:  "v2",
		Resource: "helmreleases",
	}
}

// TenantService provides operations on Cozystack tenants.
type TenantService struct {
	baseCfg *rest.Config
}

// NewTenantService creates a new tenant service.
func NewTenantService(baseCfg *rest.Config) *TenantService {
	return &TenantService{baseCfg: baseCfg}
}

// List returns all tenants visible to the user.
func (tsv *TenantService) List(ctx context.Context, username string, groups []string) ([]Tenant, error) {
	client, err := NewImpersonatingClient(tsv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	nsGVR := NamespaceGVR()
	hrGVR := HelmReleaseGVR()

	nsList, err := client.Resource(nsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}

	tenants := make([]Tenant, 0)

	for idx := range nsList.Items {
		ns := &nsList.Items[idx]
		name := ns.GetName()

		if !strings.HasPrefix(name, tenantNamespacePrefix) {
			continue
		}

		tenant := tenantFromNamespace(name)
		tenants = append(tenants, tenant)
	}

	// Count apps per tenant
	for idx := range tenants {
		hrList, listErr := client.Resource(hrGVR).Namespace(tenants[idx].Name).List(ctx, metav1.ListOptions{
			LabelSelector: "apps.cozystack.io/application.kind",
		})
		if listErr == nil {
			tenants[idx].AppCount = len(hrList.Items)
		}
	}

	// Build hierarchy
	childCounts := make(map[string]int)

	for idx := range tenants {
		if tenants[idx].Parent != "" {
			childCounts[tenants[idx].Parent]++
		}
	}

	for idx := range tenants {
		tenants[idx].ChildCount = childCounts[tenants[idx].Name]
	}

	return tenants, nil
}

// Get returns a single tenant with its children and apps.
func (tsv *TenantService) Get(ctx context.Context, username string, groups []string, name string) (*Tenant, error) {
	client, err := NewImpersonatingClient(tsv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	nsGVR := NamespaceGVR()
	hrGVR := HelmReleaseGVR()

	_, err = client.Resource(nsGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting namespace %s: %w", name, err)
	}

	tenant := tenantFromNamespace(name)

	// Find children
	nsList, err := client.Resource(nsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces for children: %w", err)
	}

	for idx := range nsList.Items {
		childName := nsList.Items[idx].GetName()
		if childName != name && strings.HasPrefix(childName, name+"-") {
			tenant.Children = append(tenant.Children, childName)
		}
	}

	tenant.ChildCount = len(tenant.Children)

	// Count apps
	hrList, err := client.Resource(hrGVR).Namespace(name).List(ctx, metav1.ListOptions{
		LabelSelector: "apps.cozystack.io/application.kind",
	})
	if err == nil {
		tenant.AppCount = len(hrList.Items)
	}

	tenant.Status = "Active"

	return &tenant, nil
}

// Create creates a new tenant by creating a HelmRelease in the parent namespace.
func (tsv *TenantService) Create(ctx context.Context, username string, groups []string, req CreateTenantRequest) (*Tenant, error) {
	client, err := NewImpersonatingClient(tsv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	parentNS := req.Parent
	if parentNS == "" {
		parentNS = tenantNamespacePrefix + "root"
	}

	tenantName := tenantNamespacePrefix + req.Name
	hrGVR := HelmReleaseGVR()

	helmRelease := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "helm.toolkit.fluxcd.io/v2",
			"kind":       "HelmRelease",
			"metadata": map[string]any{
				"name":      tenantName,
				"namespace": parentNS,
				"labels": map[string]any{
					"apps.cozystack.io/application.kind": "Tenant",
					"apps.cozystack.io/application.name": tenantName,
				},
			},
			"spec": map[string]any{
				"chartRef": map[string]any{
					"kind":      "ExternalArtifact",
					"name":      "cozystack-tenant-application-default-tenant",
					"namespace": "cozy-system",
				},
				"interval": "5m",
				"timeout":  "10m",
			},
		},
	}

	_, err = client.Resource(hrGVR).Namespace(parentNS).Create(ctx, helmRelease, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating tenant HelmRelease: %w", err)
	}

	tenant := tenantFromNamespace(tenantName)
	tenant.Status = "Reconciling"

	return &tenant, nil
}

// Delete removes a tenant by deleting its HelmRelease.
func (tsv *TenantService) Delete(ctx context.Context, username string, groups []string, name string) error {
	client, err := NewImpersonatingClient(tsv.baseCfg, username, groups)
	if err != nil {
		return err
	}

	parent := parentNamespace(name)
	hrGVR := HelmReleaseGVR()

	err = client.Resource(hrGVR).Namespace(parent).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("deleting tenant HelmRelease: %w", err)
	}

	return nil
}

func tenantFromNamespace(name string) Tenant {
	displayName := strings.TrimPrefix(name, tenantNamespacePrefix)
	parent := parentNamespace(name)

	return Tenant{
		Name:        name,
		DisplayName: displayName,
		Parent:      parent,
		Status:      "Active",
	}
}

func parentNamespace(name string) string {
	// "tenant-root-team1" has parent "tenant-root"; "tenant-root" has no parent.
	parts := strings.Split(name, "-")
	if len(parts) <= minTenantParts {
		return ""
	}

	return strings.Join(parts[:len(parts)-1], "-")
}
