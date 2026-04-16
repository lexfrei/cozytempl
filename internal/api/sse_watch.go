package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view/page"
)

// watchStreamMaxAge caps the lifetime of a user-credentialed watch
// stream. Shorter than the HelmRelease SSEHandler's cap (60 min)
// because the apiserver bearer token carried over HTTP for
// passthrough auth has no refresh path inside the stream — a stale
// token would turn the watch into a 401 silently. 30 minutes keeps
// us well under Keycloak's default 30 min ID-token TTL while still
// amortising reconnect cost across most natural session lengths.
const watchStreamMaxAge = 30 * time.Minute

// errWatchEventMissingName is returned by a resource renderer when
// an incoming watch.Event has no metadata.name. Declared once so
// err113 stays happy and callers compare with errors.Is instead of
// substring-matching a dynamic message.
var errWatchEventMissingName = errors.New("watch event missing metadata.name")

// WatchSSEHandler streams Kubernetes watch events over Server-Sent
// Events using the subscribing user's credentials. Unlike the
// shared-SA SSEHandler above, every subscription opens its own
// watch against the apiserver with the caller's rest.Config. The
// handler pre-authorizes via SelfSubjectAccessReview (verb=watch)
// so a denied subscription returns 403 upfront instead of an
// empty SSE stream.
//
// Current scope: core/v1 Events in a tenant namespace, rendered
// as <tr> rows the Events tab already uses. Future resources
// (Jobs, Pods, conditions) follow the same pattern — add a
// resourceTargetPrefix entry and a renderer, no handler rewiring.
type WatchSSEHandler struct {
	proxy   *k8s.WatchProxy
	baseCfg *rest.Config
	mode    config.AuthMode
	log     *slog.Logger
}

// NewWatchSSEHandler wires the handler to a shared WatchProxy. The
// proxy itself is stateless; the handler holds the baseCfg + auth
// mode so every request can build a per-user rest.Config.
func NewWatchSSEHandler(proxy *k8s.WatchProxy, baseCfg *rest.Config, mode config.AuthMode, log *slog.Logger) *WatchSSEHandler {
	return &WatchSSEHandler{proxy: proxy, baseCfg: baseCfg, mode: mode, log: log}
}

// rowRenderer is the signature every per-resource row renderer
// satisfies: unstructured watch object in, (element name, HTML,
// error) out. Named so gocritic's unnamedResult stays happy with
// the renderRow field in watchResourceConfig.
type rowRenderer func(obj *unstructured.Unstructured) (string, string, error)

// watchResource is one entry in the resource dispatch table. Keeping
// this as a named type (rather than an anonymous struct in the map
// literal) lets the row-renderer signature pick up a name for lint.
type watchResource struct {
	gvr         schema.GroupVersionResource
	rowIDPrefix string
	renderRow   rowRenderer
}

// watchResourceConfig maps a human-readable resource kind (the
// {resource} path parameter) to the apiserver GVR plus the DOM row
// id prefix the client reducer uses to find existing rows. Keeping
// the mapping in one place means a future resource addition only
// touches this table plus a renderer; the HTTP handler stays
// resource-agnostic.
//
//nolint:gochecknoglobals // read-only init-time dispatch table
var watchResourceConfig = map[string]watchResource{
	"events": {
		gvr:         schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"},
		rowIDPrefix: "event-row",
		renderRow:   renderEventRow,
	},
}

// Stream serves GET /api/watch/{resource}?tenant={tenant}. The
// resource path segment is one of watchResourceConfig's keys; the
// tenant query param is the namespace to watch.
func (wsh *WatchSSEHandler) Stream(writer http.ResponseWriter, req *http.Request) {
	usr, conf, tenant, ok := wsh.validateStreamRequest(writer, req)
	if !ok {
		return
	}

	flusher, ok := writer.(http.Flusher)
	if !ok {
		Error(writer, http.StatusInternalServerError, "streaming not supported")

		return
	}

	userCfg, ok := wsh.authorize(writer, req, usr, conf, tenant)
	if !ok {
		return
	}

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)

	_, _ = fmt.Fprint(writer, "retry: 5000\n:ok\n\n")

	flusher.Flush()
	wsh.pump(req, writer, flusher, userCfg, conf, tenant)
}

// validateStreamRequest handles the 400/401/404 path-and-query
// validation up front so Stream stays under the funlen budget. The
// ok return doubles as "response already written; bail now".
func (wsh *WatchSSEHandler) validateStreamRequest(
	writer http.ResponseWriter, req *http.Request,
) (*auth.UserContext, watchResource, string, bool) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return nil, watchResource{}, "", false
	}

	resource := req.PathValue("resource")

	conf, known := watchResourceConfig[resource]
	if !known {
		Error(writer, http.StatusNotFound, "unknown watch resource: "+resource)

		return nil, watchResource{}, "", false
	}

	tenant := req.URL.Query().Get("tenant")
	if tenant == "" {
		Error(writer, http.StatusBadRequest, "tenant parameter required")

		return nil, watchResource{}, "", false
	}

	return usr, conf, tenant, true
}

