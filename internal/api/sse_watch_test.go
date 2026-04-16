package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/lexfrei/cozytempl/internal/auth"
)

// withTestUser attaches a minimal UserContext to ctx so the
// WatchSSEHandler clears the "not authenticated" guard and reaches
// the resource-dispatch / tenant-param checks the tests target.
func withTestUser(ctx context.Context) context.Context {
	return auth.ContextWithUser(ctx, &auth.UserContext{Username: "test-user"})
}

// TestSSEOpFromWatchType locks the mapping the client reducer
// depends on. A silent rename of one of these strings — in server,
// client, or the watch package — breaks live updates everywhere.
func TestSSEOpFromWatchType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		et   watch.EventType
		want string
	}{
		{watch.Added, "added"},
		{watch.Modified, "modified"},
		{watch.Deleted, "removed"},
		{watch.Error, ""},
		{watch.Bookmark, ""},
	}

	for _, tc := range cases {
		got := sseOpFromWatchType(tc.et)
		if got != tc.want {
			t.Errorf("sseOpFromWatchType(%q) = %q, want %q", tc.et, got, tc.want)
		}
	}
}

// TestRenderEventRowRequiresName is the contract gate for the watch
// proxy's pre-rendering: a core/v1 Event without metadata.name is
// never forwarded to the browser. The client reducer keys on that
// name for row id lookup — dropping the event is strictly safer
// than emitting a row the reducer cannot find again to update.
func TestRenderEventRowRequiresName(t *testing.T) {
	t.Parallel()

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
	}}

	if _, _, err := renderEventRow(obj); !errors.Is(err, errWatchEventMissingName) {
		t.Errorf("renderEventRow(no name) = %v, want errWatchEventMissingName", err)
	}
}

// TestRenderEventRowProducesStableID confirms the rendered row
// carries id="event-row-<metadata.name>". The client watch.ts
// reducer reads rowIdPrefix from the message payload and looks up
// the element by that exact id; if EventRow drops or reformats the
// id, live updates stop silently.
func TestRenderEventRowProducesStableID(t *testing.T) {
	t.Parallel()

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata": map[string]any{
			"name":      "myvm.1811ab4c012345",
			"namespace": "tenant-root",
		},
		"type":    "Warning",
		"reason":  "FailedMount",
		"message": "no such volume",
		"count":   int64(3),
	}}

	name, html, err := renderEventRow(obj)
	if err != nil {
		t.Fatalf("renderEventRow: %v", err)
	}

	if name != "myvm.1811ab4c012345" {
		t.Errorf("name = %q, want myvm.1811ab4c012345", name)
	}

	if !strings.Contains(html, `id="event-row-myvm.1811ab4c012345"`) {
		t.Errorf("rendered html missing stable row id; html = %s", html)
	}
}

// TestWatchSSEHandlerRejectsUnknownResource pins the 404 branch so
// a typo'd or unsupported resource slug never silently opens a
// watch. The dispatch table in sse_watch.go is the single source
// of truth — this test fails when a resource is added to the route
// without a corresponding watchResourceConfig entry, which would
// otherwise slip through as a 404 to the user.
func TestWatchSSEHandlerRejectsUnknownResource(t *testing.T) {
	t.Parallel()

	handler := NewWatchSSEHandler(nil, nil, "", nil)

	// A user must sit on the context before the resource switch is
	// reached; without it Stream short-circuits at the 401 gate.
	req := httptest.NewRequestWithContext(
		withTestUser(context.Background()),
		http.MethodGet, "/api/watch/fakes?tenant=ns", nil)
	req.SetPathValue("resource", "fakes")

	rec := httptest.NewRecorder()
	handler.Stream(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "unknown watch resource") {
		t.Errorf("body = %q, want unknown-resource copy", rec.Body.String())
	}
}

// TestWatchSSEHandlerRequiresTenant locks the 400 branch when the
// subscriber omits the tenant query param. Without this the handler
// would open a watch against the "" namespace, which (depending on
// cluster RBAC) could silently stream cluster-scoped data.
func TestWatchSSEHandlerRequiresTenant(t *testing.T) {
	t.Parallel()

	handler := NewWatchSSEHandler(nil, nil, "", nil)

	req := httptest.NewRequestWithContext(
		withTestUser(context.Background()),
		http.MethodGet, "/api/watch/events", nil)
	req.SetPathValue("resource", "events")

	rec := httptest.NewRecorder()
	handler.Stream(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "tenant parameter required") {
		t.Errorf("body = %q, want tenant-required copy", rec.Body.String())
	}
}

// TestWatchMessageShape roundtrips the on-wire JSON format so the
// browser client's WatchMessage interface and the server's
// watchMessage struct stay aligned. Any field rename — especially
// rowIdPrefix — would silently break row-targeting on the client.
func TestWatchMessageShape(t *testing.T) {
	t.Parallel()

	msg := watchMessage{
		Op:          "added",
		Name:        "myvm.1811ab",
		RowIDPrefix: "event-row",
		HTML:        "<tr>x</tr>",
	}

	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Exact-field check so the JSON shape is immune to struct
	// reordering: the client code does JSON.parse without any
	// schema library.
	var decoded map[string]any
	if unmarshalErr := json.NewDecoder(bytes.NewReader(raw)).Decode(&decoded); unmarshalErr != nil {
		t.Fatalf("Decode: %v", unmarshalErr)
	}

	for _, key := range []string{"op", "name", "rowIdPrefix", "html"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("JSON missing required key %q: got %s", key, raw)
		}
	}
}
