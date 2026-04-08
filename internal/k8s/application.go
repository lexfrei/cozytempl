package k8s

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

const (
	cozyAppGroup    = "apps.cozystack.io"
	secretsResource = "secrets"
)

// ErrAppNotFound is returned when an application is not found in a tenant.
// ErrAppNotFound is returned when an application cannot be found.
var ErrAppNotFound = errors.New("application not found")

// ApplicationService provides operations on Cozystack applications
// via the apps.cozystack.io/v1alpha1 CRDs.
type ApplicationService struct {
	baseCfg   *rest.Config
	schemaSvc *SchemaService
}

// NewApplicationService creates a new application service.
func NewApplicationService(baseCfg *rest.Config, schemaSvc *SchemaService) *ApplicationService {
	return &ApplicationService{baseCfg: baseCfg, schemaSvc: schemaSvc}
}

// List returns all applications in a tenant namespace by querying each known app CRD.
func (asv *ApplicationService) List(
	ctx context.Context, username string, groups []string, tenant string,
) ([]Application, error) {
	client, err := NewImpersonatingClient(asv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	// Also list HelmReleases with the cozystack label as a unified view
	hrList, err := client.Resource(HelmReleaseGVR()).Namespace(tenant).List(ctx, metav1.ListOptions{
		LabelSelector: "apps.cozystack.io/application.kind",
	})
	if err != nil {
		return nil, fmt.Errorf("listing applications in %s: %w", tenant, err)
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

	// First try to find it as a HelmRelease (the canonical view with status)
	hrList, err := client.Resource(HelmReleaseGVR()).Namespace(tenant).List(ctx, metav1.ListOptions{
		LabelSelector: "apps.cozystack.io/application.name=" + name,
	})
	if err != nil {
		return nil, fmt.Errorf("getting application %s/%s: %w", tenant, name, err)
	}

	if len(hrList.Items) == 0 {
		return nil, fmt.Errorf("%w: %s in %s", ErrAppNotFound, name, tenant)
	}

	app := helmReleaseToApplication(&hrList.Items[0], tenant)
	app.ConnectionInfo = asv.getConnectionInfo(ctx, client, tenant, name, app.Kind)

	return &app, nil
}

// Create creates a new application via its Cozystack CRD.
func (asv *ApplicationService) Create(
	ctx context.Context, username string, groups []string, tenant string, req CreateApplicationRequest,
) (*Application, error) {
	client, err := NewImpersonatingClient(asv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	gvr := appGVR(req.Kind)

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": cozyAppGroup + "/v1alpha1",
			"kind":       req.Kind,
			"metadata": map[string]any{
				"name":      req.Name,
				"namespace": tenant,
			},
			"spec": req.Spec,
		},
	}

	created, err := client.Resource(gvr).Namespace(tenant).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating %s %s/%s: %w", req.Kind, tenant, req.Name, err)
	}

	app := crdToApplication(created, tenant)

	return &app, nil
}

// Update updates an application's spec via its Cozystack CRD.
func (asv *ApplicationService) Update(
	ctx context.Context, username string, groups []string, tenant, name string, req UpdateApplicationRequest,
) (*Application, error) {
	client, err := NewImpersonatingClient(asv.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	// Need to find the kind first from HelmRelease labels
	kind, findErr := asv.findAppKind(ctx, client, tenant, name)
	if findErr != nil {
		return nil, findErr
	}

	gvr := appGVR(kind)

	obj, err := client.Resource(gvr).Namespace(tenant).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting %s for update: %w", kind, err)
	}

	err = unstructured.SetNestedField(obj.Object, req.Spec, "spec")
	if err != nil {
		return nil, fmt.Errorf("setting spec: %w", err)
	}

	updated, err := client.Resource(gvr).Namespace(tenant).Update(ctx, obj, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("updating %s %s/%s: %w", kind, tenant, name, err)
	}

	app := crdToApplication(updated, tenant)

	return &app, nil
}

// Delete removes an application via its Cozystack CRD.
func (asv *ApplicationService) Delete(
	ctx context.Context, username string, groups []string, tenant, name string,
) error {
	client, err := NewImpersonatingClient(asv.baseCfg, username, groups)
	if err != nil {
		return err
	}

	kind, findErr := asv.findAppKind(ctx, client, tenant, name)
	if findErr != nil {
		return findErr
	}

	gvr := appGVR(kind)

	err = client.Resource(gvr).Namespace(tenant).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("deleting %s %s/%s: %w", kind, tenant, name, err)
	}

	return nil
}

func (asv *ApplicationService) findAppKind(
	ctx context.Context, client dynamic.Interface, tenant, name string,
) (string, error) {
	hrList, err := client.Resource(HelmReleaseGVR()).Namespace(tenant).List(ctx, metav1.ListOptions{
		LabelSelector: "apps.cozystack.io/application.name=" + name,
	})
	if err != nil {
		return "", fmt.Errorf("finding application kind: %w", err)
	}

	if len(hrList.Items) == 0 {
		return "", fmt.Errorf("%w: %s in %s", ErrAppNotFound, name, tenant)
	}

	labels := hrList.Items[0].GetLabels()

	return labels["apps.cozystack.io/application.kind"], nil
}

