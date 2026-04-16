package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	// gorilla/websocket sits on a post-v1.5.3 pseudo-version
	// because the tagged v1.5.3 conflicts with the k8s.io/client-go
	// v0.35 line (downgrading client-go loses subresource packages
	// we depend on). The pseudo-version is a published upstream
	// commit with no behavioural divergence; pin the tag when the
	// upstream ecosystem catches up.
	"github.com/gorilla/websocket"

	"github.com/lexfrei/cozytempl/internal/audit"
	"github.com/lexfrei/cozytempl/internal/auth"
	"github.com/lexfrei/cozytempl/internal/k8s"
)

const (
	// logTailStreamMaxAge caps the lifetime of a single pod-log
	// WebSocket. Matches the watch-proxy cap (30 min) for the
	// same reason: on passthrough auth the bearer token can
	// expire inside a long stream, and reconnect is the only
	// way to refresh it. Browsers reconnect automatically and
	// the new request picks up a fresh token if Keycloak issued
	// one.
	logTailStreamMaxAge = 30 * time.Minute

	// logTailDefault is the tailLines bootstrap the UI sees when
	// it connects without specifying a backfill. 500 lines is
	// enough to show the last minute or two of most pods without
	// overwhelming the browser on first paint.
	logTailDefault int64 = 500

	// logTailMax bounds ?tail= so a user cannot ask the apiserver
	// for an arbitrarily large initial buffer. 5000 lines roughly
	// matches a typical terminal scrollback.
	logTailMax int64 = 5000

	// logReadChunk is the buffer handed to bufio.Reader.Read.
	// Small enough that individual log lines get forwarded
	// promptly; big enough that a chatty pod does not burn
	// syscalls.
	logReadChunk = 4 * 1024

	// wsPingInterval is how often the handler sends a WebSocket
	// Ping frame so intermediate proxies do not close the
	// connection as idle. Below the typical 60 s idle-close of
	// Cloudflare / nginx defaults.
	wsPingInterval = 30 * time.Second

	// wsWriteDeadline caps the blocking window on a single
	// write. A dead peer should surface as an error within
	// seconds, not pile up behind TCP send buffer flushes.
	wsWriteDeadline = 10 * time.Second

	// wsReadBufferSize / wsWriteBufferSize are the handshake
	// buffers for the upgrader. The read side only ever carries
	// tiny control frames; the write side carries log chunks
	// which are bounded by logReadChunk.
	wsReadBufferSize  = 1024
	wsWriteBufferSize = logReadChunk

	// forwardChanDepth is the backlog between the log reader
	// goroutine and the WebSocket writer. Deep enough to absorb
	// a brief stall on the browser side, shallow enough that
	// memory stays O(forwardChanDepth * logReadChunk).
	forwardChanDepth = 4
)

// wsLogUpgrader accepts WebSocket handshakes for the log-tail
// endpoint. Check-origin is strict: same-origin only, no
// permissive default. The upgrader is package-level so its
// buffer configuration is shared rather than re-allocated per
// request.
//
//nolint:gochecknoglobals // upgrader is effectively a constant
var wsLogUpgrader = websocket.Upgrader{
	ReadBufferSize:  wsReadBufferSize,
	WriteBufferSize: wsWriteBufferSize,
	CheckOrigin:     sameOriginOnly,
}

// sameOriginOnly rejects cross-origin WebSocket handshakes. The
// UI always opens the socket from its own origin; anything else
// is either a misconfigured reverse proxy or a CSRF attempt.
// Matches the CSP connect-src 'self' policy.
//
// Uses net/url to compare so a browser that sends the default
// port explicitly (https://host:443, http://host:80) still
// matches the bare-host Host header. A literal string compare
// against "http://"+Host would reject those.
func sameOriginOnly(req *http.Request) bool {
	const (
		schemeHTTP  = "http"
		schemeHTTPS = "https"
	)

	origin := req.Header.Get("Origin")
	if origin == "" {
		// Non-browser client (curl with wscat). Safe to accept
		// because any real exploit still needs the session
		// cookie the browser sends only from the UI origin.
		return true
	}

	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}

	if parsed.Scheme != schemeHTTP && parsed.Scheme != schemeHTTPS {
		return false
	}

	originHost := parsed.Host
	// Strip the default port so https://host:443 matches host.
	if (parsed.Scheme == schemeHTTP && strings.HasSuffix(originHost, ":80")) ||
		(parsed.Scheme == schemeHTTPS && strings.HasSuffix(originHost, ":443")) {
		originHost = parsed.Hostname()
	}

	return originHost == req.Host
}

