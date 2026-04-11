// SSE subscription for real-time app updates.
//
// One connection per tenant page. The server emits a unified
// "resource change" message with a discriminator (op: added | modified |
// removed). The client runs a single reducer that upserts or deletes a row
// keyed by its name, so create / update / delete go through the same code
// path. When a filter or sort is active the client refetches the fragment
// instead of blindly appending.

type ResourceOp = "added" | "modified" | "removed";

interface ResourceMessage {
  // Backwards-compatible field. Older servers may still send {type: ...}.
  type?: ResourceOp;
  op?: ResourceOp;
  name: string;
  status?: string;
  html?: string;
}

const RECONNECT_DELAY_MS = 5000;
const ROW_FLASH_MS = 1600;

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

function rowForName(name: string): HTMLTableRowElement | null {
  const byId = document.getElementById(`row-${name}`);
  if (byId instanceof HTMLTableRowElement) return byId;

  // Legacy fallback: older server renders only put an id on the status badge.
  const badge = document.getElementById(`status-${name}`);
  return badge?.closest("tr") ?? null;
}

function applyStatus(name: string, status: string): void {
  const badgeEl = document.getElementById(`status-${name}`);
  if (!badgeEl) return;
  badgeEl.className = badgeClass(status);
  badgeEl.textContent = status;
}

function getFilterValue(selector: string): string {
  const el = document.querySelector<HTMLInputElement | HTMLSelectElement>(selector);
  return el?.value ?? "";
}

// Any filter/sort input on the tenant page has a non-default value. When
// true, blindly inserting a row would show rows that ought to be hidden or
// place them out of sort order — we refetch the fragment instead.
function hasActiveAppFilter(): boolean {
  const query = getFilterValue('input[name="q"]');
  const kind = getFilterValue('select[name="kind"]');
  const sort = getFilterValue('select[name="sort"]');
  return query !== "" || kind !== "" || (sort !== "" && sort !== "name");
}

interface HtmxGlobal {
  ajax: (method: string, url: string, target: string) => void;
}

function refreshAppTable(tenant: string): void {
  if (!tenant) return;

  const params = new URLSearchParams({
    tenant,
    q: getFilterValue('input[name="q"]'),
    kind: getFilterValue('select[name="kind"]'),
    sort: getFilterValue('select[name="sort"]'),
  });

  const htmxGlobal = (window as unknown as { htmx?: HtmxGlobal }).htmx;
  if (htmxGlobal) {
    htmxGlobal.ajax(
      "GET",
      `/fragments/app-table?${params.toString()}`,
      "#app-table-body",
    );
  }
}

function flashRow(row: HTMLElement): void {
  row.classList.add("row-flash");
  window.setTimeout(() => row.classList.remove("row-flash"), ROW_FLASH_MS);
}

// upsertRow is a single path for "row should exist with this HTML".
// Called for added and modified events; the server sends the full row HTML.
function upsertRow(name: string, html: string): void {
  const tbody = document.getElementById("app-table-body");
  if (!tbody) return;

  const existing = rowForName(name);

  // If a filter/sort is active, any structural change (add or reorder) must
  // be reflected by refetching. Simple status updates still work because the
  // badge DOM node is replaced via applyStatus.
  if (!existing && hasActiveAppFilter()) {
    refreshAppTable(currentTenant);
    return;
  }

  if (!html) {
    return;
  }

  const template = document.createElement("template");
  template.innerHTML = html.trim();
  const fragment = template.content;
  if (fragment.childNodes.length === 0) return;

  const newRow = fragment.firstElementChild as HTMLElement | null;
  if (!newRow) return;

  if (existing) {
    existing.replaceWith(newRow);
  } else {
    tbody.appendChild(newRow);
    flashRow(newRow);
  }
}

function removeRow(name: string): void {
  const row = rowForName(name);
  if (row) row.remove();
}

// applyChange is the single reducer for every server-pushed update. Any
// future event shape (e.g. tenant changes, quota updates) should plug into
// this function rather than growing a parallel handler.
function applyChange(msg: ResourceMessage): void {
  if (!msg.name) return;

  const op = msg.op ?? msg.type;
  if (!op) return;

  switch (op) {
    case "added":
      if (msg.html) {
        upsertRow(msg.name, msg.html);
      }
      if (msg.status) {
        applyStatus(msg.name, msg.status);
      }
      return;
    case "modified":
      if (msg.html) {
        upsertRow(msg.name, msg.html);
      }
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

function handleMessage(raw: string): void {
  let msg: ResourceMessage;
  try {
    msg = JSON.parse(raw) as ResourceMessage;
  } catch {
    return;
  }

  applyChange(msg);
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
