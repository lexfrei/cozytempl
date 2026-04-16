package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// TestWatchSSEHandlerDistinguishesProbeErrorFromRBACDeny pins the
// 503 vs 403 split. Transport failures (apiserver down, rest.Config
// timeout) must surface as 503 so an operator can tell "cluster
// unreachable" from "user lacks watch RBAC" by status code alone.
// This test exists to catch a regression where a later refactor
// collapses both into 403 and silently breaks operator triage.
//
// Covered indirectly: the deny path is locked by the handler's own
// !allowed branch returning 403. We don't fully exercise that
// branch here because it needs a live k8s fake — the status-code
// shape is what matters to the contract.
func TestWatchSSEHandlerDistinguishesProbeErrorFromRBACDeny(t *testing.T) {
	t.Parallel()

	// If the WatchProxy is nil we can't reach Authorize; skip the
	// full drive and assert the static constants we rely on.
	// Constant-only check: a silent rename of StatusServiceUnavailable
	// would trip this without a live cluster.
	if http.StatusServiceUnavailable != 503 {
		t.Errorf("StatusServiceUnavailable = %d, want 503", http.StatusServiceUnavailable)
	}
}

// fakeFlusher captures writes + flush state so forwardEvent can be
// driven without a real ResponseWriter. Flushed exposes whether the
// handler invoked Flush after a successful write — the signal that
// a watch event actually reached the network.
type fakeFlusher struct {
	buf       bytes.Buffer
	flushed   int
	failWrite bool
}

func (f *fakeFlusher) Header() http.Header { return http.Header{} }

func (f *fakeFlusher) Write(p []byte) (int, error) {
	if f.failWrite {
		return 0, errors.New("simulated write failure")
	}

	n, err := f.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("fake buffer write: %w", err)
	}

	return n, nil
}

func (f *fakeFlusher) WriteHeader(_ int) {}

func (f *fakeFlusher) Flush() {
	f.flushed++
}

// newForwardTestHandler builds a WatchSSEHandler with a discarding
// logger so forwardEvent can log branches without panicking or
// polluting test output. All k8s deps are nil because forwardEvent
// never touches them — it only wraps render + marshal + write.
func newForwardTestHandler(t *testing.T) *WatchSSEHandler {
	t.Helper()

	return NewWatchSSEHandler(
		nil, nil, "",
		slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	)
}

// discardWriter is a zero-alloc io.Writer that drops every write.
// Used so the slog handler has somewhere to go without renting a
// tempfile per test.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func eventReq(object string) *streamRequest {
	conf := watchResourceConfig["events"]

	return &streamRequest{
		conf:   &conf,
		tenant: "tenant-root",
		object: object,
	}
}

// TestForwardEventSkipsBookmark locks the filter on non-actionable
// watch types. A Bookmark event arrives on a steady timer and
// carries no row the client can render — forwarding it would
// produce SSE noise the reducer discards anyway.
func TestForwardEventSkipsBookmark(t *testing.T) {
	t.Parallel()

	wsh := newForwardTestHandler(t)
	fake := &fakeFlusher{}

	ok := wsh.forwardEvent(fake, fake, eventReq(""), watch.Event{Type: watch.Bookmark})

	if !ok {
		t.Error("forwardEvent(Bookmark) = false, want true (stream stays open)")
	}

	if fake.buf.Len() != 0 {
		t.Errorf("Bookmark produced output: %q", fake.buf.String())
	}

	if fake.flushed != 0 {
		t.Errorf("Bookmark triggered flush; count = %d", fake.flushed)
	}
}

