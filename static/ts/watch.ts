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
  object: string;
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
// optional data-watch-object attribute scopes the filter to a
// specific owner (an app name, in the Events-tab case) — when
// present the server drops events that do not belong to that
// owner, preventing neighbour-app leakage into the tbody.
function findSubscriptionTarget(): { resource: string; tenant: string; object: string } | null {
  const target = document.querySelector<HTMLElement>(
    "[data-watch-resource][data-watch-tenant]",
  );
  if (!target) return null;

  const resource = target.dataset.watchResource ?? "";
  const tenant = target.dataset.watchTenant ?? "";
  if (!resource || !tenant) return null;

  const object = target.dataset.watchObject ?? "";

  return { resource, tenant, object };
}

function connect(resource: string, tenant: string, object: string): void {
  disconnect();

  let url = `/api/watch/${encodeURIComponent(resource)}?tenant=${encodeURIComponent(tenant)}`;
  if (object) url += `&object=${encodeURIComponent(object)}`;

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
      if (
        nextTarget &&
        nextTarget.resource === resource &&
        nextTarget.tenant === tenant &&
        nextTarget.object === object
      ) {
        connect(resource, tenant, object);
      }
    }, RECONNECT_DELAY_MS);
  };

  current = { source, resource, tenant, object };
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
    refreshEmptyState();
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
  // lives) so new events appear in reverse-chronological order. The
  // empty-state div (rendered alongside the tbody when the initial
  // list was empty) gets hidden on first arrival so the "no events
  // yet" copy stops competing with the live row for attention.
  const container = document.querySelector<HTMLElement>(
    "[data-watch-resource]",
  );
  if (container) {
    container.prepend(nextRow);
    flashRow(nextRow);
    refreshEmptyState();
  }
}

// refreshEmptyState toggles the "no events yet" copy based on
// whether the watch container has any live rows. Called after
// every add (to hide the state) and every remove (to restore it
// when the last live row drops). Without the remove-side call the
// user would stare at an empty tbody with no explanation after
// Kubernetes GC sweeps the last Event entry.
function refreshEmptyState(): void {
  const container = document.querySelector<HTMLElement>(
    "[data-watch-resource]",
  );
  const emptyState = document.getElementById("event-empty-state");
  if (!container || !emptyState) return;

  emptyState.hidden = container.children.length > 0;
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
    current.tenant === target.tenant &&
    current.object === target.object
  ) {
    return;
  }

  connect(target.resource, target.tenant, target.object);
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
