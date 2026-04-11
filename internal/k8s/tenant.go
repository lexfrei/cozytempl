package k8s

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

		objNS := obj.GetNamespace()
		statusNS := nestedString(obj.Object, "status", "namespace")

		if obj.GetName() == name || objNS == name || statusNS == name ||
			objNS == tenantNamespacePrefix+name || statusNS == tenantNamespacePrefix+name {
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

// Update merges the given spec fields into an existing Tenant CR. The caller
// specifies the parent namespace (where the CR lives, i.e. metadata.namespace)
// because two tenants can share the same leaf name under different parents,
// just like Delete. The root tenant is allowed to update since we don't let
// users delete it — letting them tweak its quotas is safer than making root
// read-only and having escape-hatch editing happen in kubectl.
func (tsv *TenantService) Update(
	ctx context.Context, username string, groups []string, namespace, name string, spec map[string]any,
) (*Tenant, error) {
	if namespace == "" {
		return nil, ErrNamespaceRequired
	}

	client, err := NewImpersonatingClient(tsv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	obj, err := client.Resource(TenantCRDGVR()).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting tenant %s/%s: %w", namespace, name, err)
	}

	// Merge: we don't want to wipe fields the UI didn't render. Only the
	// keys present in the incoming spec map are set; missing keys keep
	// their current value. This matches the schema-driven form where
	// leaving a field blank means "don't change".
	existing, _, _ := unstructured.NestedMap(obj.Object, "spec")
	if existing == nil {
		existing = map[string]any{}
	}

	maps.Copy(existing, spec)

	setErr := unstructured.SetNestedField(obj.Object, existing, "spec")
	if setErr != nil {
		return nil, fmt.Errorf("setting spec: %w", setErr)
	}

	updated, err := client.Resource(TenantCRDGVR()).Namespace(namespace).Update(ctx, obj, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("updating tenant %s/%s: %w", namespace, name, err)
	}

	tenant := crdToTenant(updated)

	return &tenant, nil
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

// ErrProtectedTenant is returned when someone tries to delete the root tenant.
var ErrProtectedTenant = errors.New("tenant is protected")

// IsRootTenant reports whether the given name or namespace refers to root.
func IsRootTenant(nameOrNamespace string) bool {
	return nameOrNamespace == "root" || nameOrNamespace == tenantNamespacePrefix+"root"
}

// ErrNamespaceRequired is returned when a delete call is missing the parent namespace.
var ErrNamespaceRequired = errors.New("namespace required")

// Delete removes a tenant via the Tenant CRD.
// The root tenant is protected and cannot be deleted. Both the parent
// namespace (where the Tenant CR lives) and the leaf name are required,
// because Tenant CRs are namespaced — two different tenants can share
// the same leaf name under different parents.
func (tsv *TenantService) Delete(
	ctx context.Context, username string, groups []string, namespace, name string,
) error {
	if IsRootTenant(name) {
		return fmt.Errorf("%w: %s", ErrProtectedTenant, name)
	}

	if namespace == "" {
		return ErrNamespaceRequired
	}

	client, err := NewImpersonatingClient(tsv.baseCfg, username, groups)
	if err != nil {
		return err
	}

	delErr := client.Resource(TenantCRDGVR()).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if delErr != nil {
		return fmt.Errorf("deleting tenant %s/%s: %w", namespace, name, delErr)
	}

	return nil
}

func crdToTenant(obj *unstructured.Unstructured) Tenant {
	name := obj.GetName()

	// metadata.namespace is where the CR lives (parent's namespace).
	// For root, it equals the tenant's own namespace.
	parentNamespace := obj.GetNamespace()
	namespace := parentNamespace

	// status.namespace is the actual workload namespace created by the controller.
	statusNS := nestedString(obj.Object, "status", "namespace")
	if statusNS != "" {
		namespace = statusNS
	}

	version := nestedString(obj.Object, "status", "version")

	status := string(extractStatus(obj))
	if status == string(AppStatusReady) {
		status = "Active"
	}

	// Determine logical parent from namespace hierarchy
	parent := parentFromNamespace(namespace)

	return Tenant{
		Name:            name,
		Namespace:       namespace,
		ParentNamespace: parentNamespace,
		DisplayName:     name,
		Parent:          parent,
		Status:          status,
		Version:         version,
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
