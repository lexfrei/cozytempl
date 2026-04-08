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

const tenantNamespacePrefix = "tenant-"

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

// TenantCRDGVR returns the GVR for the Cozystack Tenant CRD.
func TenantCRDGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    cozyAppGroup,
		Version:  "v1alpha1",
		Resource: "tenants",
	}
}

// TenantService provides operations on Cozystack tenants
// via the apps.cozystack.io/v1alpha1 Tenant CRD.
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

	tenantList, err := client.Resource(TenantCRDGVR()).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing tenants: %w", err)
	}

	tenants := make([]Tenant, 0, len(tenantList.Items))

	for idx := range tenantList.Items {
		tenant := crdToTenant(&tenantList.Items[idx])
		tenants = append(tenants, tenant)
	}

	// Count apps per tenant via HelmReleases
	hrGVR := HelmReleaseGVR()

	for idx := range tenants {
		hrList, listErr := client.Resource(hrGVR).Namespace(tenants[idx].Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "apps.cozystack.io/application.kind",
		})
		if listErr == nil {
			tenants[idx].AppCount = len(hrList.Items)
		}
	}

	// Build hierarchy: count children per tenant
	childCounts := make(map[string]int)

	for idx := range tenants {
		if tenants[idx].Parent != "" {
			childCounts[tenants[idx].Name]++
		}
	}

	for idx := range tenants {
		tenants[idx].ChildCount = childCounts[tenants[idx].Name]
	}

	return tenants, nil
}

// Get returns a single tenant with details.
func (tsv *TenantService) Get(ctx context.Context, username string, groups []string, name string) (*Tenant, error) {
	client, err := NewImpersonatingClient(tsv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	tenantList, err := client.Resource(TenantCRDGVR()).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing tenants: %w", err)
	}

	obj := findTenantObj(tenantList.Items, name)
	if obj == nil {
		return nil, fmt.Errorf("%w: %s", ErrAppNotFound, name)
	}

	tenant := crdToTenant(obj)
	tenant.Children = findChildren(tenantList.Items, tenant.Namespace)
	tenant.ChildCount = len(tenant.Children)

	hrList, listErr := client.Resource(HelmReleaseGVR()).Namespace(tenant.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "apps.cozystack.io/application.kind",
	})
	if listErr == nil {
		tenant.AppCount = len(hrList.Items)
	}

	return &tenant, nil
}

func findTenantObj(items []unstructured.Unstructured, name string) *unstructured.Unstructured {
	for idx := range items {
		obj := &items[idx]
		if obj.GetName() == name || obj.GetNamespace() == tenantNamespacePrefix+name {
			return obj
		}
	}

	return nil
}

func findChildren(items []unstructured.Unstructured, parentNS string) []string {
	var children []string

	for idx := range items {
		childNS := nestedString(items[idx].Object, "status", "namespace")
		if childNS != "" && strings.HasPrefix(childNS, parentNS+"-") {
			children = append(children, items[idx].GetName())
		}
	}

	return children
}

// Create creates a new tenant via the Tenant CRD.
func (tsv *TenantService) Create(ctx context.Context, username string, groups []string, req CreateTenantRequest) (*Tenant, error) {
	client, err := NewImpersonatingClient(tsv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	parentNS := req.Parent
	if parentNS == "" {
		parentNS = tenantNamespacePrefix + "root"
	}

	spec := req.Spec
	if spec == nil {
		spec = map[string]any{}
	}

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": cozyAppGroup + "/v1alpha1",
			"kind":       "Tenant",
			"metadata": map[string]any{
				"name":      req.Name,
				"namespace": parentNS,
			},
			"spec": spec,
		},
	}

	created, err := client.Resource(TenantCRDGVR()).Namespace(parentNS).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating tenant: %w", err)
	}

	tenant := crdToTenant(created)

	return &tenant, nil
}

// Delete removes a tenant via the Tenant CRD.
func (tsv *TenantService) Delete(ctx context.Context, username string, groups []string, name string) error {
	client, err := NewImpersonatingClient(tsv.baseCfg, username, groups)
	if err != nil {
		return err
	}

	// Find the tenant to get its namespace
	tenantList, err := client.Resource(TenantCRDGVR()).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing tenants for delete: %w", err)
	}

	for idx := range tenantList.Items {
		obj := &tenantList.Items[idx]
		if obj.GetName() == name {
			delErr := client.Resource(TenantCRDGVR()).Namespace(obj.GetNamespace()).Delete(ctx, name, metav1.DeleteOptions{})
			if delErr != nil {
				return fmt.Errorf("deleting tenant %s: %w", name, delErr)
			}

			return nil
		}
	}

	return fmt.Errorf("%w: %s", ErrAppNotFound, name)
}

func crdToTenant(obj *unstructured.Unstructured) Tenant {
	name := obj.GetName()
	namespace := obj.GetNamespace()

	// status.namespace is the actual tenant namespace created by the controller
	statusNS := nestedString(obj.Object, "status", "namespace")
	if statusNS != "" {
		namespace = statusNS
	}

	version := nestedString(obj.Object, "status", "version")

	status := string(extractStatus(obj))
	if status == string(AppStatusReady) {
		status = "Active"
	}

	// Determine parent from namespace hierarchy
	parent := parentFromNamespace(namespace)

	return Tenant{
		Name:        name,
		Namespace:   namespace,
		DisplayName: name,
		Parent:      parent,
		Status:      status,
		Version:     version,
	}
}

const minTenantNameParts = 2

func parentFromNamespace(namespace string) string {
	parts := strings.Split(namespace, "-")
	if len(parts) <= minTenantNameParts {
		return ""
	}

	return strings.Join(parts[:len(parts)-1], "-")
}
