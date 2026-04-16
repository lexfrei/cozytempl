// htmx is imported from the vendored npm package and bundled into
// bundle.js by esbuild. We intentionally do NOT load it from unpkg
// or any other CDN — the old `<script src="unpkg.com/htmx.org@2/...">`
// tag was a supply-chain RCE vector (no SRI, unpinned patch version,
// third-party origin in script-src). Bundling locks us to a reviewed
// version and ties integrity to our own asset-version cache buster.
import htmx from "htmx.org";

import { initToasts } from "./toast";
import { initModals, openModal, closeModal } from "./modal";
import { initClipboard } from "./clipboard";
import { initSSE } from "./sse";
import { initHtmxFeedback } from "./htmx";
import { initReveal } from "./reveal";
import { initThemeToggle } from "./theme";
import { initCommandPalette } from "./palette";
import { initOverviewFilter } from "./overview";
import { initWatchSSE } from "./watch";

declare global {
  interface Window {
    __cozytemplInitialized: boolean;
    htmx: typeof htmx;
  }
}
window.__cozytemplInitialized = false;
// htmx needs to be reachable from `window.htmx` for attributes like
// hx-on::*, inline scripts in extensions, and our SSE glue. The ESM
// export gives us the object back; we expose it explicitly instead of
// relying on the IIFE's side-effects.
window.htmx = htmx;

// initActionDelegation wires a single document-level click listener
// that routes every data-action button to the right handler. Switching
// from inline onclick attributes to delegation is required because
// the CSP applied in api/router.go does NOT include 'unsafe-inline'
// in script-src, which means inline event-handler attributes are
// rejected by the browser. Delegation is also tidier: one place to
// add a new action, no window globals.
function initActionDelegation(): void {
  document.addEventListener("click", (evt) => {
    const target = (evt.target as HTMLElement | null)?.closest<HTMLElement>("[data-action]");
    if (!target) return;

    const action = target.dataset.action;
    const modalId = target.dataset.modal;

    switch (action) {
      case "modal-open":
        if (modalId) openModal(modalId);
        return;
      case "modal-close":
        if (modalId) closeModal(modalId);
        return;
      case "modal-dismiss":
        // Dismiss = remove the modal element entirely. Used by the
        // dynamically-inserted edit modals (tenant-edit, app-edit)
        // whose slot containers are reused — removing the node
        // guarantees a fresh fetch next time.
        if (modalId) document.getElementById(modalId)?.remove();
        return;
      case "clear-filters":
        clearAppFilters();
        return;
      case "set-sort":
        updateSortInput(target.dataset.sort ?? "name");
        return;
      case "tenant-navigate":
        // Whole-row navigation on the tenants table. Guarded by the
        // "original target inside button/link" check so the Edit /
        // Delete buttons and the name cell's anchor keep their
        // primary click semantics — this handler only fires on cells
        // with no other click target.
        handleTenantNavigate(evt, target);
        return;
      default:
        return;
    }
  });
}

// handleTenantNavigate runs the whole-row click on the tenants list.
// Suppresses itself when the real click target was a button or link
// so the action cells and the name anchor keep working. Otherwise
// uses htmx.ajax to swap #main-content and pushes history so browser
// back / reload land on the tenant page.
function handleTenantNavigate(evt: Event, row: HTMLElement): void {
  const clicked = evt.target as HTMLElement | null;
  if (clicked?.closest("button, a")) return;

  const href = row.dataset.href;
  if (!href) return;

  const htmxAPI = (window as unknown as { htmx?: { ajax: (method: string, url: string, opts: Record<string, unknown>) => void } }).htmx;
  if (htmxAPI) {
    htmxAPI.ajax("GET", href, { target: "#main-content", swap: "innerHTML" });
    window.history.pushState({}, "", href);

    return;
  }

  // Fallback for the rare case htmx isn't loaded yet — full nav still
  // reaches the destination, just without the SPA-style swap.
  window.location.href = href;
}

// updateSortInput writes the chosen sort key into the hidden
// <input name="sort"> on the tenant detail page. The click that
// triggered the call is already wired to an htmx-fetch on the
// column header <button>, so this runs purely to keep the q/kind
// inputs' hx-include in sync for any subsequent refetch.
function updateSortInput(value: string): void {
  const sort = document.querySelector<HTMLInputElement>('input[name="sort"]');
  if (sort) sort.value = value;
}

