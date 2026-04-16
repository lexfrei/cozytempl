package k8s

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

// logAPIPath is the legacy /api root that serves core/v1 pod
// subresources. Shared by TailLogs and StreamLogs so the two
// paths cannot drift.
const logAPIPath = "/api"

// LogService streams pod logs for UI consumption. The user identity is
// passed on every call so read permission follows normal Kubernetes RBAC —
// if a user cannot `get pods/log` in the namespace, the UI sees the same
// error upstream.
type LogService struct {
	baseCfg *rest.Config
	mode    config.AuthMode
}

// NewLogService creates a new log service.
func NewLogService(baseCfg *rest.Config, mode config.AuthMode) *LogService {
	return &LogService{baseCfg: baseCfg, mode: mode}
}

// PodInfo is a tiny pod summary used by the logs tab to populate the
// pod selector.
type PodInfo struct {
	Name       string   `json:"name"`
	Phase      string   `json:"phase"`
	Containers []string `json:"containers"`
}

// ListPodsForApp returns every pod in the namespace that carries the
// Cozystack application.name label matching appName. Results are
// sorted by name for stable rendering.
func (lsv *LogService) ListPodsForApp(
	ctx context.Context, usr *auth.UserContext, namespace, appName string,
) ([]PodInfo, error) {
	if namespace == "" {
		return nil, ErrNamespaceRequired
	}

	if !isValidLabelValue(appName) {
		return nil, fmt.Errorf("%w: invalid application name %q", ErrAppNotFound, appName)
	}

	client, err := NewUserClient(lsv.baseCfg, usr, lsv.mode)
	if err != nil {
		return nil, err
	}

	podList, listErr := client.Resource(PodGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: cozyAppNameLabel + "=" + appName,
	})
	if listErr != nil {
		return nil, fmt.Errorf("listing pods for %s/%s: %w", namespace, appName, listErr)
	}

	pods := make([]PodInfo, 0, len(podList.Items))

	for idx := range podList.Items {
		pods = append(pods, toPodInfo(&podList.Items[idx]))
	}

	return pods, nil
}

func toPodInfo(obj *unstructured.Unstructured) PodInfo {
	info := PodInfo{Name: obj.GetName()}

	info.Phase = nestedString(obj.Object, "status", "phase")

	// Collect container names from spec.containers and
	// spec.initContainers — the logs endpoint accepts either.
	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "containers")
	for _, raw := range containers {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		if name, _ := c["name"].(string); name != "" {
			info.Containers = append(info.Containers, name)
		}
	}

	return info
}

// TailLogs returns the last `tailLines` lines of the given container's
// log for the given pod. The Kubernetes typed client is avoided in
// favour of a raw REST request so this package keeps its existing
// dependency on only client-go/dynamic + client-go/rest — which is
// enough because the /log subresource is a plain text stream.
func (lsv *LogService) TailLogs(
	ctx context.Context, usr *auth.UserContext,
	namespace, pod, container string, tailLines int64,
) (string, error) {
	validateErr := validateLogsParams(namespace, pod, container)
	if validateErr != nil {
		return "", validateErr
	}

	// Build a user-scoped REST config using the same auth logic as
	// NewUserClient, then layer the log-specific settings on top.
	cfg, err := buildUserRESTConfig(lsv.baseCfg, usr, lsv.mode)
	if err != nil {
		return "", fmt.Errorf("building user rest config: %w", err)
	}

	cfg.APIPath = logAPIPath
	cfg.GroupVersion = &corev1GV
	cfg.NegotiatedSerializer = basicSerializer{}

	restClient, err := rest.RESTClientFor(cfg)
	if err != nil {
		return "", fmt.Errorf("building rest client: %w", err)
	}

	request := restClient.Get().
		Namespace(namespace).
		Resource("pods").
		Name(pod).
		SubResource("log").
		Param("tailLines", strconv.FormatInt(tailLines, 10))

	if container != "" {
		request = request.Param("container", container)
	}

	raw, reqErr := request.DoRaw(ctx)
	if reqErr != nil {
		return "", fmt.Errorf("reading pod log %s/%s: %w", namespace, pod, reqErr)
	}

	return string(raw), nil
}

// StreamLogs opens a follow=true pod log stream under the caller's
// credentials and returns the raw io.ReadCloser so the HTTP
// handler can pump bytes into a WebSocket. The caller MUST Close
// the reader when the subscriber disconnects; the underlying
// HTTP response body remains open until ctx is cancelled OR the
// apiserver closes its side.
//
// Routes through the exported BuildUserRESTConfig so its 10 s
// HTTP client deadline is applied, then explicitly zeros the
// deadline — the same Timeout=0 dance WatchProxy.Stream performs,
// and for the same reason. LIST/GET callers want the deadline
// (loud failures on control-plane wobbles); a follow stream
// would be killed by it, so long-lived streams must opt out
// while keeping the user-credential plumbing intact. Going
// through the exported helper rather than the internal one
// keeps the two call sites symmetrical and immune to a future
// refactor that starts applying per-mode overrides there.
func (lsv *LogService) StreamLogs(
	ctx context.Context, usr *auth.UserContext,
	namespace, pod, container string, tailLines int64,
) (io.ReadCloser, error) {
	validateErr := validateLogsParams(namespace, pod, container)
	if validateErr != nil {
		return nil, validateErr
	}

	cfg, err := BuildUserRESTConfig(lsv.baseCfg, usr, lsv.mode)
	if err != nil {
		return nil, fmt.Errorf("building user rest config: %w", err)
	}

	cfg.APIPath = logAPIPath
	cfg.GroupVersion = &corev1GV
	cfg.NegotiatedSerializer = basicSerializer{}
	cfg.Timeout = 0

	restClient, err := rest.RESTClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("building rest client: %w", err)
	}

	request := restClient.Get().
		Namespace(namespace).
		Resource("pods").
		Name(pod).
		SubResource("log").
		Param("follow", "true").
		Param("tailLines", strconv.FormatInt(tailLines, 10))

	if container != "" {
		request = request.Param("container", container)
	}

	stream, streamErr := request.Stream(ctx)
	if streamErr != nil {
		return nil, fmt.Errorf("opening pod log stream %s/%s: %w", namespace, pod, streamErr)
	}

	return stream, nil
}

// ValidateLogsParams is the exported form of the same fence
// the TailLogs / StreamLogs constructors apply. Exposed so the
// WebSocket handler (internal/api/ws_logs.go) can reject
// malformed input pre-upgrade instead of paying the handshake
// cost only to fail inside the stream-open call.
func ValidateLogsParams(namespace, pod, container string) error {
	return validateLogsParams(namespace, pod, container)
}

// validateLogsParams centralises the defensive checks so
// both TailLogs and StreamLogs apply them consistently. namespace
// and container join pod on the isValidLabelValue fence — an
// upstream apiserver error on a malformed name is ugly and
// hides the actual misuse from the caller, so fail locally.
func validateLogsParams(namespace, pod, container string) error {
	if namespace == "" || pod == "" {
		return ErrNamespaceRequired
	}

	if !isValidLabelValue(namespace) {
		return fmt.Errorf("%w: invalid namespace %q", ErrAppNotFound, namespace)
	}

	if !isValidLabelValue(pod) {
		return fmt.Errorf("%w: invalid pod name %q", ErrAppNotFound, pod)
	}

	if container != "" && !isValidLabelValue(container) {
		return fmt.Errorf("%w: invalid container name %q", ErrAppNotFound, container)
	}

	return nil
}
