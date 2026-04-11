package k8s

import (
	"context"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

const (
	eventChannelBuffer  = 100
	watchReconnectDelay = 2 * time.Second
	watchLabelSelector  = cozyAppKindLabel
)

// WatchEventType matches a subset of watch.EventType useful for SSE fan-out.
type WatchEventType string

// WatchEventType values.
const (
	WatchEventAdded    WatchEventType = "added"
	WatchEventModified WatchEventType = "modified"
	WatchEventDeleted  WatchEventType = "deleted"
)

// WatchEvent carries an application change to SSE subscribers.
type WatchEvent struct {
	Type WatchEventType
	App  Application
}

// Watcher watches HelmRelease changes and fans out to SSE subscribers.
type Watcher struct {
	baseCfg     *rest.Config
	log         *slog.Logger
	subscribers map[string]map[chan WatchEvent]struct{}
	mu          sync.RWMutex
}

// NewWatcher creates a new watcher.
func NewWatcher(baseCfg *rest.Config, log *slog.Logger) *Watcher {
	return &Watcher{
		baseCfg:     baseCfg,
		log:         log,
		subscribers: make(map[string]map[chan WatchEvent]struct{}),
	}
}

// Start begins watching HelmReleases across all tenant namespaces.
// The watch loop automatically reconnects on channel close or errors.
func (wat *Watcher) Start(ctx context.Context) error {
	client, err := dynamic.NewForConfig(wat.baseCfg)
	if err != nil {
		return err //nolint:wrapcheck // startup error, caller handles
	}

	go wat.watchLoop(ctx, client)

	return nil
}

// Subscribe creates a new event channel for a tenant namespace.
func (wat *Watcher) Subscribe(tenant, _ string) chan WatchEvent {
	eventChan := make(chan WatchEvent, eventChannelBuffer)

	wat.mu.Lock()
	defer wat.mu.Unlock()

	if wat.subscribers[tenant] == nil {
		wat.subscribers[tenant] = make(map[chan WatchEvent]struct{})
	}

	wat.subscribers[tenant][eventChan] = struct{}{}

	return eventChan
}

// Unsubscribe removes a subscriber channel.
func (wat *Watcher) Unsubscribe(eventChan chan WatchEvent) {
	wat.mu.Lock()
	defer wat.mu.Unlock()

	for tenant, subs := range wat.subscribers {
		if _, exists := subs[eventChan]; exists {
			delete(subs, eventChan)
			close(eventChan)

			if len(subs) == 0 {
				delete(wat.subscribers, tenant)
			}

			return
		}
	}
}

func (wat *Watcher) watchLoop(ctx context.Context, client dynamic.Interface) {
	for {
		if ctx.Err() != nil {
			return
		}

		watcher, err := client.Resource(HelmReleaseGVR()).Namespace("").Watch(ctx, metav1.ListOptions{
			LabelSelector: watchLabelSelector,
		})
		if err != nil {
			wat.log.Warn("failed to open watch, retrying", "error", err)

			if !sleepCtx(ctx, watchReconnectDelay) {
				return
			}

			continue
		}

		wat.log.Info("watch opened", "resource", "helmreleases")
		wat.processEvents(ctx, watcher)
		wat.log.Info("watch closed, reconnecting")

		if !sleepCtx(ctx, watchReconnectDelay) {
			return
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (wat *Watcher) processEvents(ctx context.Context, watcher watch.Interface) {
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				wat.log.Warn("watch channel closed, reconnecting...")

				return
			}

			obj, isUnstructured := event.Object.(*unstructured.Unstructured)
			if !isUnstructured {
				continue
			}

			wat.handleEvent(event.Type, obj)
		}
	}
}

func (wat *Watcher) handleEvent(kind watch.EventType, obj *unstructured.Unstructured) {
	eventType, ok := mapEventType(kind)
	if !ok {
		return
	}

	namespace := obj.GetNamespace()
	app := helmReleaseToApplication(obj, namespace)

	watchEvt := WatchEvent{Type: eventType, App: app}

	wat.mu.RLock()
	subs, exists := wat.subscribers[namespace]
	wat.mu.RUnlock()

	if !exists {
		return
	}

	for sub := range subs {
		select {
		case sub <- watchEvt:
		default:
			wat.log.Debug("dropping event for slow subscriber", "namespace", namespace)
		}
	}
}

func mapEventType(kind watch.EventType) (WatchEventType, bool) {
	switch kind {
	case watch.Added:
		return WatchEventAdded, true
	case watch.Modified:
		return WatchEventModified, true
	case watch.Deleted:
		return WatchEventDeleted, true
	case watch.Bookmark, watch.Error:
		return "", false
	default:
		return "", false
	}
}
