package k8s

import (
	"context"
	"fmt"
	"sort"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// capiClusterLabel is the standard Cluster API label every
// MachineDeployment carries pointing back at its parent Cluster. We
// never read the OwnerReference chain because Cluster API sets this
// label on creation and our list+filter is both cheaper and simpler
// than walking owners.
const capiClusterLabel = "cluster.x-k8s.io/cluster-name"

// machineDeploymentsGVR pins the CAPI group/version we talk to. CAPI
// has been on v1beta1 since ~2021 and Cozystack ships a CAPI that
// honours this contract; if upstream bumps the version we'll update
// the constant rather than discovering it dynamically.
func machineDeploymentsGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "cluster.x-k8s.io",
		Version:  "v1beta1",
		Resource: "machinedeployments",
	}
}

// NodeGroup is a simplified view of a CAPI MachineDeployment —
// enough to render "3 desired, 2 ready" on a cluster detail page
// without dragging the full unstructured shape into the template.
//
// The four counters mirror the CAPI status fields one-to-one. When
// the apiserver hasn't populated a field yet (e.g. a brand-new MD),
// the unset int64 zero value is what the user sees, which reads
// naturally as "0 ready" and matches what kubectl shows.
type NodeGroup struct {
	Name            string
	DesiredReplicas int64
	StatusReplicas  int64
	ReadyReplicas   int64
	UpdatedReplicas int64
	Phase           string
}

// CAPIService reads Cluster API resources visible to the current
// user. Used by the Kubernetes application detail page to show
// actual vs. desired node counts while a node group is scaling.
//
// Callers with no CAPI CRDs installed on the target cluster see a
// clean "NotFound" / empty-list path rather than an error — the
// Kubernetes app type is a no-op in that environment anyway.
type CAPIService struct {
	baseCfg *rest.Config
	mode    config.AuthMode
}

// NewCAPIService creates a new CAPI service.
func NewCAPIService(baseCfg *rest.Config, mode config.AuthMode) *CAPIService {
	return &CAPIService{baseCfg: baseCfg, mode: mode}
}

// ListMachineDeploymentsForCluster returns every MachineDeployment
// in namespace that carries the standard CAPI cluster-name label
// pointing at clusterName. Order is stable (by Name) so the UI
// doesn't shuffle rows between renders.
func (svc *CAPIService) ListMachineDeploymentsForCluster(
	ctx context.Context, usr *auth.UserContext, namespace, clusterName string,
) ([]NodeGroup, error) {
	if namespace == "" {
		return nil, ErrNamespaceRequired
	}

	if clusterName == "" {
		return nil, nil
	}

	client, err := NewUserClient(svc.baseCfg, usr, svc.mode)
	if err != nil {
		return nil, err
	}

	selector := capiClusterLabel + "=" + clusterName

	list, listErr := client.Resource(machineDeploymentsGVR()).
		Namespace(namespace).
		List(ctx, metav1.ListOptions{LabelSelector: selector})
	if listErr != nil {
		return nil, fmt.Errorf("listing MachineDeployments for cluster %s/%s: %w",
			namespace, clusterName, listErr)
	}

	return toSortedNodeGroups(list.Items), nil
}

// toSortedNodeGroups converts raw MachineDeployment items to the
// simplified NodeGroup type and returns them sorted by Name. Extracted
// so tests can exercise the shape flattening without spinning up a
// dynamic client.
func toSortedNodeGroups(items []unstructured.Unstructured) []NodeGroup {
	groups := make([]NodeGroup, 0, len(items))

	for i := range items {
		groups = append(groups, toNodeGroup(&items[i]))
	}

	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Name < groups[j].Name
	})

	return groups
}

// toNodeGroup flattens one MachineDeployment into the display shape.
// Missing fields collapse to their zero values — a partially-
// reconciled MD that has no status.replicas yet reads as "0 ready"
// in the UI, which is exactly how kubectl presents it too.
func toNodeGroup(obj *unstructured.Unstructured) NodeGroup {
	desired, _, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	statusReplicas, _, _ := unstructured.NestedInt64(obj.Object, "status", "replicas")
	ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
	updated, _, _ := unstructured.NestedInt64(obj.Object, "status", "updatedReplicas")
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")

	return NodeGroup{
		Name:            obj.GetName(),
		DesiredReplicas: desired,
		StatusReplicas:  statusReplicas,
		ReadyReplicas:   ready,
		UpdatedReplicas: updated,
		Phase:           phase,
	}
}
