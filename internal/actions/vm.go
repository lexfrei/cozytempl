package actions

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// vmSubresourcePath is the shared prefix for every KubeVirt VM
// subresource verb. subresources.kubevirt.io is served by a
// dedicated virt-api pod that takes PUT requests (not POST) with
// an empty JSON body.
const vmSubresourcePath = "/apis/subresources.kubevirt.io/v1"

// vmInstanceReleasePrefix is the Cozystack VMInstance chart's
// release prefix. A Cozystack application named 'myvm' renders a
// HelmRelease 'vm-instance-myvm' and a KubeVirt VirtualMachine with
// the same 'vm-instance-myvm' name. Hard-coded here because it is
// the single contract the Cozystack ApplicationDefinition publishes;
// every new VMInstance Kind this library adds targets the same
// prefix by construction. If Cozystack ever ships a second chart
// under the same Kind with a different prefix, this constant
// becomes a switch keyed on ApplicationDefinition metadata.
const vmInstanceReleasePrefix = "vm-instance-"

// CozystackTenantAdminAggregationLabel is the ClusterRole
// aggregation label that folds a custom ClusterRole into
// cozy:tenant:admin on Cozystack clusters. Upstream Kubernetes uses
// rbac.authorization.k8s.io/aggregate-to-admin, which points at the
// stock namespace-admin role and is NOT what Cozystack tenant
// admins pick up. Cross-checked against cozystack upstream
// packages/system/cozystack-basics/templates/clusterroles.yaml.
// Referenced from the README RBAC example; the readme_test gate
// ensures the doc's YAML stays in sync with this constant.
const CozystackTenantAdminAggregationLabel = "rbac.cozystack.io/aggregate-to-tenant-admin"

// vmInstanceTargetName derives the KubeVirt VM resource name from
// the Cozystack application name. Exported via a function (not a
// direct string concat at registration time) so the handler and
// tests call one canonical helper rather than re-deriving the
// prefix ad-hoc.
func vmInstanceTargetName(appName string) string {
	return vmInstanceReleasePrefix + appName
}

// init registers the three KubeVirt VM actions for Cozystack's
// VMInstance Kind. The Cozystack VMInstance CR and the KubeVirt
// VirtualMachine it renders share a name (the Helm release name
// is used verbatim for the underlying VM), so the action handler
// can pass the Cozystack app name straight through to the KubeVirt
// subresource without extra lookup.
//
//nolint:gochecknoinits // registry wiring is the whole point of init
func init() {
	Register("VMInstance", Action{
		ID:            "start",
		LabelKey:      "page.appDetail.action.vmStart",
		AuditCategory: "vm.start",
		Destructive:   false,
		Capability:    vmSubresourceCapability("start"),
		TargetName:    vmInstanceTargetName,
		Run:           invokeVMSubresource("start"),
	})

	Register("VMInstance", Action{
		ID:            "stop",
		LabelKey:      "page.appDetail.action.vmStop",
		AuditCategory: "vm.stop",
		Destructive:   true,
		Capability:    vmSubresourceCapability("stop"),
		TargetName:    vmInstanceTargetName,
		Run:           invokeVMSubresource("stop"),
	})

	Register("VMInstance", Action{
		ID:            "restart",
		LabelKey:      "page.appDetail.action.vmRestart",
		AuditCategory: "vm.restart",
		Destructive:   true,
		Capability:    vmSubresourceCapability("restart"),
		TargetName:    vmInstanceTargetName,
		Run:           invokeVMSubresource("restart"),
	})
}

// vmSubresourceCapability builds the SSAR tuple for a KubeVirt VM
// subresource verb. KubeVirt's subresource apiserver serves
// /apis/subresources.kubevirt.io/v1/.../virtualmachines/{name}/{verb}
// with PUT, which Kubernetes RBAC checks as verb=update on resource
// virtualmachines in the subresources.kubevirt.io group with the
// specific subresource. Cozystack's stock tenant roles do NOT grant
// this today — an operator who wants the buttons visible needs to
// layer a custom Role granting update on
// subresources.kubevirt.io/virtualmachines/{start,stop,restart}
// onto the tenant-admin aggregation. The capability probe in the
// detail-page handler uses these exact tuples to hide the buttons
// from users who lack the grant.
func vmSubresourceCapability(verb string) Capability {
	return Capability{
		Group:       "subresources.kubevirt.io",
		Resource:    "virtualmachines",
		Subresource: verb,
		Verb:        "update",
	}
}

// invokeVMSubresource returns a Run closure that PUTs an empty body
// to the named KubeVirt subresource on the VM with the given name in
// the caller's tenant namespace. Errors from kubernetes.NewForConfig
// or from the apiserver reach the caller verbatim; the HTTP handler
// one layer up translates them into a toast / 4xx for the user.
func invokeVMSubresource(verb string) func(context.Context, *rest.Config, string, string) error {
	return func(ctx context.Context, userCfg *rest.Config, namespace, name string) error {
		client, err := kubernetes.NewForConfig(userCfg)
		if err != nil {
			return fmt.Errorf("building clientset: %w", err)
		}

		err = client.CoreV1().RESTClient().
			Put().
			AbsPath(vmSubresourcePath, "namespaces", namespace, "virtualmachines", name, verb).
			Body([]byte("{}")).
			Do(ctx).
			Error()
		if err != nil {
			return fmt.Errorf("kubevirt %s %s/%s: %w", verb, namespace, name, err)
		}

		return nil
	}
}
