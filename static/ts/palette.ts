// Command palette: a Cmd/Ctrl-K overlay that lists the top-level
// actions in the app — navigation, theme toggle, a couple of mode
// shortcuts — and lets the user filter by substring, pick with
// arrow keys, and activate with Enter. Modeled on the VSCode /
// Linear / Raycast pattern.
//
// Everything renders client-side from a static action catalog. No
// templ, no server fetch. The palette is one DOM node created on
// demand and reused across openings so the document root stays
// clean when the palette is idle.
//
// The navigation actions use htmx.ajax() to swap #main-content
// instead of a full page reload, matching every other link in the
// app. A full reload would be a regression against the SPA-feel
// the rest of the UI provides.

import htmx from "htmx.org";

type PaletteAction = {
  id: string;
  label: string;
  hint?: string;
  // Run when the user picks this action. The palette closes itself
  // before calling handler, so handlers can open modals or do
  // document.body mutations without fighting the overlay.
  handler: () => void;
};

// Catalog is built lazily so we see the current DOM when the
// palette opens — specifically so the "On this tenant" actions can
// reference the tenant namespace parsed out of the URL.
function buildCatalog(): PaletteAction[] {
  const actions: PaletteAction[] = [
    {
      id: "nav.dashboard",
      label: "Go to Dashboard",
      hint: "/",
      handler: () => navigate("/"),
    },
    {
      id: "nav.tenants",
      label: "Go to Tenants",
      hint: "/tenants",
      handler: () => navigate("/tenants"),
    },
    {
      id: "nav.marketplace",
      label: "Go to Marketplace",
      hint: "/marketplace",
      handler: () => navigate("/marketplace"),
    },
    {
      id: "nav.profile",
      label: "Go to Profile",
      hint: "/profile",
      handler: () => navigate("/profile"),
    },
    {
      id: "app.toggle-theme",
      label: "Toggle light / dark theme",
      hint: "Header button",
      handler: (): void => {
        const btn = document.getElementById("theme-toggle");
        btn?.click();
      },
    },
    {
      id: "app.create-tenant",
      label: "Create a new tenant",
      hint: "Opens the create-tenant modal",
      handler: () => openModalById("create-tenant-modal"),
    },
  ];

  const tenant = currentTenantNamespace();
  if (tenant) {
    actions.push({
      id: "tenant.view",
      label: `Open tenant ${tenant}`,
      hint: `/tenants/${tenant}`,
      handler: () => navigate(`/tenants/${tenant}`),
    });
    actions.push({
      id: "tenant.create-app",
      label: `Create application in ${tenant}`,
      hint: "Opens the create-app modal",
      handler: () => openModalById("create-app-modal"),
    });
  }

  return actions;
}

// currentTenantNamespace parses /tenants/<ns> or
// /tenants/<ns>/apps/... out of location.pathname. Returns an
// empty string when the current page is not tenant-scoped so
// callers can skip the tenant-specific entries.
function currentTenantNamespace(): string {
  const match = /^\/tenants\/([^/]+)/.exec(window.location.pathname);
  return match ? match[1] : "";
}

// navigate uses htmx.ajax so a palette-driven navigation produces
// the same partial swap as clicking a sidebar link. history.pushState
// is handled by htmx because we pass pushUrl: true.
function navigate(path: string): void {
  void htmx.ajax("GET", path, {
    target: "#main-content",
    swap: "innerHTML",
    ...({ pushUrl: true } as Record<string, unknown>),
  });
}

// openModalById fires the existing data-action="modal-open" pipeline
// instead of reaching into modal.ts directly. Keeps a single source
// of truth for modal lifecycle (focus trap, backdrop, scroll lock).
function openModalById(id: string): void {
  const trigger = document.querySelector<HTMLElement>(`[data-action="modal-open"][data-modal="${id}"]`);
  if (trigger) {
    trigger.click();
    return;
  }

  // Fall back to the modal element directly if no trigger button is
  // currently in the DOM (e.g. the Clear button on the app filters
  // row is the only create-tenant trigger on the dashboard).
  const modal = document.getElementById(id);
  if (modal) modal.classList.add("is-open");
}

// Singleton state: one overlay element, one action list, one
// active-index pointer. Rebuilt on every open() so the list always
// matches the current DOM state.
let overlay: HTMLDivElement | null = null;
let searchInput: HTMLInputElement | null = null;
let listEl: HTMLUListElement | null = null;
let items: PaletteAction[] = [];
let filtered: PaletteAction[] = [];
let activeIndex = 0;

function ensureOverlay(): HTMLDivElement {
  if (overlay) return overlay;

  const root = document.createElement("div");
  root.className = "command-palette";
  root.setAttribute("role", "dialog");
  root.setAttribute("aria-modal", "true");
  root.setAttribute("aria-label", "Command palette");
  root.hidden = true;

  root.innerHTML = `
    <div class="command-palette-backdrop" data-palette-close="true"></div>
    <div class="command-palette-panel">
      <input type="text"
             class="command-palette-input"
             placeholder="Type a command..."
             autocomplete="off"
             spellcheck="false"
             aria-label="Filter commands" />
      <ul class="command-palette-list" role="listbox"></ul>
      <div class="command-palette-footer">
        <span><kbd>↑↓</kbd> navigate</span>
        <span><kbd>⏎</kbd> run</span>
        <span><kbd>esc</kbd> close</span>
      </div>
    </div>
  `;

  document.body.appendChild(root);

  overlay = root;
  searchInput = root.querySelector<HTMLInputElement>(".command-palette-input");
  listEl = root.querySelector<HTMLUListElement>(".command-palette-list");

  root.addEventListener("click", (evt) => {
    const target = evt.target as HTMLElement | null;
    if (target?.dataset.paletteClose === "true") close();
  });

  searchInput?.addEventListener("input", () => {
    updateFiltered();
    render();
  });

  searchInput?.addEventListener("keydown", (evt) => {
    switch (evt.key) {
      case "ArrowDown":
        evt.preventDefault();
        moveActive(1);
        break;
      case "ArrowUp":
        evt.preventDefault();
        moveActive(-1);
        break;
      case "Enter":
        evt.preventDefault();
        runActive();
        break;
      case "Escape":
        evt.preventDefault();
        close();
        break;
      default:
        break;
    }
  });

  return root;
}

