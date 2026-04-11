package k8s

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
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

	// replayBufferSize is the number of recent events retained per
	// tenant namespace so a reconnecting SSE client can catch up on
	// what it missed. Picked large enough that a 5-second network
	// blip on a busy tenant (say, a rolling restart fanning out 30
	// modified events) is fully covered, and small enough that a
	// 500-tenant cluster's replay buffer memory stays bounded
	// (~50 KB per tenant worst case).
	replayBufferSize = 128
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
// EventID is a monotonic global sequence number assigned by the
// watcher when the event is published, used for SSE Last-Event-ID
// reconnection replay.
type WatchEvent struct {
	Type    WatchEventType
	App     Application
	EventID int64
}

// replayBuffer is a per-tenant ring of the most recent events so a
// reconnecting subscriber can fetch what it missed during the gap
// window between disconnect and reconnect. It is a plain slice used
// as a fixed-capacity ring to avoid an external dependency.
type replayBuffer struct {
	events []WatchEvent
	head   int
	full   bool
}

// add appends an event to the ring, overwriting the oldest slot
// once the ring is full. Callers must hold the Watcher mutex.
// evt is taken by pointer because WatchEvent is ~170 bytes and
// every HelmRelease change would otherwise copy it twice (once
// into the ring, once out on read).
func (buf *replayBuffer) add(evt *WatchEvent) {
	if buf.events == nil {
		buf.events = make([]WatchEvent, replayBufferSize)
	}

	buf.events[buf.head] = *evt
	buf.head = (buf.head + 1) % replayBufferSize

	if buf.head == 0 {
		buf.full = true
	}
}

// since returns every event in the ring with EventID strictly
// greater than sinceID, in the order they were added. The result
// is a fresh slice so callers can hold it without a lock.
func (buf *replayBuffer) since(sinceID int64) []WatchEvent {
	if buf.events == nil {
		return nil
	}

	// Walk the ring in chronological order. Start from the oldest
	// slot: head when full, 0 when not yet wrapped.
	start := 0
	count := buf.head

	if buf.full {
		start = buf.head
		count = replayBufferSize
	}

	var out []WatchEvent

	for i := range count {
		idx := (start + i) % replayBufferSize
		if buf.events[idx].EventID > sinceID {
			out = append(out, buf.events[idx])
		}
	}

	return out
}

// Watcher watches HelmRelease changes and fans out to SSE subscribers.
type Watcher struct {
	baseCfg     *rest.Config
	log         *slog.Logger
	subscribers map[string]map[chan WatchEvent]struct{}
	// buffers holds per-tenant ring buffers for SSE replay on
	// reconnection. Keyed by the same tenant namespace string as
	// subscribers. Under the same mu.
	buffers map[string]*replayBuffer
	// nextEventID is the monotonic sequence used to stamp each
	// event as it's published. Atomic so the buffer read path
	// doesn't need the main mutex just to generate an ID.
	nextEventID atomic.Int64
	mu          sync.RWMutex
}

// NewWatcher creates a new watcher.
func NewWatcher(baseCfg *rest.Config, log *slog.Logger) *Watcher {
	return &Watcher{
		baseCfg:     baseCfg,
		log:         log,
		subscribers: make(map[string]map[chan WatchEvent]struct{}),
		buffers:     make(map[string]*replayBuffer),
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

// Subscribe creates a new event channel for a tenant namespace. The caller
// is responsible for verifying the user has permission to watch events in
// that namespace BEFORE subscribing — see SSEHandler.authorizeTenant. The
// watcher itself runs as the service account so it can deliver events for
// any namespace it is asked about, which is why the authz check must be
// external.
//
// sinceID is the Last-Event-ID the client observed before disconnecting;
// any events in the per-tenant replay buffer with a higher ID are returned
// in the missed slice so the caller can flush them to the client before
// pumping live events. Pass 0 for a fresh subscription.
func (wat *Watcher) Subscribe(tenant string, sinceID int64) (chan WatchEvent, []WatchEvent) {
	eventChan := make(chan WatchEvent, eventChannelBuffer)

	wat.mu.Lock()
	defer wat.mu.Unlock()

	subs := wat.subscribers[tenant]
	if subs == nil {
		subs = make(map[chan WatchEvent]struct{})
		wat.subscribers[tenant] = subs
	}

	subs[eventChan] = struct{}{}

	var missed []WatchEvent

	if sinceID > 0 {
		if buf, ok := wat.buffers[tenant]; ok {
			missed = buf.since(sinceID)
		}
	}

	return eventChan, missed
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

	watchEvt := WatchEvent{
		Type:    eventType,
		App:     app,
		EventID: wat.nextEventID.Add(1),
	}

	// Append to the per-tenant replay buffer even when there are
	// no live subscribers right now — a client that connects a
	// second later should still see the event. Buffers are
	// bounded to replayBufferSize so the memory footprint stays
	// predictable.
	wat.mu.Lock()
	buf, bufExists := wat.buffers[namespace]

	if !bufExists {
		buf = &replayBuffer{}
		wat.buffers[namespace] = buf
	}

	buf.add(&watchEvt)

	subs, exists := wat.subscribers[namespace]
	wat.mu.Unlock()

	if !exists {
		return
	}

	for sub := range subs {
		select {
		case sub <- watchEvt:
		default:
			// Warn, not debug: a dropped event means the UI will be
			// momentarily out of sync with cluster state. The fragment
			// refetch on filter/sort still fixes this, but operators
			// should see that a subscriber is behind.
			wat.log.Warn("dropping event for slow subscriber",
				"namespace", namespace,
				"resource", obj.GetName(),
				"event", eventType)
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
