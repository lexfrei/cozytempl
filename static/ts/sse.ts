// SSE subscription for real-time app status badge updates, row inserts, and removals.
// Parses tenant namespace from URL (/tenants/:ns/...) and subscribes to
// /api/events?tenant=:ns. Server pushes JSON messages with type=added|status|removed.

interface SSEMessage {
  type: "added" | "status" | "removed";
  name: string;
  status?: string;
  html?: string;
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

function getFilterValue(selector: string): string {
  const el = document.querySelector<HTMLInputElement | HTMLSelectElement>(selector);
  return el?.value ?? "";
}

// hasActiveAppFilter reports whether any filter/sort input on the tenant page
// has a non-default value. When true, naive append would show rows that should
// be hidden, so we must refetch the fragment with the current params instead.
function hasActiveAppFilter(): boolean {
  const query = getFilterValue('input[name="q"]');
  const kind = getFilterValue('select[name="kind"]');
  const sort = getFilterValue('select[name="sort"]');
  return query !== "" || kind !== "" || (sort !== "" && sort !== "name");
}

// refreshAppTable re-fetches the app-table fragment with current filter params
// and swaps it via htmx. Keeps the visible list consistent with server state.
function refreshAppTable(tenant: string): void {
  if (!tenant) return;

  const params = new URLSearchParams({
    tenant,
    q: getFilterValue('input[name="q"]'),
    kind: getFilterValue('select[name="kind"]'),
    sort: getFilterValue('select[name="sort"]'),
  });

  const htmxGlobal = (window as unknown as { htmx?: { ajax: (method: string, url: string, target: string) => void } }).htmx;
  if (htmxGlobal) {
    htmxGlobal.ajax("GET", `/fragments/app-table?${params.toString()}`, "#app-table-body");
  }
}

function appendRow(name: string, html: string): void {
  if (!html) return;
  // If a row for this app already exists, don't duplicate it.
  if (document.getElementById(`status-${name}`)) return;

  const tbody = document.getElementById("app-table-body");
  if (!tbody) return;

  // If filters/sort are active, the naive append would put rows in the wrong
  // position or show rows that should be hidden. Refetch instead.
  if (hasActiveAppFilter()) {
    refreshAppTable(currentTenant);

    return;
  }

  const template = document.createElement("template");
  template.innerHTML = html.trim();
  const fragment = template.content;
  if (fragment.childNodes.length === 0) return;

  tbody.appendChild(fragment);

  // Flash the newly inserted row once.
  const inserted = document.getElementById(`status-${name}`)?.closest("tr");
  if (inserted) {
    inserted.classList.add("row-flash");
    window.setTimeout(() => inserted.classList.remove("row-flash"), 1600);
  }
}

function removeRow(name: string): void {
  const badge = document.getElementById(`status-${name}`);
  if (!badge) return;
  const row = badge.closest("tr");
  if (row) row.remove();
}

function handleMessage(raw: string): void {
  let msg: SSEMessage;
  try {
    msg = JSON.parse(raw) as SSEMessage;
  } catch {
    return;
  }

  if (!msg.name) return;

  switch (msg.type) {
    case "added":
      if (msg.html) {
        appendRow(msg.name, msg.html);
      }
      if (msg.status) {
        applyStatus(msg.name, msg.status);
      }
      return;
    case "status":
      if (msg.status) {
        applyStatus(msg.name, msg.status);
      }
      return;
    case "removed":
      removeRow(msg.name);
      return;
    default:
      return;
  }
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
    handleMessage(event.data);
  };

  source.onerror = (): void => {
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

  document.addEventListener("htmx:afterSwap", (e) => {
    const detail = (e as CustomEvent).detail;
    if (detail.target?.id === "main-content") {
      syncFromLocation();
    }
  });

  window.addEventListener("popstate", () => {
    syncFromLocation();
  });
}
