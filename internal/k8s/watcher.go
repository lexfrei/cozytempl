package k8s

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

const eventChannelBuffer = 100

// WatchEvent is sent to SSE subscribers.
type WatchEvent struct {
	Type string
	Data string
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
func (wat *Watcher) Start(ctx context.Context) error {
	client, err := dynamic.NewForConfig(wat.baseCfg)
	if err != nil {
		return err //nolint:wrapcheck // startup error, caller handles
	}

	watcher, err := client.Resource(HelmReleaseGVR()).Namespace("").Watch(ctx, metav1.ListOptions{
		LabelSelector: "apps.cozystack.io/application.kind",
	})
	if err != nil {
		return err //nolint:wrapcheck // startup error, caller handles
	}

	go wat.processEvents(ctx, watcher)

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

			wat.handleEvent(obj)
		}
	}
}

func (wat *Watcher) handleEvent(obj *unstructured.Unstructured) {
	namespace := obj.GetNamespace()
	name := obj.GetName()
	status := extractStatus(obj)

	eventData := SSEEvent{
		Type: "status:" + name,
		Name: name,
		Data: string(status),
	}

	jsonData, err := json.Marshal(eventData)
	if err != nil {
		wat.log.Error("marshaling SSE event", "error", err)

		return
	}

	watchEvt := WatchEvent{
		Type: "status:" + name,
		Data: string(jsonData),
	}

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