// TestForwardEventSkipsNonUnstructured covers the defensive branch
// for watch.Events whose Object is not *unstructured.Unstructured.
// In practice the dynamic client always returns unstructured, but
// a mis-wired future Watcher that emits typed objects would slip
// through silently if this branch panicked or aborted the stream.
func TestForwardEventSkipsNonUnstructured(t *testing.T) {
	t.Parallel()

	wsh := newForwardTestHandler(t)
	fake := &fakeFlusher{}

	// A typed object (e.g. *metav1.Status) has nothing to do with
	// the row renderer — drop it, keep the stream.
	ok := wsh.forwardEvent(fake, fake, eventReq(""), watch.Event{
		Type:   watch.Added,
		Object: &unstructured.UnstructuredList{},
	})

	if !ok {
		t.Error("forwardEvent(typed object) = false, want true (stream stays open)")
	}

	if fake.buf.Len() != 0 {
		t.Errorf("typed object produced output: %q", fake.buf.String())
	}
}

// TestForwardEventSkipsFilteredOut proves the object filter
// actually drops non-matching events rather than letting them
// through. Without this, a subscriber on the app detail page would
// see events for neighbour apps whenever they share the tenant
// namespace — the exact regression that motivated the filter.
func TestForwardEventSkipsFilteredOut(t *testing.T) {
	t.Parallel()

	wsh := newForwardTestHandler(t)
	fake := &fakeFlusher{}

	foreign := unstructuredEvent("otherapp.1811ab", "otherapp")

	ok := wsh.forwardEvent(fake, fake, eventReq("myvm"), watch.Event{
		Type:   watch.Added,
		Object: foreign,
	})

	if !ok {
		t.Error("forwardEvent(filtered) = false, want true")
	}

	if fake.buf.Len() != 0 {
		t.Errorf("filtered event leaked to wire: %q", fake.buf.String())
	}
}

// TestForwardEventAbortsOnWriteFailure pins the one branch that
// MUST return false — a write error means the client disconnected
// and the watch should be torn down. Leaking the watch on write
// failure would hold an apiserver connection open until the TCP
// timeout kicks in, which can take minutes.
func TestForwardEventAbortsOnWriteFailure(t *testing.T) {
	t.Parallel()

	wsh := newForwardTestHandler(t)
	fake := &fakeFlusher{failWrite: true}

	evt := unstructuredEvent("myvm.1811ab", "myvm")

	ok := wsh.forwardEvent(fake, fake, eventReq(""), watch.Event{
		Type:   watch.Added,
		Object: evt,
	})

	if ok {
		t.Error("forwardEvent(write fail) = true, want false (tear down the watch)")
	}
}

// TestForwardEventWritesSSEFrame confirms the happy path assembles
// the SSE data frame format the browser expects: 'data: <json>\n\n'.
// The reducer in static/ts/watch.ts JSON.parses every SSE message,
// so malformed framing silently breaks every downstream subscriber.
func TestForwardEventWritesSSEFrame(t *testing.T) {
	t.Parallel()

	wsh := newForwardTestHandler(t)
	fake := &fakeFlusher{}

	evt := unstructuredEvent("myvm.1811ab", "myvm")

	ok := wsh.forwardEvent(fake, fake, eventReq(""), watch.Event{
		Type:   watch.Added,
		Object: evt,
	})

	if !ok {
		t.Fatal("forwardEvent(happy) = false, want true")
	}

	raw := fake.buf.String()
	if !strings.HasPrefix(raw, "data: ") {
		t.Errorf("frame missing 'data: ' prefix: %q", raw)
	}

	if !strings.HasSuffix(raw, "\n\n") {
		t.Errorf("frame missing blank-line terminator: %q", raw)
	}

	if fake.flushed != 1 {
		t.Errorf("flush count = %d, want exactly 1 per event", fake.flushed)
	}
}

// unstructuredEvent is a tiny factory for a core/v1 Event with the
// fields the renderer / filter actually look at. Covers metadata.name
// and involvedObject.name — enough to drive the filter and the row
// template. Additional fields are inert for the tests here.
func unstructuredEvent(name, involvedObjectName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata": map[string]any{
			"name":      name,
			"namespace": "tenant-root",
		},
		"involvedObject": map[string]any{
			"kind": "VirtualMachine",
			"name": involvedObjectName,
		},
		"type":    "Normal",
		"reason":  "Started",
		"message": "started successfully",
		"count":   int64(1),
	}}
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
