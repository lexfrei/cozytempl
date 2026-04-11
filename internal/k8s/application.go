package k8s

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// ErrAppNotFound is returned when an application cannot be found.
var ErrAppNotFound = errors.New("application not found")

// ErrConflict is returned by Update when the stored resource has
// changed since the caller fetched its spec. The user should
// reload the edit form and retry. Mapped from k8s 409 Conflict
// responses, which the API server emits when the incoming
// resourceVersion no longer matches.
var ErrConflict = errors.New("resource was modified by another writer")

// conflictMessageFragment matches the stable English fragment k8s
// embeds in every 409 Conflict error message. Used as a fallback
// conflict detector when cozystack's admission webhook rewraps
// the underlying StatusError in a plain %v error, stripping the
// StatusReason metadata apierrors.IsConflict relies on.
const conflictMessageFragment = "the object has been modified"

// isConflictError returns true for both typed k8s 409 Conflict
// StatusErrors and for cozystack's webhook-wrapped variant where
// the type information has been lost but the canonical message
// fragment survives.
func isConflictError(err error) bool {
	if err == nil {
		return false
	}

	if apierrors.IsConflict(err) {
		return true
	}

	return strings.Contains(err.Error(), conflictMessageFragment)
}

// SpecSnapshot is an editable point-in-time read of a CRD's spec.
// ResourceVersion pins the read so a subsequent Update can use it
// for optimistic concurrency control — the API server will reject
// a Put whose resourceVersion no longer matches with 409 Conflict.
type SpecSnapshot struct {
	Spec            map[string]any
	Kind            string
	ResourceVersion string
}

// ApplicationService provides operations on Cozystack applications
// via the apps.cozystack.io/v1alpha1 CRDs.
type ApplicationService struct {
	baseCfg   *rest.Config
	schemaSvc *SchemaService
	mode      config.AuthMode
}

// NewApplicationService creates a new application service.
func NewApplicationService(baseCfg *rest.Config, schemaSvc *SchemaService, mode config.AuthMode) *ApplicationService {
	return &ApplicationService{baseCfg: baseCfg, schemaSvc: schemaSvc, mode: mode}
}

// AppListLimit caps how many applications a single List call
// returns. Picked high enough that a normal tenant (1-50 apps)
// never hits it and low enough that a pathological 10 000-app
// tenant can't OOM cozytempl or blow out the HTML response.
// A truncated result surfaces to the UI so the user can see
// that not every app is on screen.
const AppListLimit int64 = 500

// ApplicationList is the pagination-aware return shape of
// ApplicationService.List. Items holds the current page (up to
// AppListLimit entries) and Truncated is set when the k8s API
// returned a continue token — i.e. there is at least one more
// page we chose not to fetch. Callers render the truncation
// warning in the UI so the user knows the filter/sort they're
// looking at is over a subset.
type ApplicationList struct {
	Items     []Application
	Truncated bool
}

// List returns applications in a tenant namespace, capped at
// AppListLimit. The cap is deliberate: the UI does client-side
// filter and sort over the returned slice, so paginating the
// full set would produce confusing partial-data behaviour
// ("why does this filter match 3 of 10 000 apps?"). Better to
// return a bounded window and surface the truncation warning.
func (asv *ApplicationService) List(
	ctx context.Context, usr *auth.UserContext, tenant string,
) (ApplicationList, error) {
	client, err := NewUserClient(asv.baseCfg, usr, asv.mode)
	if err != nil {
		return ApplicationList{}, err
	}

	// HelmReleases with the cozystack label are the unified view
	// over every app kind. One List call per tenant namespace is
	// still much cheaper than fanning out per CRD.
	hrList, err := client.Resource(HelmReleaseGVR()).Namespace(tenant).List(ctx, metav1.ListOptions{
		LabelSelector: cozyAppKindLabel,
		Limit:         AppListLimit,
	})
	if err != nil {
		return ApplicationList{}, fmt.Errorf("listing applications in %s: %w", tenant, err)
	}

	apps := make([]Application, 0, len(hrList.Items))

	for idx := range hrList.Items {
		app := helmReleaseToApplication(&hrList.Items[idx], tenant)
		apps = append(apps, app)
	}

	return ApplicationList{
		Items:     apps,
		Truncated: hrList.GetContinue() != "",
	}, nil
}

// Get returns a single application with full details.
func (asv *ApplicationService) Get(
	ctx context.Context, usr *auth.UserContext, tenant, name string,
) (*Application, error) {
	if !isValidLabelValue(name) {
		return nil, fmt.Errorf("%w: invalid application name %q", ErrAppNotFound, name)
	}

	client, err := NewUserClient(asv.baseCfg, usr, asv.mode)
	if err != nil {
		return nil, err
	}

	// First try to find it as a HelmRelease (the canonical view with status)
	hrList, err := client.Resource(HelmReleaseGVR()).Namespace(tenant).List(ctx, metav1.ListOptions{
		LabelSelector: cozyAppNameLabel + "=" + name,
	})
	if err != nil {
		return nil, fmt.Errorf("getting application %s/%s: %w", tenant, name, err)
	}

	if len(hrList.Items) == 0 {
		return nil, fmt.Errorf("%w: %s in %s", ErrAppNotFound, name, tenant)
	}

	app := helmReleaseToApplication(&hrList.Items[0], tenant)
	app.ConnectionInfo = asv.getConnectionInfo(ctx, client, usr, tenant, name, app.Kind)

	return &app, nil
}

