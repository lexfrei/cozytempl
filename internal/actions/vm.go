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
		Run:           invokeVMSubresource("start"),
	})

	Register("VMInstance", Action{
		ID:            "stop",
		LabelKey:      "page.appDetail.action.vmStop",
		AuditCategory: "vm.stop",
		Run:           invokeVMSubresource("stop"),
	})

	Register("VMInstance", Action{
		ID:            "restart",
		LabelKey:      "page.appDetail.action.vmRestart",
		AuditCategory: "vm.restart",
		Run:           invokeVMSubresource("restart"),
	})
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
