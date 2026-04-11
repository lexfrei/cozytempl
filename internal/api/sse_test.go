package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/lexfrei/cozytempl/internal/k8s"
)

// TestBuildMessageReturnsHTMLForAddedAndModified locks in the unified
// resource-change protocol: both added and modified must carry fully
// rendered row HTML so the client can run one upsert path keyed by name.
func TestBuildMessageReturnsHTMLForAddedAndModified(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(testingWriter{t}, nil))
	handler := NewSSEHandler(nil, nil, log)

	cases := []struct {
		name   string
		evt    k8s.WatchEvent
		wantOp string
	}{
		{
			name: "added",
			evt: k8s.WatchEvent{
				Type: k8s.WatchEventAdded,
				App: k8s.Application{
					Name:   "redis-1",
					Kind:   "Redis",
					Status: k8s.AppStatusReady,
				},
			},
			wantOp: "added",
		},
		{
			name: "modified",
			evt: k8s.WatchEvent{
				Type: k8s.WatchEventModified,
				App: k8s.Application{
					Name:   "redis-1",
					Kind:   "Redis",
					Status: k8s.AppStatusReconciling,
				},
			},
			wantOp: "modified",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			msg := handler.buildMessage(context.Background(), "tenant-root", &tc.evt)
			if msg == nil {
				t.Fatalf("buildMessage returned nil for %s", tc.name)
			}

			if msg.Op != tc.wantOp {
				t.Errorf("Op = %q, want %q", msg.Op, tc.wantOp)
			}
			// Legacy field kept in sync for old clients.
			if msg.Type != tc.wantOp {
				t.Errorf("Type = %q, want %q", msg.Type, tc.wantOp)
			}
			if msg.Name != "redis-1" {
				t.Errorf("Name = %q, want %q", msg.Name, "redis-1")
			}
			if msg.HTML == "" {
				t.Error("HTML is empty; client needs the rendered row to upsert")
			}
			// Sanity: the HTML should contain the row id so sse.ts lookups work.
			if !strings.Contains(msg.HTML, `id="row-redis-1"`) {
				t.Errorf("HTML missing row id; got:\n%s", msg.HTML)
			}
		})
	}
}

// TestBuildMessageDeletedHasNoHTML keeps the delete event small — the client
// only needs the name to remove the element.
func TestBuildMessageDeletedHasNoHTML(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(testingWriter{t}, nil))
	handler := NewSSEHandler(nil, nil, log)

	evt := k8s.WatchEvent{
		Type: k8s.WatchEventDeleted,
		App:  k8s.Application{Name: "redis-1"},
	}

	msg := handler.buildMessage(context.Background(), "tenant-root", &evt)
	if msg == nil {
		t.Fatal("buildMessage returned nil for deleted")
	}

	if msg.Op != "removed" {
		t.Errorf("Op = %q, want %q", msg.Op, "removed")
	}
	if msg.HTML != "" {
		t.Errorf("HTML should be empty on delete; got %q", msg.HTML)
	}
}

// TestBuildMessageDropsAppsWithoutName guards the watcher label-selector
// contract: if a HelmRelease is missing the application.name label we drop
// it silently rather than emit an event with an empty name that the client
// would have to defend against.
func TestBuildMessageDropsAppsWithoutName(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(testingWriter{t}, nil))
	handler := NewSSEHandler(nil, nil, log)

	evt := k8s.WatchEvent{
		Type: k8s.WatchEventAdded,
		App:  k8s.Application{Name: "", Kind: "Redis"},
	}

	if msg := handler.buildMessage(context.Background(), "tenant-root", &evt); msg != nil {
		t.Errorf("buildMessage should return nil for empty name; got %+v", msg)
	}
}

// TestAuthorizeTenantNilBaseCfgAllows makes sure the nil-baseCfg escape hatch
// used in tests does not crash and behaves as allow-all.
func TestAuthorizeTenantNilBaseCfgAllows(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(testingWriter{t}, nil))
	handler := NewSSEHandler(nil, nil, log)

	err := handler.authorizeTenant(context.Background(), "alice", []string{"dev"}, "tenant-alice")
	if err != nil {
		t.Errorf("authorizeTenant with nil baseCfg should allow; got %v", err)
	}
}

// TestSSEMessageMarshalHasUnifiedFields verifies the wire format has both the
// new "op" field and the legacy "type" mirror, plus html/status are optional.
func TestSSEMessageMarshalHasUnifiedFields(t *testing.T) {
	t.Parallel()

	msg := sseMessage{
		Op:     "added",
		Type:   "added",
		Name:   "foo",
		Status: "Ready",
		HTML:   "<tr id=\"row-foo\"></tr>",
	}

	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := `{"op":"added","type":"added","name":"foo","status":"Ready","html":"\u003ctr id=\"row-foo\"\u003e\u003c/tr\u003e"}`
	if string(raw) != want {
		t.Errorf("marshal mismatch\n got: %s\nwant: %s", raw, want)
	}

	// Removed op has no html/status fields.
	del := sseMessage{Op: "removed", Type: "removed", Name: "foo"}

	raw, err = json.Marshal(del)
	if err != nil {
		t.Fatalf("marshal deleted: %v", err)
	}

	if !strings.Contains(string(raw), `"op":"removed"`) {
		t.Errorf("deleted marshal missing op: %s", raw)
	}
	if strings.Contains(string(raw), `"html"`) {
		t.Errorf("deleted marshal should omit empty html: %s", raw)
	}
	if strings.Contains(string(raw), `"status"`) {
		t.Errorf("deleted marshal should omit empty status: %s", raw)
	}
}

// testingWriter adapts testing.T to io.Writer so slog output goes to the
// test log instead of stderr, and stays attached to the failing test.
type testingWriter struct {
	t *testing.T
}

func (w testingWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(p), "\n"))

	return len(p), nil
}