// Create creates a new application via its Cozystack CRD.
func (asv *ApplicationService) Create(
	ctx context.Context, usr *auth.UserContext, tenant string, req CreateApplicationRequest,
) (*Application, error) {
	client, err := NewUserClient(asv.baseCfg, usr, asv.mode)
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

// GetSpecSnapshot returns the raw spec map, resolved Kind, and
// resourceVersion of an existing application CRD. The edit modal
// uses the spec to pre-populate form fields, the kind to find the
// right schema, and the resourceVersion to pin the subsequent
// Update for optimistic concurrency — if another user writes to
// the same object between this call and the Update, the API
// server returns 409 Conflict.
func (asv *ApplicationService) GetSpecSnapshot(
	ctx context.Context, usr *auth.UserContext, tenant, name string,
) (*SpecSnapshot, error) {
	if !isValidLabelValue(name) {
		return nil, fmt.Errorf("%w: invalid application name %q", ErrAppNotFound, name)
	}

	client, err := NewUserClient(asv.baseCfg, usr, asv.mode)
	if err != nil {
		return nil, err
	}

	kind, findErr := asv.findAppKind(ctx, client, tenant, name)
	if findErr != nil {
		return nil, findErr
	}

	gvr := appGVR(kind)

	obj, getErr := client.Resource(gvr).Namespace(tenant).Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		return nil, fmt.Errorf("getting %s %s/%s: %w", kind, tenant, name, getErr)
	}

	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")

	return &SpecSnapshot{
		Spec:            spec,
		Kind:            kind,
		ResourceVersion: obj.GetResourceVersion(),
	}, nil
}

// Update updates an application's spec via its Cozystack CRD. The
// incoming spec is deep-merged into the existing spec, so fields the
// UI does not render (arrays, objects deeper than maxNestedDepth,
// e.g. postgresql.parameters.max_connections) survive every edit.
// A plain replacement would be a silent data-loss bug for any app
// whose schema has nested objects beyond what the form exposes.
//
// If req.ResourceVersion is non-empty, Update pins it on the
// outgoing object so the API server rejects the Put with 409
// Conflict when another writer has modified the resource in
// between — preventing silent clobbers in the multi-user case.
// The 409 is surfaced as ErrConflict so callers can give the
// user a specific "reload and retry" message.
func (asv *ApplicationService) Update(
	ctx context.Context, usr *auth.UserContext, tenant, name string, req UpdateApplicationRequest,
) (*Application, error) {
	client, err := NewUserClient(asv.baseCfg, usr, asv.mode)
	if err != nil {
		return nil, err
	}

	kind, findErr := asv.findAppKind(ctx, client, tenant, name)
	if findErr != nil {
		return nil, findErr
	}

	gvr := appGVR(kind)

	obj, err := client.Resource(gvr).Namespace(tenant).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting %s for update: %w", kind, err)
	}

	existing, _, _ := unstructured.NestedMap(obj.Object, "spec")
	merged := deepMergeSpec(existing, req.Spec)

	setErr := unstructured.SetNestedField(obj.Object, merged, "spec")
	if setErr != nil {
		return nil, fmt.Errorf("setting spec: %w", setErr)
	}

	if req.ResourceVersion != "" {
		obj.SetResourceVersion(req.ResourceVersion)
	}

	updated, err := client.Resource(gvr).Namespace(tenant).Update(ctx, obj, metav1.UpdateOptions{})
	if err != nil {
		if isConflictError(err) {
			return nil, ErrConflict
		}

		return nil, fmt.Errorf("updating %s %s/%s: %w", kind, tenant, name, err)
	}

	app := crdToApplication(updated, tenant)

	return &app, nil
}

// Delete removes an application via its Cozystack CRD.
func (asv *ApplicationService) Delete(
	ctx context.Context, usr *auth.UserContext, tenant, name string,
) error {
	client, err := NewUserClient(asv.baseCfg, usr, asv.mode)
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
	if !isValidLabelValue(name) {
		return "", fmt.Errorf("%w: invalid application name %q", ErrAppNotFound, name)
	}

	hrList, err := client.Resource(HelmReleaseGVR()).Namespace(tenant).List(ctx, metav1.ListOptions{
		LabelSelector: cozyAppNameLabel + "=" + name,
	})
	if err != nil {
		return "", fmt.Errorf("finding application kind: %w", err)
	}

	if len(hrList.Items) == 0 {
		return "", fmt.Errorf("%w: %s in %s", ErrAppNotFound, name, tenant)
	}

	labels := hrList.Items[0].GetLabels()

	return labels[cozyAppKindLabel], nil
}

func (asv *ApplicationService) getConnectionInfo(
	ctx context.Context, client dynamic.Interface, usr *auth.UserContext, tenant, name, kind string,
) map[string]string {
	result := make(map[string]string)

	// Fetch the ApplicationDefinition as the requesting user, not as the
	// baked-in dev-admin — otherwise anyone who could hit GetConnectionInfo
	// could read ApplicationDefinition metadata outside their RBAC scope.
	appDef, err := asv.schemaSvc.Get(ctx, usr, kind)
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
	if labels == nil {
		// HelmReleases missing Cozystack labels are not apps we care about;
		// return a zero-value Application so callers can filter (see the
		// empty-name guard in sse.buildMessage).
		return Application{Tenant: tenant}
	}

	kind := labels[cozyAppKindLabel]
	name := labels[cozyAppNameLabel]

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