// initNamespacePreview wires the create-tenant form's namespace hint.
// The input carries data-ns-preview="<targetId>" and the listener
// updates targetId's textContent with "tenant-<value>" as the user
// types. Single handler bound at document level so dynamically
// re-rendered forms don't need re-binding.
function initNamespacePreview(): void {
  document.addEventListener("input", (evt) => {
    const input = evt.target as HTMLInputElement | null;
    if (!input || !input.dataset.nsPreview) return;

    const target = document.getElementById(input.dataset.nsPreview);
    if (!target) return;

    target.textContent = input.value ? "tenant-" + input.value : "tenant-...";
  });
}

// clearAppFilters resets q/kind/sort on the tenant detail page and
// re-issues the fragment fetch via htmx. Called by the Clear button
// through the action delegator. Server-side conditional visibility
// is not an option because filter changes go through fragment swaps
// and don't re-render the button.
function clearAppFilters(): void {
  const q = document.querySelector<HTMLInputElement>('input[name="q"]');
  const kind = document.querySelector<HTMLSelectElement>('select[name="kind"]');
  // sort is now a hidden input driven by column-header clicks.
  const sort = document.querySelector<HTMLInputElement>('input[name="sort"]');

  if (q) q.value = "";
  if (kind) kind.value = "";
  if (sort) sort.value = "name";

  q?.dispatchEvent(new Event("keyup", { bubbles: true }));
  kind?.dispatchEvent(new Event("change", { bubbles: true }));
}

// Forms inside a modal should reset after a successful CREATE / UPDATE so
// that reopening the modal presents a blank form instead of stale values.
// The trigger is narrow on purpose:
//   * only POST / PUT / DELETE (mutations) — a kind-select change in the
//     create form fires a GET /fragments/schema-fields and must NOT reset
//     the form because that would also wipe the newly-chosen kind
//   * only 2xx responses — error responses keep the input so the user can
//     fix and retry
//   * only when the triggering element is itself inside a .modal-backdrop
//     form — unrelated htmx calls elsewhere on the page must not touch it
function initFormReset(): void {
  document.body.addEventListener("htmx:afterRequest", (evt) => {
    const detail = (evt as CustomEvent).detail ?? {};
    const xhr = detail.xhr as XMLHttpRequest | undefined;
    if (!xhr || xhr.status < 200 || xhr.status >= 300) return;

    // Only treat mutations as "finished, clear the form".
    const verb: string | undefined = detail.requestConfig?.verb;
    if (verb !== "post" && verb !== "put" && verb !== "delete") return;

    const elt = detail.elt as HTMLElement | undefined;
    if (!elt) return;

    const form = elt.closest?.("form");
    if (!form) return;
    if (!form.closest?.(".modal-backdrop")) return;

    form.reset();
  });
}

// Burger menu toggle
function initBurger(): void {
  document.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>(".burger");
    if (!btn) return;

    const sidebar = document.querySelector<HTMLElement>(".sidebar");
    const overlay = document.querySelector<HTMLElement>(".sidebar-overlay");
    if (sidebar) sidebar.classList.toggle("open");
    if (overlay) overlay.classList.toggle("open");
  });

  // Close sidebar on overlay click
  document.addEventListener("click", (e) => {
    const target = e.target as HTMLElement;
    if (target.classList.contains("sidebar-overlay")) {
      document.querySelector(".sidebar")?.classList.remove("open");
      target.classList.remove("open");
    }
  });

  // Close sidebar on navigation (htmx swap)
  document.addEventListener("htmx:afterSwap", (e) => {
    const detail = (e as CustomEvent).detail;
    if (detail.target?.id === "main-content") {
      document.querySelector(".sidebar")?.classList.remove("open");
      document.querySelector(".sidebar-overlay")?.classList.remove("open");
    }
  });
}

// initSubmitOnChange wires controls tagged with
// data-action="submit-on-change" so that a change event on the
// control submits its enclosing <form>. The language switcher in
// internal/view/partial/header.templ uses this so the <select>
// can trigger a full-page POST to /lang without an inline
// onchange handler — inline handlers are blocked by the project
// CSP (script-src 'self', no 'unsafe-inline').
function initSubmitOnChange(): void {
  document.addEventListener("change", (evt) => {
    const target = evt.target as HTMLElement | null;
    if (!target || target.dataset.action !== "submit-on-change") return;

    const form = (target as HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement).form;
    if (form) form.submit();
  });
}

function initAll(): void {
  initThemeToggle();
  initHtmxFeedback();
  initActionDelegation();
  initNamespacePreview();
  initFormReset();
  initBurger();
  initToasts();
  initModals();
  initClipboard();
  initSSE();
  initReveal();
  initCommandPalette();
  initOverviewFilter();
  initWatchSSE();
  initSubmitOnChange();
  window.__cozytemplInitialized = true;
}

// `defer` scripts run after parsing but can still race DCL; handle both cases.
if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", initAll);
} else {
  initAll();
}
