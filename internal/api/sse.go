package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/config"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view/page"
)

// sseMaxStreamAge caps the lifetime of a single SSE connection. Past
// this, the handler closes the stream cleanly, the browser's
// EventSource reconnects automatically with Last-Event-ID, and
// RequireAuth runs again — picking up a fresh OIDC ID token via the
// refresh loop. Without this cap a long-lived SSE session holds a
// stale Bearer token until the server restarts, which is the entire
// reason passthrough mode needs bounded streams.
//
// Chosen below the 1-hour TTL Keycloak issues by default (15 min for
// access token, 30 min for ID token in stock cozystack) so the
// reconnect always happens while the refresh token is still valid
// but well above the natural replay ring buffer window. 60 minutes
// means worst-case latency between a manual Keycloak revocation and
// the refresh rejection is one hour; operators who need faster
// revocation can lower this.
const sseMaxStreamAge = 60 * time.Minute

// SSE operation strings shared between the shared-SA handler and
// the per-user WatchSSEHandler (sse_watch.go). The client reducer
// dispatches on these exact values — renaming either silently
// breaks the live-update path until every browser tab refreshes.
const (
	sseOpAdded    = "added"
	sseOpModified = "modified"
	sseOpRemoved  = "removed"
)

// SSEHandler handles Server-Sent Events for real-time updates. It authorizes
// each subscribe by doing a user-scoped list against the requested tenant
// namespace — without this check any authenticated user could watch any
// tenant's app stream just by passing ?tenant= in the query string.
type SSEHandler struct {
	watcher *k8s.Watcher
	log     *slog.Logger
	baseCfg *rest.Config
	mode    config.AuthMode
}

// NewSSEHandler creates a new SSE handler. baseCfg is used to build
// user-scoped clients for the per-request tenant access check. Passing nil
// disables authorization and should only be used in tests.
func NewSSEHandler(watcher *k8s.Watcher, baseCfg *rest.Config, mode config.AuthMode, log *slog.Logger) *SSEHandler {
	return &SSEHandler{watcher: watcher, log: log, baseCfg: baseCfg, mode: mode}
}

// ErrTenantAccessDenied is returned when a user tries to subscribe to a
// namespace they cannot list HelmReleases in.
var ErrTenantAccessDenied = errors.New("tenant access denied")

// sseMessage is the JSON payload delivered to browser EventSource clients.
// Op is one of "added", "modified", "removed". HTML is present for added and
// modified (a fully rendered row); status is present when known. The client
// runs a single upsert/delete reducer keyed by Name so all operations go
// through the same code path.
//
// The legacy "type" field is also emitted for backwards compatibility with
// clients that pre-date the unified message shape. It can be removed once
// no old browsers are in flight.
type sseMessage struct {
	Op     string `json:"op"`
	Type   string `json:"type"` // deprecated; mirrors Op
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
	HTML   string `json:"html,omitempty"`
}

// Stream sends real-time events to the client.
func (ssh *SSEHandler) Stream(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	flusher, ok := writer.(http.Flusher)
	if !ok {
		Error(writer, http.StatusInternalServerError, "streaming not supported")

		return
	}

	tenant := req.URL.Query().Get("tenant")
	if tenant == "" {
		Error(writer, http.StatusBadRequest, "tenant parameter required")

		return
	}

	// Authorization: verify the user can actually list resources in this
	// tenant namespace before subscribing. Otherwise any authenticated user
	// could peek at any tenant's updates just by knowing the namespace.
	authErr := ssh.authorizeTenant(req.Context(), usr, tenant)
	if authErr != nil {
		ssh.log.Warn("SSE subscribe denied", "user", usr.Username, "tenant", tenant, "error", authErr)
		Error(writer, http.StatusForbidden, "tenant access denied")

		return
	}

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)

	// Flush headers immediately so the browser sees the stream open.
	// Also write a retry hint and an initial comment to defeat proxy buffering.
	_, _ = fmt.Fprint(writer, "retry: 5000\n:ok\n\n")

	flusher.Flush()

	ssh.subscribeAndStream(req, writer, flusher, tenant, usr.Username)
}

// subscribeAndStream is the second half of Stream — split out so
// the handler stays under the funlen limit. Performs the replay
// for reconnecting clients (Last-Event-ID) and then pumps live
// events until the request context fires or the subscription is
// dropped.
func (ssh *SSEHandler) subscribeAndStream(
	req *http.Request,
	writer http.ResponseWriter,
	flusher http.Flusher,
	tenant, username string,
) {
	// EventSource automatically sends Last-Event-ID on reconnect
	// if we emit `id:` lines with our SSE messages. Parse it and
	// pass to Subscribe so the watcher can replay events in its
	// per-tenant ring buffer that the client missed during the
	// disconnect window.
	sinceID := parseLastEventID(req.Header.Get("Last-Event-ID"))

	events, missed := ssh.watcher.Subscribe(tenant, sinceID)
	defer ssh.watcher.Unsubscribe(events)

	ssh.log.Info("SSE client connected",
		"tenant", tenant,
		"user", username,
		"since_id", sinceID,
		"replayed", len(missed))

	// Replay any events that fired while the client was
	// disconnected, in original order. Each one carries its
	// original EventID so the client's internal Last-Event-ID
	// continues to advance monotonically.
	for idx := range missed {
		if !ssh.writeEvent(req.Context(), writer, flusher, tenant, &missed[idx]) {
			return
		}
	}

	ssh.pumpEvents(req.Context(), writer, flusher, tenant, events)

	ssh.log.Info("SSE client disconnected", "tenant", tenant, "user", username)
}

