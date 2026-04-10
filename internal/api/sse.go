package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
	"github.com/lexfrei/cozytempl/internal/view/page"
)

// SSEHandler handles Server-Sent Events for real-time updates.
type SSEHandler struct {
	watcher *k8s.Watcher
	log     *slog.Logger
}

// NewSSEHandler creates a new SSE handler.
func NewSSEHandler(watcher *k8s.Watcher, log *slog.Logger) *SSEHandler {
	return &SSEHandler{watcher: watcher, log: log}
}

// sseMessage is the JSON payload delivered to browser EventSource clients.
// Type is one of "added", "status", "removed".
// Status is present for "status" and "added". HTML is present only for "added".
type sseMessage struct {
	Type   string `json:"type"`
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

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)

	// Flush headers immediately so the browser sees the stream open.
	// Also write a retry hint and an initial comment to defeat proxy buffering.
	_, _ = fmt.Fprint(writer, "retry: 5000\n:ok\n\n")

	flusher.Flush()

	events := ssh.watcher.Subscribe(tenant, usr.Username)
	defer ssh.watcher.Unsubscribe(events)

	ssh.log.Info("SSE client connected", "tenant", tenant, "user", usr.Username)

	ssh.pumpEvents(req.Context(), writer, flusher, tenant, events)

	ssh.log.Info("SSE client disconnected", "tenant", tenant, "user", usr.Username)
}

func (ssh *SSEHandler) pumpEvents(
	ctx context.Context,
	writer http.ResponseWriter,
	flusher http.Flusher,
	tenant string,
	events <-chan k8s.WatchEvent,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}

			if !ssh.writeEvent(ctx, writer, flusher, tenant, &evt) {
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

	_, err = fmt.Fprintf(writer, "data: %s\n\n", payload)
	if err != nil {
		return false
	}

	flusher.Flush()

	return true
}

// buildMessage converts a watcher event into a client SSE payload.
// For Added events, renders a full table row via templ.
func (ssh *SSEHandler) buildMessage(ctx context.Context, tenant string, evt *k8s.WatchEvent) *sseMessage {
	switch evt.Type {
	case k8s.WatchEventAdded:
		html, err := renderAppRow(ctx, tenant, &evt.App)
		if err != nil {
			ssh.log.Error("rendering row for added event", "name", evt.App.Name, "error", err)

			return nil
		}

		return &sseMessage{
			Type:   "added",
			Name:   evt.App.Name,
			Status: string(evt.App.Status),
			HTML:   html,
		}

	case k8s.WatchEventModified:
		return &sseMessage{
			Type:   "status",
			Name:   evt.App.Name,
			Status: string(evt.App.Status),
		}

	case k8s.WatchEventDeleted:
		return &sseMessage{
			Type: "removed",
			Name: evt.App.Name,
		}

	default:
		return nil
	}
}

func renderAppRow(ctx context.Context, tenant string, app *k8s.Application) (string, error) {
	var buf bytes.Buffer

	err := page.AppTableRows(tenant, []k8s.Application{*app}).Render(ctx, &buf)
	if err != nil {
		return "", fmt.Errorf("rendering row: %w", err)
	}

	return buf.String(), nil
}