func (asv *ApplicationService) getConnectionInfo(
	ctx context.Context, client dynamic.Interface, tenant, name, kind string,
) map[string]string {
	result := make(map[string]string)

	// Try templates from ApplicationDefinition
	appDef, err := asv.schemaSvc.Get(ctx, "dev-admin", []string{"system:masters"}, kind)
	if err == nil {
		for _, tmpl := range appDef.SecretTemplates {
			secretName := strings.ReplaceAll(tmpl, "{{ .name }}", name)
			readSecretInto(ctx, client, tenant, secretName, result)
		}
	}

	// Fallback: find secrets with HelmRelease prefix
	if len(result) == 0 {
		prefix := toLowerKind(kind) + "-" + name + "-"
		findSecretsByPrefix(ctx, client, tenant, prefix, result)
	}

	if len(result) == 0 {
		return nil
	}

	return result
}

func findSecretsByPrefix(
	ctx context.Context, client dynamic.Interface, tenant, prefix string, dest map[string]string,
) {
	secretGVR := NamespaceGVR()
	secretGVR.Resource = secretsResource

	secretList, err := client.Resource(secretGVR).Namespace(tenant).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	for idx := range secretList.Items {
		sec := &secretList.Items[idx]
		secName := sec.GetName()

		if !strings.HasPrefix(secName, prefix) {
			continue
		}

		// Skip helm release secrets and TLS/CA certs
		if strings.Contains(secName, "sh.helm.release") {
			continue
		}

		suffix := strings.TrimPrefix(secName, prefix)
		if suffix == "ca" || suffix == "server" || suffix == "replication" || suffix == "init-script" {
			continue
		}

		readSecretInto(ctx, client, tenant, secName, dest)
	}
}

func readSecretInto(ctx context.Context, client dynamic.Interface, tenant, secretName string, dest map[string]string) {
	secretGVR := NamespaceGVR()
	secretGVR.Resource = secretsResource

	obj, err := client.Resource(secretGVR).Namespace(tenant).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return
	}

	data, found, _ := unstructured.NestedMap(obj.Object, "data")
	if !found {
		return
	}

	for key, val := range data {
		str, ok := val.(string)
		if !ok {
			continue
		}

		decoded, decErr := base64Decode(str)
		if decErr == nil {
			dest[key] = decoded
		} else {
			dest[key] = str
		}
	}
}

func helmReleaseToApplication(obj *unstructured.Unstructured, tenant string) Application {
	labels := obj.GetLabels()
	kind := labels["apps.cozystack.io/application.kind"]
	name := labels["apps.cozystack.io/application.name"]

	status := extractStatus(obj)
	conditions := extractConditions(obj)
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec", "values")

	createdAt := obj.GetCreationTimestamp().Time
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	return Application{
		Name:       name,
		Kind:       kind,
		Tenant:     tenant,
		Status:     status,
		Conditions: conditions,
		Spec:       spec,
		CreatedAt:  createdAt,
	}
}

func crdToApplication(obj *unstructured.Unstructured, tenant string) Application {
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	status := extractStatus(obj)
	conditions := extractConditions(obj)

	createdAt := obj.GetCreationTimestamp().Time
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	return Application{
		Name:       obj.GetName(),
		Kind:       obj.GetKind(),
		Tenant:     tenant,
		Status:     status,
		Conditions: conditions,
		Spec:       spec,
		CreatedAt:  createdAt,
	}
}

// appGVR returns the GVR for a Cozystack application CRD given its Kind.
// Kind "Postgres" -> resource "postgreses", Kind "Tenant" -> "tenants", etc.
func appGVR(kind string) schema.GroupVersionResource {
	plural := strings.ToLower(kind)

	// Handle known irregular plurals from Cozystack
	knownPlurals := map[string]string{
		"postgres":            "postgreses",
		"redis":               "redises",
		"kubernetes":          "kuberneteses",
		"nats":                "natses",
		"seaweedfs":           "seaweedfses",
		"clickhouse":          "clickhouses",
		"mongodb":             "mongodbs",
		"mariadb":             "mariadbs",
		"kafka":               "kafkas",
		"rabbitmq":            "rabbitmqs",
		"etcd":                "etcds",
		"harbor":              "harbors",
		"qdrant":              "qdrants",
		"openbao":             "openbaos",
		"tenant":              "tenants",
		"bucket":              "buckets",
		"ingress":             "ingresses",
		"httpcache":           "httpcaches",
		"tcpbalancer":         "tcpbalancers",
		"monitoring":          "monitorings",
		"vminstance":          "vminstances",
		"vmdisk":              "vmdisks",
		"vpn":                 "vpns",
		"virtualprivatecloud": "virtualprivateclouds",
		"minecraftserver":     "minecraftservers",
		"minecraftplugin":     "minecraftplugins",
		"foundationdb":        "foundationdbs",
		"info":                "infos",
	}

	if known, ok := knownPlurals[plural]; ok {
		plural = known
	} else {
		plural += "s"
	}

	return schema.GroupVersionResource{
		Group:    cozyAppGroup,
		Version:  "v1alpha1",
		Resource: plural,
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

func base64Decode(str string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(str)
	if err != nil {
		return "", err //nolint:wrapcheck // internal helper
	}

	return string(decoded), nil
}