// parseLastEventID converts the browser's Last-Event-ID header
// into the int64 our watcher uses for replay. Invalid values
// (non-numeric, negative, empty) become 0, which means "send
// everything from scratch" — the safe default for a first
// connection or a malformed replay request.
func parseLastEventID(raw string) int64 {
	if raw == "" {
		return 0
	}

	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		return 0
	}

	return id
}

// authorizeTenant returns nil if the user can list HelmReleases in the tenant
// namespace. A List with limit=1 is used to keep the check cheap and to
// surface the same RBAC error users would see in the UI.
// Returns ErrTenantAccessDenied wrapped with the upstream error details.
func (ssh *SSEHandler) authorizeTenant(
	ctx context.Context,
	usr *auth.UserContext,
	tenant string,
) error {
	if ssh.baseCfg == nil {
		return nil
	}

	client, err := k8s.NewUserClient(ssh.baseCfg, usr, ssh.mode)
	if err != nil {
		return fmt.Errorf("building user client: %w", err)
	}

	_, listErr := client.Resource(k8s.HelmReleaseGVR()).Namespace(tenant).List(ctx, metav1.ListOptions{
		Limit: 1,
	})
	if listErr != nil {
		return fmt.Errorf("%w: %s: %v", ErrTenantAccessDenied, tenant, listErr) //nolint:errorlint // wrap upstream error as details
	}

	return nil
}

func (ssh *SSEHandler) pumpEvents(
	ctx context.Context,
	writer http.ResponseWriter,
	flusher http.Flusher,
	tenant string,
	events <-chan k8s.WatchEvent,
) {
	// Bound the stream lifetime. When the deadline fires, this
	// function returns, the HTTP handler exits, the browser's
	// EventSource sees readyState=CLOSED and reconnects, which
	// re-runs RequireAuth (refreshing the OIDC ID token) and
	// builds a new subscription. The watcher's ring buffer
	// replays any events that happened during the momentary
	// disconnect, so the user sees no data loss.
	streamCtx, cancel := context.WithDeadline(ctx, time.Now().Add(sseMaxStreamAge))
	defer cancel()

	for {
		select {
		case <-streamCtx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}

			if !ssh.writeEvent(streamCtx, writer, flusher, tenant, &evt) {
				return
			}
		}
	}
}

func (ssh *SSEHandler) writeEvent(
	ctx context.Context,
	writer http.ResponseWriter,
	flusher http.Flusher,
	tenant string,
	evt *k8s.WatchEvent,
) bool {
	msg := ssh.buildMessage(ctx, tenant, evt)
	if msg == nil {
		return true
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		ssh.log.Error("marshaling SSE message", "error", err)

		return true
	}

	// Emit `id:` so EventSource records it as the stream's
	// Last-Event-ID. On reconnect the browser automatically
	// sends that value back in the Last-Event-ID header, which
	// parseLastEventID decodes and feeds into Subscribe's replay
	// path. Writing id BEFORE data so the spec's strict parsers
	// attach it to the correct message. The payload is a
	// JSON-encoded struct containing templ-rendered HTML, both
	// of which are already escaped at their source — gosec's
	// taint tracker can't see that, so we silence G705 here.
	_, err = fmt.Fprintf(writer, "id: %d\ndata: %s\n\n", evt.EventID, payload) //nolint:gosec // payload is JSON of templ-escaped content
	if err != nil {
		return false
	}

	flusher.Flush()

	return true
}

// buildMessage converts a watcher event into a client SSE payload. Added and
// modified events carry the fully rendered row HTML so the client can do a
// uniform upsert; removed carries only the name. The client runs a single
// reducer — no per-op handler fan-out.
func (ssh *SSEHandler) buildMessage(ctx context.Context, tenant string, evt *k8s.WatchEvent) *sseMessage {
	if evt.App.Name == "" {
		// HelmReleases without the application.name label slip through if
		// Cozystack's label selector is ever broadened. Ignore silently.
		return nil
	}

	switch evt.Type {
	case k8s.WatchEventAdded, k8s.WatchEventModified:
		operation := sseOpAdded
		if evt.Type == k8s.WatchEventModified {
			operation = sseOpModified
		}

		html, err := renderAppRow(ctx, tenant, &evt.App)
		if err != nil {
			ssh.log.Error("rendering row", "op", operation, "name", evt.App.Name, "error", err)

			return nil
		}

		return &sseMessage{
			Op:     operation,
			Type:   operation,
			Name:   evt.App.Name,
			Status: string(evt.App.Status),
			HTML:   html,
		}

	case k8s.WatchEventDeleted:
		return &sseMessage{
			Op:   sseOpRemoved,
			Type: sseOpRemoved,
			Name: evt.App.Name,
		}

	default:
		return nil
	}
}

func renderAppRow(ctx context.Context, tenant string, app *k8s.Application) (string, error) {
	var buf bytes.Buffer

	err := page.AppTableRow(tenant, *app).Render(ctx, &buf)
	if err != nil {
		return "", fmt.Errorf("rendering row: %w", err)
	}

	return buf.String(), nil
}
