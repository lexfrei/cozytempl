package api

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
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

	events := ssh.watcher.Subscribe(tenant, usr.Username)
	defer ssh.watcher.Unsubscribe(events)

	ssh.log.Info("SSE client connected", "tenant", tenant, "user", usr.Username)

	for {
		select {
		case <-req.Context().Done():
			ssh.log.Info("SSE client disconnected", "tenant", tenant, "user", usr.Username)

			return
		case evt, ok := <-events:
			if !ok {
				return
			}

			_, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", evt.Type, evt.Data)
			if err != nil {
				return
			}

			flusher.Flush()
		}
	}
}
