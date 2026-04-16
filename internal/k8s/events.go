package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
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
	mode    config.AuthMode
}

// NewEventService creates a new event service.
func NewEventService(baseCfg *rest.Config, mode config.AuthMode) *EventService {
	return &EventService{baseCfg: baseCfg, mode: mode}
}

func eventGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"}
}

// ListInNamespace returns the most recent events in the given namespace,
// sorted newest-first. limit caps the returned slice after sorting.
func (evs *EventService) ListInNamespace(
	ctx context.Context, usr *auth.UserContext, namespace string, limit int,
) ([]Event, error) {
	if namespace == "" {
		return nil, ErrNamespaceRequired
	}

	client, err := NewUserClient(evs.baseCfg, usr, evs.mode)
	if err != nil {
		return nil, err
	}

	list, listErr := client.Resource(eventGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if listErr != nil {
		return nil, fmt.Errorf("listing events in %s: %w", namespace, listErr)
	}

	return toSortedEvents(list.Items, limit), nil
}

// ListForObject returns events whose involvedObject.name is either
// the given release name itself, or a name derived from it by the
// common Kubernetes naming patterns controllers emit — StatefulSet
// pods (`myapp-0`), CNPG instances (`myapp-1`), headless Services
// (`myapp-r`), PVCs with a `data-` prefix (`data-myapp-0`), etc. The
// previous exact-match policy left the Events tab looking empty
// whenever the actual trouble was on a subresource, which is the
// usual case. See nameDerivedFromRelease for the exact set of
// patterns.
//
// The filter runs client-side because the K8s fieldSelector for
// events does not reliably support involvedObject.name across all
// clusters. The result is de-duplicated on (object, reason) so a
// controller that fires the same transition dozens of times does
// not drown out everything else — the UI only needs one
// representative per (object, reason), keyed on the most recent
// occurrence.
func (evs *EventService) ListForObject(
	ctx context.Context, usr *auth.UserContext, namespace, name string, limit int,
) ([]Event, error) {
	if namespace == "" {
		return nil, ErrNamespaceRequired
	}

	client, err := NewUserClient(evs.baseCfg, usr, evs.mode)
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
		if nameDerivedFromRelease(objName, name) {
			matched = append(matched, list.Items[idx])
		}
	}

	return dedupeByObjectReason(toSortedEvents(matched, 0), limit), nil
}

// NameDerivedFromRelease is the exported counterpart of the
// internal helper used by ListForObject. Shared so the SSE watch
// proxy (internal/api/sse_watch.go) can filter live events with
// the same "is this event about my app?" rule the initial page
// render already applies — keeping the live stream in sync with
// the paginated tab instead of leaking cross-app events.
func NameDerivedFromRelease(objName, release string) bool {
	return nameDerivedFromRelease(objName, release)
}

// nameDerivedFromRelease reports whether objName looks like a
// resource "belonging to" release: an exact match, or the release
// embedded as a hyphen-delimited segment of the name. This covers:
//
//   - exact:    myapp
//   - suffix:   myapp-0, myapp-primary, myapp-headless
//   - prefix:   data-myapp (PVCs named by volumeClaimTemplate)
//   - middle:   data-myapp-0
//
// The segment boundary check avoids sub-string false positives like
// "myapp2-foo" matching release "myapp", which a naive
// strings.Contains would catch incorrectly.
func nameDerivedFromRelease(objName, release string) bool {
	if release == "" || objName == "" {
		return false
	}

	if objName == release {
		return true
	}

	if strings.HasPrefix(objName, release+"-") {
		return true
	}

	if strings.HasSuffix(objName, "-"+release) {
		return true
	}

	return strings.Contains(objName, "-"+release+"-")
}

// dedupeByObjectReason keeps the most recent Event for each
// (Object, Reason) pair, then caps the returned slice at limit.
// Assumes events are already sorted newest-first — the first
// occurrence for any key wins. A limit <= 0 disables the cap.
func dedupeByObjectReason(events []Event, limit int) []Event {
	type key struct {
		Object string
		Reason string
	}

	seen := make(map[key]struct{}, len(events))
	out := make([]Event, 0, len(events))

	// Index access avoids copying the Event struct on every loop —
	// Event is small today but grows whenever a new display field
	// lands, and lint (gocritic rangeValCopy) flags the copy even
	// at the current size.
	for i := range events {
		dedupeKey := key{Object: events[i].Object, Reason: events[i].Reason}

		if _, ok := seen[dedupeKey]; ok {
			continue
		}

		seen[dedupeKey] = struct{}{}

		out = append(out, events[i])
	}

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}

	return out
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

// EventFromUnstructured converts a raw core/v1 Event unstructured
// object into the simplified Event shape the UI renders. Exported
// so the SSE watch-proxy handler can reuse the same conversion the
// paginated list path uses — keeping "live event row" and "refreshed
// tab row" visually identical.
func EventFromUnstructured(obj *unstructured.Unstructured) Event {
	return toEvent(obj)
}

func toEvent(obj *unstructured.Unstructured) Event {
	count, _, _ := unstructured.NestedInt64(obj.Object, "count")

	return Event{
		Name:      obj.GetName(),
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