// WSLogHandler streams pod logs from the apiserver to the
// browser over WebSocket. Auth and RBAC are inherited from the
// caller's session — the log read uses a user-credentialed
// client-go request, same as the paginated Logs tab.
type WSLogHandler struct {
	logs     *k8s.LogService
	audit    audit.Logger
	authMode string
	log      *slog.Logger
}

// NewWSLogHandler wires the handler. The LogService is expected
// to be the same one the page handler uses for TailLogs so RBAC
// stays consistent across the two log-viewing flows. auditLog
// receives a pod.log.view event per successful stream so the
// audit pipeline sees every pod log read, matching the
// secret/connection-view coverage on the rest of the handler
// surface.
func NewWSLogHandler(
	logs *k8s.LogService, auditLog audit.Logger, authMode string, log *slog.Logger,
) *WSLogHandler {
	if auditLog == nil {
		auditLog = audit.NopLogger{}
	}

	return &WSLogHandler{logs: logs, audit: auditLog, authMode: authMode, log: log}
}

// Stream serves GET /api/logs/stream?tenant=X&pod=Y&container=Z&tail=N.
// The client MUST include a valid session cookie (RequireAuth
// at the router level enforces it); path-level validation
// runs here.
//
// ?tail= caps the initial backfill the browser receives so the
// client-side reconnect loop can request tail=0 after the first
// connect and avoid re-injecting the same history on every
// abnormal-close retry.
func (wlh *WSLogHandler) Stream(writer http.ResponseWriter, req *http.Request) {
	usr := auth.UserFromContext(req.Context())
	if usr == nil {
		Error(writer, http.StatusUnauthorized, "not authenticated")

		return
	}

	tenant := req.URL.Query().Get("tenant")
	pod := req.URL.Query().Get("pod")
	container := req.URL.Query().Get("container") // optional
	tailLines := parseTailParam(req.URL.Query().Get("tail"))

	if tenant == "" || pod == "" {
		// Authenticated user asked for logs but the request was
		// malformed. Audit the denial so operators grepping
		// outcome=denied see the probe; matches the
		// SecretReveal / ConnectionView precedent.
		wlh.recordAudit(req.Context(), usr, tenant, pod, container, tailLines,
			audit.OutcomeDenied, "tenant and pod parameters required")
		Error(writer, http.StatusBadRequest, "tenant and pod parameters required")

		return
	}

	conn, err := wsLogUpgrader.Upgrade(writer, req, nil)
	if err != nil {
		wlh.log.Info("websocket upgrade failed",
			"tenant", tenant, "pod", pod, "error", err)

		return
	}

	defer conn.Close()

	// Read deadline + pong handler: a dead peer (router drop,
	// unplug) goes undetected until the kernel's TCP keepalive
	// fires, typically ~2 h. A refreshed read deadline bumped
	// forward on every pong caps that at 2×wsPingInterval.
	_ = conn.SetReadDeadline(time.Now().Add(2 * wsPingInterval))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(2 * wsPingInterval))
	})

	outcome, detail := wlh.pump(req.Context(), conn, usr, tenant, pod, container, tailLines)
	wlh.recordAudit(req.Context(), usr, tenant, pod, container, tailLines, outcome, detail)
}