// authorize builds the per-user rest.Config and runs an upfront
// SelfSubjectAccessReview. On any failure it writes the HTTP error
// and returns ok=false; the caller bails out without opening a
// stream.
func (wsh *WatchSSEHandler) authorize(
	writer http.ResponseWriter, req *http.Request,
	usr *auth.UserContext, conf watchResource, tenant string,
) (*rest.Config, bool) {
	userCfg, err := k8s.BuildUserRESTConfig(wsh.baseCfg, usr, wsh.mode)
	if err != nil {
		wsh.log.Error("building user rest config for watch",
			"resource", conf.gvr.Resource, "tenant", tenant, "error", err)
		Error(writer, http.StatusInternalServerError, "building user client")

		return nil, false
	}

	allowed, err := wsh.proxy.Authorize(req.Context(), userCfg, conf.gvr, tenant)
	if err != nil {
		wsh.log.Warn("watch authorization probe failed",
			"resource", conf.gvr.Resource, "tenant", tenant, "user", usr.Username, "error", err)
		Error(writer, http.StatusForbidden, "watch authorization failed")

		return nil, false
	}

	if !allowed {
		wsh.log.Info("watch subscribe denied by RBAC",
			"resource", conf.gvr.Resource, "tenant", tenant, "user", usr.Username)
		Error(writer, http.StatusForbidden, "watch access denied")

		return nil, false
	}

	return userCfg, true
}

// pump opens the watch and forwards events until the request
// context fires or the stream hits watchStreamMaxAge.
func (wsh *WatchSSEHandler) pump(
	req *http.Request,
	writer http.ResponseWriter,
	flusher http.Flusher,
	userCfg *rest.Config,
	conf watchResource,
	tenant string,
) {
	streamCtx, cancel := context.WithDeadline(req.Context(), time.Now().Add(watchStreamMaxAge))
	defer cancel()

	w, err := wsh.proxy.Stream(streamCtx, userCfg, conf.gvr, tenant, "")
	if err != nil {
		wsh.log.Warn("opening watch stream",
			"resource", conf.gvr.Resource, "tenant", tenant, "error", err)

		return
	}

	defer w.Stop()

	for {
		select {
		case <-streamCtx.Done():
			return
		case evt, ok := <-w.ResultChan():
			if !ok {
				return
			}

			if !wsh.forwardEvent(writer, flusher, conf.rowIDPrefix, conf.renderRow, evt) {
				return
			}
		}
	}
}

// forwardEvent translates one watch.Event into one SSE message.
// Returns false to signal the caller to abort the stream (write
// error). Skipping a single malformed event is safer than aborting
// the whole subscription, so recoverable problems return true.
func (wsh *WatchSSEHandler) forwardEvent(
	writer http.ResponseWriter,
	flusher http.Flusher,
	rowIDPrefix string,
	renderRow rowRenderer,
	evt watch.Event,
) bool {
	operation := sseOpFromWatchType(evt.Type)
	if operation == "" {
		// watch.Error and watch.Bookmark are not actionable for the
		// client's row reducer. Bookmarks in particular arrive on a
		// steady timer and would bloat the SSE stream with no user
		// benefit.
		return true
	}

	obj, ok := evt.Object.(*unstructured.Unstructured)
	if !ok {
		wsh.log.Warn("watch event with non-unstructured object",
			"type", evt.Type, "raw", fmt.Sprintf("%T", evt.Object))

		return true
	}

	name, html, err := renderRow(obj)
	if err != nil {
		wsh.log.Warn("rendering watch event row",
			"name", obj.GetName(), "error", err)

		return true
	}

	payload, err := json.Marshal(watchMessage{
		Op:          operation,
		Name:        name,
		HTML:        html,
		RowIDPrefix: rowIDPrefix,
	})
	if err != nil {
		wsh.log.Error("marshalling watch SSE payload", "error", err)

		return true
	}

	_, writeErr := fmt.Fprintf(writer, "data: %s\n\n", payload)
	if writeErr != nil {
		return false
	}

	flusher.Flush()

	return true
}

// watchMessage is the JSON payload delivered to browser clients
// subscribed via /api/watch/{resource}. Op mirrors the shared-SA
// SSEHandler's vocabulary. RowIDPrefix is the DOM id prefix — the
// element the reducer targets is document.getElementById(
// RowIDPrefix + "-" + Name). Delete payloads omit HTML.
type watchMessage struct {
	Op          string `json:"op"`
	Name        string `json:"name"`
	RowIDPrefix string `json:"rowIdPrefix"`
	HTML        string `json:"html,omitempty"`
}

// sseOpFromWatchType maps k8s watch.EventType to the op string the
// client reducer dispatches on. Returns "" for types we do not
// forward (Error, Bookmark).
func sseOpFromWatchType(eventType watch.EventType) string {
	switch eventType {
	case watch.Added:
		return sseOpAdded
	case watch.Modified:
		return sseOpModified
	case watch.Deleted:
		return sseOpRemoved
	case watch.Error, watch.Bookmark:
		return ""
	}

	return ""
}

// renderEventRow converts a core/v1 Event unstructured object into
// the HTML fragment emitted on the wire. Uses the same templ
// component the initial page render uses, so a live event is
// visually identical to a refreshed-tab event. Returns
// (metadata.name, rendered HTML, err) — naming the three strings
// would trip nonamedreturns, which outranks gocritic's
// unnamedResult preference for small files like this one.
//
//nolint:gocritic // unnamedResult: nonamedreturns forbids the alternative
func renderEventRow(obj *unstructured.Unstructured) (string, string, error) {
	name := obj.GetName()
	if name == "" {
		return "", "", errWatchEventMissingName
	}

	evt := k8s.EventFromUnstructured(obj)

	var buf bytes.Buffer

	renderErr := page.EventRow(evt).Render(context.Background(), &buf)
	if renderErr != nil {
		return "", "", fmt.Errorf("rendering event row: %w", renderErr)
	}

	return name, buf.String(), nil
}
