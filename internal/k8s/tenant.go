package k8s

import (
	"context"
	"errors"
	"fmt"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

const (
	tenantNamespacePrefix = "tenant-"
	rootTenantName        = "root"
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
	mode    config.AuthMode
}

// NewTenantService creates a new tenant service.
func NewTenantService(baseCfg *rest.Config, mode config.AuthMode) *TenantService {
	return &TenantService{baseCfg: baseCfg, mode: mode}
}

// List returns all tenants visible to the user.
func (tsv *TenantService) List(ctx context.Context, usr *auth.UserContext) ([]Tenant, error) {
	client, err := NewUserClient(tsv.baseCfg, usr, tsv.mode)
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
			LabelSelector: cozyAppKindLabel,
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

// ListMinimal returns the visible tenants with namespace +
// display name + parent only, skipping the per-tenant AppCount
// and ChildCount fan-out List does. One apiserver round-trip
// vs 1+N — meant for callers like the command palette that
// render tenants as navigation rows and never read the counters.
// Writing a separate method instead of a flag keeps the happy
// path of List untouched for existing callers (sidebar,
// dashboard) that do depend on the counters.
func (tsv *TenantService) ListMinimal(ctx context.Context, usr *auth.UserContext) ([]Tenant, error) {
	client, err := NewUserClient(tsv.baseCfg, usr, tsv.mode)
	if err != nil {
		return nil, err
	}

	tenantList, err := client.Resource(TenantCRDGVR()).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing tenants: %w", err)
	}

	tenants := make([]Tenant, 0, len(tenantList.Items))

	for idx := range tenantList.Items {
		tenants = append(tenants, crdToTenant(&tenantList.Items[idx]))
	}

	return tenants, nil
}

// Get returns a single tenant with details.
func (tsv *TenantService) Get(ctx context.Context, usr *auth.UserContext, name string) (*Tenant, error) {
	client, err := NewUserClient(tsv.baseCfg, usr, tsv.mode)
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
		LabelSelector: cozyAppKindLabel,
	})
	if listErr == nil {
		tenant.AppCount = len(hrList.Items)
	}

	return &tenant, nil
}

// findTenantObj resolves a tenant lookup that may be spelled as either the
// short name ("demo"), the workload namespace ("tenant-demo"), or the bare
// prefixed form. The lookup goes in specificity order: exact CR name first,
// then workload namespace (status.namespace), then prefixed name fallback.
// Matching on metadata.namespace is deliberately NOT used — since
// cozystack 1.2 flattened namespace naming, every child CR shares the same
// metadata.namespace as its sibling, so that match would collide.
func findTenantObj(items []unstructured.Unstructured, name string) *unstructured.Unstructured {
	// First pass: exact name match.
	for idx := range items {
		if items[idx].GetName() == name {
			return &items[idx]
		}
	}

	// Second pass: workload namespace match (status.namespace).
	for idx := range items {
		statusNS := nestedString(items[idx].Object, "status", "namespace")
		if statusNS != "" && (statusNS == name || statusNS == tenantNamespacePrefix+name) {
			return &items[idx]
		}
	}

	return nil
}

func findChildren(items []unstructured.Unstructured, parentNS string) []string {
	var children []string

	// A child Tenant CR lives in its parent's workload namespace — i.e.
	// metadata.namespace of the child equals status.namespace of the parent.
	// This is flat since cozystack 1.2 stopped nesting namespace names
	// (tenant "demo" under "tenant-root" is now "tenant-demo", not
	// "tenant-root-demo"), so we identify parent/child by CR location,
	// not by string prefix on the namespace name.
	for idx := range items {
		if items[idx].GetNamespace() == parentNS {
			name := items[idx].GetName()
			// Skip root tenant itself: it lives in its own namespace, which
			// would otherwise make it appear as a child of itself.
			if name != rootTenantName {
				children = append(children, name)
			}
		}
	}

	return children
}

// GetSpecSnapshot returns the raw spec map and resourceVersion of an
// existing Tenant CR. Used by edit flows to pre-populate a
// schema-driven form AND to pin the optimistic-lock token for the
// subsequent Update call — the returned ResourceVersion is echoed
// back through a hidden form field so a concurrent write by another
// user is rejected with 409 instead of silently clobbered.
func (tsv *TenantService) GetSpecSnapshot(
	ctx context.Context, usr *auth.UserContext, namespace, name string,
) (*SpecSnapshot, error) {
	if namespace == "" {
		return nil, ErrNamespaceRequired
	}

	client, err := NewUserClient(tsv.baseCfg, usr, tsv.mode)
	if err != nil {
		return nil, err
	}

	obj, err := client.Resource(TenantCRDGVR()).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting tenant %s/%s: %w", namespace, name, err)
	}

	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")

	return &SpecSnapshot{
		Spec:            spec,
		Kind:            "Tenant",
		ResourceVersion: obj.GetResourceVersion(),
	}, nil
}

// reservedTenantSpecKeys are never legitimate spec fields. They appear in
// stored tenants if an earlier buggy build leaked query/path params into
// the spec via ParseForm, so Update scrubs them on every save.
//
//nolint:gochecknoglobals // small read-only set
var reservedTenantSpecKeys = []string{"ns", "parent", "name"}

