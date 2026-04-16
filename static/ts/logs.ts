// Live pod log tail over WebSocket.
//
// Scoped to the Logs tab on the app detail page. Activates when
// an element tagged `data-log-stream` is present in #main-content
// (either on initial load or after an htmx swap that lands on
// ?tab=logs). The matching element carries `data-log-tenant`,
// `data-log-pod`, and `data-log-container`; this module assembles
// the /api/logs/stream?... URL from those three, opens a
// WebSocket, and appends every text frame to the element.
//
// Why WebSocket and not SSE: the shared-SA /api/events stream is
// SSE because HelmRelease events are small and fan out to many
// clients from one source. A pod log stream is per-user (user
// credentials), per-pod, and carries arbitrary bytes — WebSocket
// gives us ping/pong for proxy idle-timeout resilience without
// any protocol overhead for binary-safe chunks.

const RECONNECT_DELAY_MS = 3000;
const MAX_BUFFERED_BYTES = 1_000_000; // ~1 MB of scrollback before we drop lines.

interface LogTarget {
  container: string;
  tenant: string;
  pod: string;
  containerName: string;
}

let activeSocket: WebSocket | null = null;
let reconnectTimer: number | null = null;

// findTarget scans the document once for the log-tail <pre>. It
// returns null on every page that is not the Logs tab so the
// init/swap listeners can short-circuit.
function findTarget(): LogTarget | null {
  const el = document.querySelector<HTMLElement>("[data-log-stream]");
  if (!el) return null;

  const tenant = el.dataset.logTenant ?? "";
  const pod = el.dataset.logPod ?? "";
  const containerName = el.dataset.logContainer ?? "";

  if (!tenant || !pod) return null;

  return { container: el.id, tenant, pod, containerName };
}

// streamURL builds the absolute WebSocket URL for the caller's
// page origin. `ws:` / `wss:` mirrors the page scheme — CSP
// connect-src 'self' already covers that pairing so no further
// allowlist is needed.
function streamURL(target: LogTarget): string {
  const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
  const params = new URLSearchParams({
    tenant: target.tenant,
    pod: target.pod,
  });

  if (target.containerName) params.set("container", target.containerName);

  return `${scheme}//${window.location.host}/api/logs/stream?${params.toString()}`;
}

// attach opens the WebSocket, prepends a "connected" marker so
// the user sees the transition from the server-rendered tail to
// the live stream, and appends incoming text with auto-scroll.
// Reconnects once on abnormal close; a second failure surfaces
// an inline hint instead of flapping forever.
function attach(target: LogTarget): void {
  disconnect();

  const host = document.getElementById(target.container);
  if (!host) return;

  const socket = new WebSocket(streamURL(target));
  activeSocket = socket;

  socket.addEventListener("open", () => {
    appendLine(host, "\n--- live stream attached ---\n", "log-marker");
  });

  socket.addEventListener("message", (event: MessageEvent<string>) => {
    appendText(host, event.data);
  });

  socket.addEventListener("close", (event: CloseEvent) => {
    if (socket !== activeSocket) return;
    activeSocket = null;

    if (event.code === 1000 || event.code === 1001) {
      appendLine(host, "\n--- live stream closed ---\n", "log-marker");
      return;
    }

    appendLine(host, "\n--- live stream dropped, retrying ---\n", "log-marker-warn");
    scheduleReconnect(target);
  });

  socket.addEventListener("error", () => {
    // The close handler runs after error; defer user-visible
    // state changes to that branch so we don't double-annotate
    // the pre element.
  });
}

// appendText writes raw bytes from the server. Trims oldest
// content when the buffer exceeds MAX_BUFFERED_BYTES so a chatty
// pod cannot leak memory in a long-lived tab.
function appendText(host: HTMLElement, chunk: string): void {
  const node = document.createTextNode(chunk);
  host.appendChild(node);

  while (host.textContent && host.textContent.length > MAX_BUFFERED_BYTES) {
    const first = host.firstChild;
    if (!first) break;
    host.removeChild(first);
  }

  // Auto-scroll only when the user is already at the bottom,
  // otherwise they're reading scrollback and a jump feels hostile.
  const nearBottom =
    host.scrollHeight - host.scrollTop - host.clientHeight < 60;
  if (nearBottom) host.scrollTop = host.scrollHeight;
}

// appendLine is the "marker" path: a small coloured label for
// lifecycle events (connected, dropped). Rendered as a <span>
// with a class so the styles stay in one place.
function appendLine(host: HTMLElement, text: string, className: string): void {
  const span = document.createElement("span");
  span.className = className;
  span.textContent = text;
  host.appendChild(span);
  host.scrollTop = host.scrollHeight;
}

function scheduleReconnect(target: LogTarget): void {
  if (reconnectTimer !== null) return;

  reconnectTimer = window.setTimeout(() => {
    reconnectTimer = null;

    // Only reconnect when the user is still on the same Logs
    // tab. A swap away (tab change, navigation) clears the
    // target, and attach() becomes a no-op.
    const next = findTarget();
    if (next && next.pod === target.pod && next.tenant === target.tenant) {
      attach(next);
    }
  }, RECONNECT_DELAY_MS);
}

function disconnect(): void {
  if (activeSocket) {
    activeSocket.close(1000, "client navigating");
    activeSocket = null;
  }

  if (reconnectTimer !== null) {
    window.clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
}

function syncFromDOM(): void {
  const target = findTarget();
  if (target) {
    attach(target);
  } else {
    disconnect();
  }
}

export function initLogStream(): void {
  syncFromDOM();

  document.addEventListener("htmx:afterSwap", (evt) => {
    const detail = (evt as CustomEvent).detail;
    if (detail.target?.id === "main-content") {
      syncFromDOM();
    }
  });

  window.addEventListener("beforeunload", () => disconnect());
}
