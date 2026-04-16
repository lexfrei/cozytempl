// Generic watch-proxy SSE client.
//
// Subscribes to /api/watch/{resource} on pages that opt in via a
// `data-watch-resource` attribute on the target tbody (or other
// container). The server opens a user-credentialed watch against
// the Kubernetes API and pushes one message per event; the client
// reducer finds the row by `{rowIdPrefix}-{name}` and performs an
// upsert/remove in place.
//
// One subscription per page — the connection tears down and rebuilds
// on every htmx main-content swap (same shape as sse.ts) so a user
// navigating between apps doesn't accumulate stale EventSources.

type WatchOp = "added" | "modified" | "removed";

interface WatchMessage {
  op: WatchOp;
  name: string;
  rowIdPrefix: string;
  html?: string;
}

interface WatchSubscription {
  source: EventSource;
  resource: string;
  tenant: string;
}

const RECONNECT_DELAY_MS = 5000;
const ROW_FLASH_MS = 1600;

let current: WatchSubscription | null = null;
let reconnectTimer: number | null = null;

function disconnect(): void {
  if (current) {
    current.source.close();
    current = null;
  }
  if (reconnectTimer !== null) {
    window.clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
}

// findSubscriptionTarget looks for an element tagged with
// data-watch-resource and data-watch-tenant attributes. These mark
// the container whose rows the server will push updates into. The
// template-rendered events tbody on the app detail page is the
// canonical consumer; adding more pages is a one-line templ edit
// plus a watchResourceConfig entry on the server.
function findSubscriptionTarget(): { resource: string; tenant: string } | null {
  const target = document.querySelector<HTMLElement>(
    "[data-watch-resource][data-watch-tenant]",
  );
  if (!target) return null;

  const resource = target.dataset.watchResource ?? "";
  const tenant = target.dataset.watchTenant ?? "";
  if (!resource || !tenant) return null;

  return { resource, tenant };
}

function connect(resource: string, tenant: string): void {
  disconnect();

  const url =
    `/api/watch/${encodeURIComponent(resource)}?tenant=${encodeURIComponent(tenant)}`;
  const source = new EventSource(url);

  source.onmessage = (event: MessageEvent<string>): void => {
    handleMessage(event.data);
  };

  source.onerror = (): void => {
    if (source.readyState !== EventSource.CLOSED) return;

    // CLOSED means the server hung up — bounce with a delay so a
    // flapping apiserver doesn't storm us with reconnects.
    disconnect();
    reconnectTimer = window.setTimeout(() => {
      const nextTarget = findSubscriptionTarget();
      if (nextTarget && nextTarget.resource === resource && nextTarget.tenant === tenant) {
        connect(resource, tenant);
      }
    }, RECONNECT_DELAY_MS);
  };

  current = { source, resource, tenant };
}

function handleMessage(raw: string): void {
  let msg: WatchMessage;
  try {
    msg = JSON.parse(raw) as WatchMessage;
  } catch {
    return;
  }

  if (!msg.name || !msg.rowIdPrefix || !msg.op) return;

  const rowId = `${msg.rowIdPrefix}-${msg.name}`;

  if (msg.op === "removed") {
    document.getElementById(rowId)?.remove();
    return;
  }

  if (!msg.html) return;

  const template = document.createElement("template");
  template.innerHTML = msg.html.trim();
  const nextRow = template.content.firstElementChild;
  if (!(nextRow instanceof HTMLElement)) return;

  const existing = document.getElementById(rowId);
  if (existing) {
    existing.replaceWith(nextRow);
    flashRow(nextRow);
    return;
  }

  // Insert at the top of the tbody (or wherever data-watch-resource
  // lives) so new events appear in reverse-chronological order.
  const container = document.querySelector<HTMLElement>(
    "[data-watch-resource]",
  );
  if (container) {
    container.prepend(nextRow);
    flashRow(nextRow);
  }
}

function flashRow(row: HTMLElement): void {
  row.classList.add("row-flash");
  window.setTimeout(() => {
    row.classList.remove("row-flash");
  }, ROW_FLASH_MS);
}

function syncFromDOM(): void {
  const target = findSubscriptionTarget();
  if (!target) {
    disconnect();
    return;
  }

  if (
    current &&
    current.resource === target.resource &&
    current.tenant === target.tenant
  ) {
    return;
  }

  connect(target.resource, target.tenant);
}

export function initWatchSSE(): void {
  syncFromDOM();

  document.addEventListener("htmx:afterSwap", (e) => {
    const detail = (e as CustomEvent).detail;
    if (detail.target?.id === "main-content") {
      syncFromDOM();
    }
  });

  window.addEventListener("popstate", () => {
    syncFromDOM();
  });
}
