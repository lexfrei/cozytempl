package k8s

import (
	"context"
	"fmt"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// WatchProxy opens user-credentialed watch streams against the
// Kubernetes API on behalf of the browser subscriber. Unlike the
// shared Watcher type in this package — which runs as a privileged
// service account and fans out a single watch to every SSE client —
// WatchProxy builds one watch per (user, resource, namespace) tuple
// so every subscription inherits the caller's RBAC. That lets the
// UI stream live updates for any resource the user has list/watch
// rights on, without granting the watcher SA blanket permissions.
//
// The trade-off versus the shared watcher is one apiserver watch per
// subscription. Acceptable for the current usage pattern — an
// operator sitting on the tenant detail page watching one namespace
// — but worth revisiting once we're running dozens of concurrent
// subscribers per user or fanning out to thousands of tenants.
type WatchProxy struct{}

// NewWatchProxy returns a stateless proxy. Kept as a constructor
// rather than a bare struct literal so future additions (metrics,
// config knobs, a cached clientset pool) don't break call sites.
func NewWatchProxy() *WatchProxy {
	return &WatchProxy{}
}

// Authorize runs a SelfSubjectAccessReview with verb=watch against
// the target resource + namespace using the caller's credentials.
// Returns (true, nil) when the apiserver says allowed, (false, nil)
// on an explicit deny, (false, err) on a transport / probe failure.
// Must be called before Stream; a watch that 403s mid-stream
// surfaces to the user as a silently-empty SSE stream, whereas an
// upfront SSAR gives the handler a clean 403 to turn into a toast.
func (wp *WatchProxy) Authorize(
	ctx context.Context, userCfg *rest.Config, gvr schema.GroupVersionResource, namespace string,
) (bool, error) {
	client, err := kubernetes.NewForConfig(userCfg)
	if err != nil {
		return false, fmt.Errorf("building clientset for watch SSAR: %w", err)
	}

	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "watch",
				Group:     gvr.Group,
				Resource:  gvr.Resource,
				Version:   gvr.Version,
			},
		},
	}

	result, err := client.AuthorizationV1().
		SelfSubjectAccessReviews().
		Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("watch SSAR for %s/%s in %s: %w",
			gvr.Group, gvr.Resource, namespace, err)
	}

	return result.Status.Allowed, nil
}

// Stream opens a watch against the apiserver using userCfg and
// returns the raw watch.Interface so the caller can range over
// ResultChan(). ctx cancellation propagates to the underlying
// HTTP request — closing the watch cleanly when the subscriber
// disconnects.
//
// The returned Interface MUST have its Stop() called by the caller
// when the subscription ends; leaking it leaves an apiserver watch
// open until the TCP connection dies.
//
// The watch starts at the current apiserver state (ResourceVersion
// empty). This does create a narrow race: an event that landed
// between the preceding LIST and this Watch can appear twice in
// the caller's UI (once via the paginated LIST, once via the live
// stream). In practice the client-side reducer upserts by row id,
// so a duplicate modifies the same row in place — cosmetically
// the same flash twice, functionally harmless. If a future
// consumer needs strictly-once delivery, thread the LIST's
// metadata.resourceVersion in here.
func (wp *WatchProxy) Stream(
	ctx context.Context, userCfg *rest.Config, gvr schema.GroupVersionResource, namespace string,
) (watch.Interface, error) {
	client, err := dynamic.NewForConfig(userCfg)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client for watch: %w", err)
	}

	opts := metav1.ListOptions{Watch: true}

	w, err := client.Resource(gvr).Namespace(namespace).Watch(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("opening watch for %s/%s in %s: %w",
			gvr.Group, gvr.Resource, namespace, err)
	}

	return w, nil
}
