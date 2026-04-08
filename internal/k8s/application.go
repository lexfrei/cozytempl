package k8s

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// ApplicationService provides operations on Cozystack applications (HelmReleases).
type ApplicationService struct {
	baseCfg *rest.Config
}

// NewApplicationService creates a new application service.
func NewApplicationService(baseCfg *rest.Config) *ApplicationService {
	return &ApplicationService{baseCfg: baseCfg}
}

// List returns all applications in a tenant namespace.
func (asv *ApplicationService) List(
	ctx context.Context, username string, groups []string, tenant string,
) ([]Application, error) {
	client, err := NewImpersonatingClient(asv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	hrList, err := client.Resource(HelmReleaseGVR()).Namespace(tenant).List(ctx, metav1.ListOptions{
		LabelSelector: "apps.cozystack.io/application.kind",
	})
	if err != nil {
		return nil, fmt.Errorf("listing HelmReleases in %s: %w", tenant, err)
	}

	apps := make([]Application, 0, len(hrList.Items))

	for idx := range hrList.Items {
		app := helmReleaseToApplication(&hrList.Items[idx], tenant)
		apps = append(apps, app)
	}

	return apps, nil
}

// Get returns a single application with full details.
func (asv *ApplicationService) Get(
	ctx context.Context, username string, groups []string, tenant, name string,
) (*Application, error) {
	client, err := NewImpersonatingClient(asv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	obj, err := client.Resource(HelmReleaseGVR()).Namespace(tenant).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting HelmRelease %s/%s: %w", tenant, name, err)
	}

	app := helmReleaseToApplication(obj, tenant)

	// Try to get connection info from related secret
	app.ConnectionInfo = asv.getConnectionInfo(ctx, client, tenant, name)

	return &app, nil
}

// Create creates a new application HelmRelease.
func (asv *ApplicationService) Create(
	ctx context.Context, username string, groups []string, tenant string, req CreateApplicationRequest,
) (*Application, error) {
	client, err := NewImpersonatingClient(asv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	chartRefName := fmt.Sprintf("cozystack-%s-application-default-%s",
		toLowerKind(req.Kind), toLowerKind(req.Kind))

	helmRelease := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "helm.toolkit.fluxcd.io/v2",
			"kind":       "HelmRelease",
			"metadata": map[string]any{
				"name":      req.Name,
				"namespace": tenant,
				"labels": map[string]any{
					"apps.cozystack.io/application.kind": req.Kind,
					"apps.cozystack.io/application.name": req.Name,
				},
			},
			"spec": map[string]any{
				"chartRef": map[string]any{
					"kind":      "ExternalArtifact",
					"name":      chartRefName,
					"namespace": "cozy-system",
				},
				"interval": "5m",
				"timeout":  "10m",
				"values":   req.Values,
			},
		},
	}

	created, err := client.Resource(HelmReleaseGVR()).Namespace(tenant).Create(ctx, helmRelease, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating HelmRelease: %w", err)
	}

	app := helmReleaseToApplication(created, tenant)

	return &app, nil
}

// Update patches an application's values.
func (asv *ApplicationService) Update(
	ctx context.Context, username string, groups []string, tenant, name string, req UpdateApplicationRequest,
) (*Application, error) {
	client, err := NewImpersonatingClient(asv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	obj, err := client.Resource(HelmReleaseGVR()).Namespace(tenant).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting HelmRelease for update: %w", err)
	}

	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	if spec == nil {
		spec = map[string]any{}
	}

	spec["values"] = req.Values

	err = unstructured.SetNestedField(obj.Object, spec, "spec")
	if err != nil {
		return nil, fmt.Errorf("setting spec: %w", err)
	}

	updated, err := client.Resource(HelmReleaseGVR()).Namespace(tenant).Update(ctx, obj, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("updating HelmRelease: %w", err)
	}

	app := helmReleaseToApplication(updated, tenant)

	return &app, nil
}

// Delete removes an application HelmRelease.
func (asv *ApplicationService) Delete(
	ctx context.Context, username string, groups []string, tenant, name string,
) error {
	client, err := NewImpersonatingClient(asv.baseCfg, username, groups)
	if err != nil {
		return err
	}

	err = client.Resource(HelmReleaseGVR()).Namespace(tenant).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("deleting HelmRelease %s/%s: %w", tenant, name, err)
	}

	return nil
}

func (asv *ApplicationService) getConnectionInfo(
	ctx context.Context, client dynamic.Interface, tenant, name string,
) map[string]string {
	secretGVR := NamespaceGVR()
	secretGVR.Resource = "secrets"

	obj, err := client.Resource(secretGVR).Namespace(tenant).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil
	}

	data, found, _ := unstructured.NestedStringMap(obj.Object, "data")
	if !found {
		return nil
	}

	return data
}

func helmReleaseToApplication(obj *unstructured.Unstructured, tenant string) Application {
	labels := obj.GetLabels()
	kind := labels["apps.cozystack.io/application.kind"]

	status := extractStatus(obj)
	conditions := extractConditions(obj)
	values, _, _ := unstructured.NestedMap(obj.Object, "spec", "values")

	createdAt := obj.GetCreationTimestamp().Time
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	return Application{
		Name:       obj.GetName(),
		Kind:       kind,
		Tenant:     tenant,
		Status:     status,
		Conditions: conditions,
		Values:     values,
		CreatedAt:  createdAt,
	}
}

func extractStatus(obj *unstructured.Unstructured) AppStatus {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return AppStatusUnknown
	}

	for _, cond := range conditions {
		condMap, ok := cond.(map[string]any)
		if !ok {
			continue
		}

		condType, _ := condMap["type"].(string)
		condStatus, _ := condMap["status"].(string)

		if condType == "Ready" {
			if condStatus == "True" {
				return AppStatusReady
			}

			reason, _ := condMap["reason"].(string)
			if reason == "Progressing" || reason == "ArtifactFailed" {
				return AppStatusReconciling
			}

			return AppStatusFailed
		}
	}

	return AppStatusUnknown
}

func extractConditions(obj *unstructured.Unstructured) []Condition {
	rawConditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return nil
	}

	result := make([]Condition, 0, len(rawConditions))

	for _, raw := range rawConditions {
		condMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		cond := Condition{
			Type:    stringFromMap(condMap, "type"),
			Status:  stringFromMap(condMap, "status"),
			Reason:  stringFromMap(condMap, "reason"),
			Message: stringFromMap(condMap, "message"),
		}

		if ts, ok := condMap["lastTransitionTime"].(string); ok {
			parsed, parseErr := time.Parse(time.RFC3339, ts)
			if parseErr == nil {
				cond.LastTransitionTime = parsed
			}
		}

		result = append(result, cond)
	}

	return result
}

func stringFromMap(m map[string]any, key string) string {
	val, _ := m[key].(string)

	return val
}

func toLowerKind(kind string) string {
	return strings.ToLower(kind)
}