// Update merges the given spec fields into an existing Tenant CR.
// The caller specifies the parent namespace (where the CR lives, i.e.
// metadata.namespace) because two tenants can share the same leaf
// name under different parents, just like Delete. The root tenant is
// allowed to update since we don't let users delete it — letting
// them tweak its quotas is safer than making root read-only.
//
// If resourceVersion is non-empty, Update pins it on the outgoing
// object and the API server rejects concurrent writes with 409,
// surfaced as k8s.ErrConflict. Pass "" to opt out of optimistic
// concurrency (last-write-wins, historic behaviour).
func (tsv *TenantService) Update(
	ctx context.Context, usr *auth.UserContext, namespace, name string,
	spec map[string]any, resourceVersion string,
) (*Tenant, error) {
	if namespace == "" {
		return nil, ErrNamespaceRequired
	}

	client, err := NewUserClient(tsv.baseCfg, usr, tsv.mode)
	if err != nil {
		return nil, err
	}

	obj, err := client.Resource(TenantCRDGVR()).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting tenant %s/%s: %w", namespace, name, err)
	}

	// Merge: we don't want to wipe fields the UI didn't render. Nested
	// maps are merged recursively so a partial update that only touches
	// spec.backup.schedule keeps spec.backup.s3SecretKey intact — a
	// shallow merge was a real data-loss bug during E2E, hitting any
	// app or tenant whose schema had a nested block with secrets.
	existing, _, _ := unstructured.NestedMap(obj.Object, "spec")
	if existing == nil {
		existing = map[string]any{}
	}

	// Scrub reserved keys first so earlier bugs that leaked query params
	// into the spec get cleaned up the next time the user saves.
	for _, key := range reservedTenantSpecKeys {
		delete(existing, key)
	}

	merged := deepMergeSpec(existing, spec)

	setErr := unstructured.SetNestedField(obj.Object, merged, "spec")
	if setErr != nil {
		return nil, fmt.Errorf("setting spec: %w", setErr)
	}

	if resourceVersion != "" {
		obj.SetResourceVersion(resourceVersion)
	}

	updated, err := client.Resource(TenantCRDGVR()).Namespace(namespace).Update(ctx, obj, metav1.UpdateOptions{})
	if err != nil {
		if isConflictError(err) {
			return nil, ErrConflict
		}

		return nil, fmt.Errorf("updating tenant %s/%s: %w", namespace, name, err)
	}

	tenant := crdToTenant(updated)

	return &tenant, nil
}

// Create creates a new tenant via the Tenant CRD.
func (tsv *TenantService) Create(ctx context.Context, usr *auth.UserContext, req CreateTenantRequest) (*Tenant, error) {
	client, err := NewUserClient(tsv.baseCfg, usr, tsv.mode)
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
	return nameOrNamespace == rootTenantName || nameOrNamespace == tenantNamespacePrefix+rootTenantName
}

// ErrNamespaceRequired is returned when a delete call is missing the parent namespace.
var ErrNamespaceRequired = errors.New("namespace required")

// Delete removes a tenant via the Tenant CRD.
// The root tenant is protected and cannot be deleted. Both the parent
// namespace (where the Tenant CR lives) and the leaf name are required,
// because Tenant CRs are namespaced — two different tenants can share
// the same leaf name under different parents.
func (tsv *TenantService) Delete(
	ctx context.Context, usr *auth.UserContext, namespace, name string,
) error {
	if IsRootTenant(name) {
		return fmt.Errorf("%w: %s", ErrProtectedTenant, name)
	}

	if namespace == "" {
		return ErrNamespaceRequired
	}

	client, err := NewUserClient(tsv.baseCfg, usr, tsv.mode)
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

	// metadata.namespace is where the CR lives (the parent's workload
	// namespace). For the root tenant, it equals the tenant's own
	// namespace. For any other tenant, it is distinct.
	parentNamespace := obj.GetNamespace()
	namespace := parentNamespace

	// status.namespace is the actual workload namespace created by the
	// Cozystack controller. Since cozystack 1.2 this is flat — a tenant
	// named "demo" becomes "tenant-demo" regardless of parent depth, so
	// the hierarchy can no longer be derived by splitting the namespace
	// name on hyphens (the 1.1-era "tenant-root-demo" scheme is gone).
	statusNS := nestedString(obj.Object, "status", "namespace")
	if statusNS != "" {
		namespace = statusNS
	}

	version := nestedString(obj.Object, "status", "version")

	status := string(extractStatus(obj))
	if status == string(AppStatusReady) {
		status = "Active"
	}

	// Parent = the namespace the CR lives in. That IS the parent tenant's
	// workload namespace by definition (cozystack creates each tenant's
	// children as CRs inside the parent's namespace). The root tenant
	// lives in its own namespace, so Parent equals Namespace in that case
	// and we zero it out to mark it as root.
	parent := parentNamespace
	if parent == namespace {
		parent = ""
	}

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