// recordAudit emits the pod.log.view event with the outcome
// observed by pump or by the 400 bad-request guard. Split from
// Stream so the malformed-param branch can record a denial
// without threading the same fields through the early return.
// The 401 path does not audit because usr is nil there; the
// middleware logs the anonymous attempt separately.
func (wlh *WSLogHandler) recordAudit(
	ctx context.Context, usr *auth.UserContext,
	tenant, pod, container string, tailLines int64,
	outcome audit.Outcome, detail string,
) {
	details := map[string]any{
		"container": container,
		"tail":      tailLines,
	}
	if detail != "" {
		details["error"] = detail
	}

	wlh.audit.Record(ctx, &audit.Event{
		RequestID: audit.RequestIDFromContext(ctx),
		Actor:     usr.Username,
		Groups:    usr.Groups,
		Action:    audit.ActionPodLogView,
		Resource:  pod,
		Tenant:    tenant,
		Outcome:   outcome,
		AuthMode:  wlh.authMode,
		Details:   details,
	})
}

// parseTailParam clamps the caller's ?tail= value to [0,
// logTailMax]. Empty or invalid strings fall back to the
// default backfill so a client that forgets to send the param
// still sees the last 500 lines. Negative or overflowing
// numbers are treated as "default" rather than silently flipping
// to the cap — a caller asking for -1 or 999999 is buggy and
// should not engineer a slow-path on the apiserver.
func parseTailParam(raw string) int64 {
	if raw == "" {
		return logTailDefault
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return logTailDefault
	}

	if value > logTailMax {
		return logTailMax
	}

	return value
}

// pump opens the pod log stream, forwards bytes to the
// WebSocket, and sends periodic pings to keep intermediate
// proxies from closing the connection. Returns when ctx fires,
// the log stream ends, or the peer disconnects.
//
// Returns (outcome, detail) so the caller can emit a matching
// audit event:
//   - OutcomeSuccess when StreamLogs opened and the stream
//     terminated normally (EOF / ctx / peer close);
//   - OutcomeDenied  when the apiserver refused (403/401) —
//     the user asked but was not allowed;
//   - OutcomeError   on any other failure (pod missing, apiserver
//     unreachable, transport error). detail carries the upstream
//     message for the audit trail.
//
// Naming the returns would read cleanest but the project's
// nonamedreturns lint forbids it; the three lines of comment
// above are the contract in place of named returns.
//
//nolint:gocritic // unnamedResult: nonamedreturns forbids the alternative
func (wlh *WSLogHandler) pump(
	ctx context.Context, conn *websocket.Conn,
	usr *auth.UserContext, tenant, pod, container string, tailLines int64,
) (audit.Outcome, string) {
	streamCtx, cancel := context.WithDeadline(ctx, time.Now().Add(logTailStreamMaxAge))
	defer cancel()

	stream, err := wlh.logs.StreamLogs(streamCtx, usr, tenant, pod, container, tailLines)
	if err != nil {
		wlh.log.Warn("opening pod log stream for websocket",
			"tenant", tenant, "pod", pod, "container", container, "error", err)

		closeErr := conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "failed to open log stream"),
			time.Now().Add(wsWriteDeadline))
		if closeErr != nil {
			wlh.log.Debug("writing ws close frame", "error", closeErr)
		}

		return classifyStreamError(err), err.Error()
	}

	defer stream.Close()

	go watchPeerDisconnect(conn, cancel)

	wlh.forwardBytes(streamCtx, conn, stream)

	return audit.OutcomeSuccess, ""
}

// classifyStreamError maps a StreamLogs error to an audit
// Outcome. The apiserver's "forbidden" text pattern is stable
// across kubectl versions; we match on the rendered wrap's
// body rather than unwrapping a typed error because client-go's
// REST error comes through fmt.Errorf already.
func classifyStreamError(err error) audit.Outcome {
	msg := err.Error()
	if strings.Contains(msg, "forbidden") || strings.Contains(msg, "Unauthorized") {
		return audit.OutcomeDenied
	}

	return audit.OutcomeError
}

// watchPeerDisconnect exits as soon as the peer side of conn
// closes and cancels the pump context so the log goroutine
// tears down without waiting for the next Read.
func watchPeerDisconnect(conn *websocket.Conn, cancel context.CancelFunc) {
	defer cancel()

	for {
		_, _, readErr := conn.NextReader()
		if readErr != nil {
			return
		}
	}
}

