package k8s

import (
	"context"
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// EventService reads Kubernetes Events visible to the current user.
// Used by the UI to surface lifecycle / reconciliation events for a
// tenant namespace or a specific application. Every call is impersonated
// so a user only sees events in namespaces they have permission on.
type EventService struct {
	baseCfg *rest.Config
}

// NewEventService creates a new event service.
func NewEventService(baseCfg *rest.Config) *EventService {
	return &EventService{baseCfg: baseCfg}
}

func eventGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"}
}

// ListInNamespace returns the most recent events in the given namespace,
// sorted newest-first. limit caps the returned slice after sorting.
func (evs *EventService) ListInNamespace(
	ctx context.Context, username string, groups []string, namespace string, limit int,
) ([]Event, error) {
	if namespace == "" {
		return nil, ErrNamespaceRequired
	}

	client, err := NewImpersonatingClient(evs.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	list, listErr := client.Resource(eventGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if listErr != nil {
		return nil, fmt.Errorf("listing events in %s: %w", namespace, listErr)
	}

	return toSortedEvents(list.Items, limit), nil
}

// ListForObject returns events whose involvedObject.name equals the given
// name (typical for an app like a Redis CR). The filter runs client-side
// because the K8s fieldSelector for events does not reliably support
// involvedObject.name across all clusters.
func (evs *EventService) ListForObject(
	ctx context.Context, username string, groups []string, namespace, name string, limit int,
) ([]Event, error) {
	if namespace == "" {
		return nil, ErrNamespaceRequired
	}

	client, err := NewImpersonatingClient(evs.baseCfg, username, groups)
	if err != nil {
		return nil, err
	}

	list, listErr := client.Resource(eventGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if listErr != nil {
		return nil, fmt.Errorf("listing events in %s: %w", namespace, listErr)
	}

	matched := make([]unstructured.Unstructured, 0)

	for idx := range list.Items {
		objName := nestedString(list.Items[idx].Object, "involvedObject", "name")
		if objName == name {
			matched = append(matched, list.Items[idx])
		}
	}

	return toSortedEvents(matched, limit), nil
}

// toSortedEvents converts raw Event items to the simplified Event type and
// returns them sorted by LastSeen descending, truncated to limit. A limit
// <= 0 returns everything.
func toSortedEvents(items []unstructured.Unstructured, limit int) []Event {
	events := make([]Event, 0, len(items))

	for idx := range items {
		events = append(events, toEvent(&items[idx]))
	}

	sort.Slice(events, func(i, j int) bool {
		return events[i].LastSeen.After(events[j].LastSeen)
	})

	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}

	return events
}

func toEvent(obj *unstructured.Unstructured) Event {
	count, _, _ := unstructured.NestedInt64(obj.Object, "count")

	return Event{
		Type:      nestedString(obj.Object, "type"),
		Reason:    nestedString(obj.Object, "reason"),
		Message:   nestedString(obj.Object, "message"),
		Object:    eventObjectRef(obj),
		Source:    eventSource(obj),
		Count:     count,
		FirstSeen: parseEventTime(nestedString(obj.Object, "firstTimestamp"), time.RFC3339),
		LastSeen:  pickEventLastSeen(obj),
	}
}

func eventObjectRef(obj *unstructured.Unstructured) string {
	kind := nestedString(obj.Object, "involvedObject", "kind")
	name := nestedString(obj.Object, "involvedObject", "name")

	if kind != "" && name != "" {
		return kind + "/" + name
	}

	return ""
}

func eventSource(obj *unstructured.Unstructured) string {
	component := nestedString(obj.Object, "source", "component")
	host := nestedString(obj.Object, "source", "host")

	switch {
	case component != "" && host != "":
		return component + " on " + host
	case component != "":
		return component
	case host != "":
		return host
	default:
		return ""
	}
}

// pickEventLastSeen reads lastTimestamp first and falls back to eventTime
// (newer events API) to support both shapes without caring which cluster
// version we're on.
func pickEventLastSeen(obj *unstructured.Unstructured) time.Time {
	if t := parseEventTime(nestedString(obj.Object, "lastTimestamp"), time.RFC3339); !t.IsZero() {
		return t
	}

	return parseEventTime(nestedString(obj.Object, "eventTime"), time.RFC3339Nano)
}

func parseEventTime(raw, layout string) time.Time {
	if raw == "" {
		return time.Time{}
	}

	parsed, err := time.Parse(layout, raw)
	if err != nil {
		return time.Time{}
	}

	return parsed
}