function open(): void {
  const root = ensureOverlay();
  items = buildCatalog();
  activeIndex = 0;
  if (searchInput) searchInput.value = "";
  updateFiltered();
  render();
  root.hidden = false;
  document.body.classList.add("command-palette-open");
  // Focus runs on the next frame so the browser has a chance to
  // commit the unhidden overlay before we move focus into it.
  requestAnimationFrame(() => searchInput?.focus());
}

function close(): void {
  if (!overlay) return;
  overlay.hidden = true;
  document.body.classList.remove("command-palette-open");
}

function updateFiltered(): void {
  const query = searchInput?.value.trim().toLowerCase() ?? "";
  if (!query) {
    filtered = items.slice();
  } else {
    filtered = items.filter((item) => {
      return item.label.toLowerCase().includes(query) || (item.hint?.toLowerCase().includes(query) ?? false);
    });
  }
  if (activeIndex >= filtered.length) activeIndex = 0;
}

function render(): void {
  if (!listEl) return;

  if (filtered.length === 0) {
    listEl.innerHTML = `<li class="command-palette-empty">No matching commands.</li>`;
    return;
  }

  const html = filtered
    .map((item, idx) => {
      const isActive = idx === activeIndex;
      const cls = isActive ? "command-palette-item command-palette-item-active" : "command-palette-item";
      const hint = item.hint ? `<span class="command-palette-item-hint">${escapeHtml(item.hint)}</span>` : "";
      return `
        <li class="${cls}"
            role="option"
            data-palette-index="${idx}"
            aria-selected="${isActive}">
          <span class="command-palette-item-label">${escapeHtml(item.label)}</span>
          ${hint}
        </li>
      `;
    })
    .join("");

  listEl.innerHTML = html;

  // Click-to-run on any row. Bound once per render — the list is
  // short (under a dozen items) so replacing innerHTML and re-binding
  // is cheaper than delegating and tracking drag state.
  listEl.querySelectorAll<HTMLLIElement>(".command-palette-item").forEach((li) => {
    li.addEventListener("mouseenter", () => {
      const idx = Number(li.dataset.paletteIndex ?? "0");
      activeIndex = idx;
      updateActiveDom();
    });
    li.addEventListener("click", () => {
      const idx = Number(li.dataset.paletteIndex ?? "0");
      activeIndex = idx;
      runActive();
    });
  });
}

function updateActiveDom(): void {
  if (!listEl) return;
  listEl.querySelectorAll<HTMLLIElement>(".command-palette-item").forEach((li, idx) => {
    const isActive = idx === activeIndex;
    li.classList.toggle("command-palette-item-active", isActive);
    li.setAttribute("aria-selected", String(isActive));
  });
}

function moveActive(delta: number): void {
  if (filtered.length === 0) return;
  activeIndex = (activeIndex + delta + filtered.length) % filtered.length;
  updateActiveDom();
  // Keep the active row in view when the list is long enough to scroll.
  listEl?.querySelector<HTMLLIElement>(`.command-palette-item[data-palette-index="${activeIndex}"]`)?.scrollIntoView({
    block: "nearest",
  });
}

function runActive(): void {
  const action = filtered[activeIndex];
  if (!action) return;
  close();
  action.handler();
}

function escapeHtml(raw: string): string {
  const div = document.createElement("div");
  div.textContent = raw;
  return div.innerHTML;
}

// shouldIntercept blocks the Cmd/Ctrl-K shortcut when the user is
// typing in a real input so keystrokes intended for a form don't
// vanish into the palette. contenteditable is included because
// some templ-rendered spec editors use it.
function shouldIntercept(evt: KeyboardEvent): boolean {
  // Even inside an input the explicit shortcut should still work —
  // the user might be mid-search and want to jump somewhere else.
  // Only block the "/" shortcut when already inside a text field.
  const target = evt.target as HTMLElement | null;
  if (!target) return true;
  const tag = target.tagName.toLowerCase();
  const isTextField = tag === "input" || tag === "textarea" || target.isContentEditable;
  if ((evt.metaKey || evt.ctrlKey) && evt.key.toLowerCase() === "k") {
    return true;
  }

  if (evt.key === "/" && !isTextField) {
    return true;
  }

  return false;
}

export function initCommandPalette(): void {
  document.addEventListener("keydown", (evt) => {
    if (!shouldIntercept(evt)) return;

    if ((evt.metaKey || evt.ctrlKey) && evt.key.toLowerCase() === "k") {
      evt.preventDefault();
      if (overlay && !overlay.hidden) {
        close();
      } else {
        open();
      }

      return;
    }

    if (evt.key === "/" && (overlay === null || overlay.hidden)) {
      evt.preventDefault();
      open();
    }
  });
}