// forwardBytes pipes the log stream into the WebSocket. Pings
// run on a ticker parallel to the reader so a quiet pod does
// not let the connection idle-timeout inside a load balancer.
// Cognitive complexity stays manageable by keeping the reader
// goroutine and the select loop in separate functions.
func (wlh *WSLogHandler) forwardBytes(
	ctx context.Context, conn *websocket.Conn, stream io.Reader,
) {
	reader := bufio.NewReaderSize(stream, logReadChunk)
	bytesCh := make(chan []byte, forwardChanDepth)
	errCh := make(chan error, 1)

	go readChunks(ctx, reader, bytesCh, errCh)

	wlh.writeLoop(ctx, conn, bytesCh, errCh)
}

// readChunks pulls raw bytes off the log stream and hands them
// to bytesCh. On EOF or context cancel it closes bytesCh so the
// writer loop terminates; on a real error it reports via errCh
// first.
//
// Critical: the channel send is wrapped in a select on ctx.Done
// so a writeLoop that has already exited cannot deadlock the
// reader. Without this, a fast pod that queues N chunks while
// the writer is tearing down would block readChunks on the
// (N+1)-th send forever, leaking the goroutine even though
// stream.Close() was called.
func readChunks(ctx context.Context, reader io.Reader, bytesCh chan<- []byte, errCh chan<- error) {
	defer close(bytesCh)

	buf := make([]byte, logReadChunk)

	for {
		n, readErr := reader.Read(buf)
		if n > 0 && !sendChunkCtx(ctx, bytesCh, buf[:n]) {
			return
		}

		if readErr != nil {
			reportReadErr(ctx, errCh, readErr)

			return
		}
	}
}

// sendChunkCtx copies raw into a fresh buffer and forwards it on
// bytesCh, bailing if ctx cancels before the send lands. Returns
// false when the caller should stop (ctx dead) and true on a
// successful hand-off.
func sendChunkCtx(ctx context.Context, bytesCh chan<- []byte, raw []byte) bool {
	chunk := make([]byte, len(raw))
	copy(chunk, raw)

	select {
	case bytesCh <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

// reportReadErr forwards a real read error on errCh unless ctx
// already fired (reader is tearing down — no point waking the
// writer on its way out). EOF and ctx errors are filtered out
// upstream already.
func reportReadErr(ctx context.Context, errCh chan<- error, err error) {
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return
	}

	select {
	case errCh <- err:
	case <-ctx.Done():
	}
}

// writeLoop multiplexes outbound writes: periodic pings, log
// chunks from the reader goroutine, and an early exit on
// context cancel or read-side error.
func (wlh *WSLogHandler) writeLoop(
	ctx context.Context, conn *websocket.Conn,
	bytesCh <-chan []byte, errCh <-chan error,
) {
	pinger := time.NewTicker(wsPingInterval)
	defer pinger.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pinger.C:
			pingErr := conn.WriteControl(websocket.PingMessage, nil,
				time.Now().Add(wsWriteDeadline))
			if pingErr != nil {
				return
			}
		case chunk, ok := <-bytesCh:
			if !ok {
				return
			}

			if !wlh.sendChunk(conn, chunk) {
				return
			}
		case readErr := <-errCh:
			wlh.log.Warn("reading pod log stream", "error", readErr)

			return
		}
	}
}

// sendChunk writes one log chunk to the WebSocket with a
// bounded deadline. Returns false when the write fails so the
// caller tears down the stream.
func (wlh *WSLogHandler) sendChunk(conn *websocket.Conn, chunk []byte) bool {
	deadlineErr := conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
	if deadlineErr != nil {
		return false
	}

	// BinaryMessage (not TextMessage) because pod stdout can
	// legitimately carry invalid UTF-8 (crash dumps, binary
	// protobuf log lines). TextMessage asserts UTF-8 per
	// RFC 6455 §5.6; strict clients or inspecting proxies
	// would drop a mis-typed frame. The TS client sets
	// binaryType="arraybuffer" and decodes via TextDecoder
	// with fatal:false so replacement characters are rendered
	// instead of throwing.
	writeErr := conn.WriteMessage(websocket.BinaryMessage, chunk)

	return writeErr == nil
}
