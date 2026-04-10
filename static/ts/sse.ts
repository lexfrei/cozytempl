// SSE subscription for real-time app status badge updates.
// Parses tenant namespace from URL (/tenants/:ns/...) and subscribes to
// /api/events?tenant=:ns. Server pushes {name, data} events; we update the
// matching #status-{name} badge in place.

interface StatusEventPayload {
  type: string;
  name: string;
  data: string;
}

const RECONNECT_DELAY_MS = 5000;

let currentSource: EventSource | null = null;
let currentTenant = "";
let reconnectTimer: number | null = null;

function tenantFromPath(pathname: string): string {
  const match = pathname.match(/^\/tenants\/([^/]+)/);
  return match ? (match[1] ?? "") : "";
}

function badgeClass(status: string): string {
  const lower = status.toLowerCase();
  if (lower === "ready" || lower === "active") return "badge badge-ready";
  if (lower === "reconciling") return "badge badge-reconciling";
  if (lower === "failed") return "badge badge-failed";
  return "badge badge-unknown";
}

function applyStatus(name: string, status: string): void {
  const el = document.getElementById(`status-${name}`);
  if (!el) return;
  el.className = badgeClass(status);
  el.textContent = status;
}

function disconnect(): void {
  if (currentSource) {
    currentSource.close();
    currentSource = null;
  }
  if (reconnectTimer !== null) {
    window.clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
}

function connect(tenant: string): void {
  disconnect();
  if (!tenant) {
    currentTenant = "";
    return;
  }

  currentTenant = tenant;
  const source = new EventSource(`/api/events?tenant=${encodeURIComponent(tenant)}`);

  source.onmessage = (event: MessageEvent<string>): void => {
    try {
      const payload = JSON.parse(event.data) as StatusEventPayload;
      if (payload.name && typeof payload.data === "string") {
        applyStatus(payload.name, payload.data);
      }
    } catch {
      // Ignore malformed events
    }
  };

  source.onerror = (): void => {
    // Browser auto-reconnect is aggressive; fall back to manual delay on hard failure.
    if (source.readyState === EventSource.CLOSED) {
      disconnect();
      reconnectTimer = window.setTimeout(() => {
        if (currentTenant === tenant) connect(tenant);
      }, RECONNECT_DELAY_MS);
    }
  };

  currentSource = source;
}

function syncFromLocation(): void {
  const tenant = tenantFromPath(window.location.pathname);
  if (tenant !== currentTenant) {
    connect(tenant);
  }
}

export function initSSE(): void {
  syncFromLocation();

  // Reconnect on htmx SPA navigation (hx-push-url updates history).
  document.addEventListener("htmx:afterSwap", (e) => {
    const detail = (e as CustomEvent).detail;
    if (detail.target?.id === "main-content") {
      syncFromLocation();
    }
  });

  // Reconnect on browser back/forward.
  window.addEventListener("popstate", () => {
    syncFromLocation();
  });
}
