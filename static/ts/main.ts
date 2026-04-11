import { initToasts } from "./toast";
import { initModals, openModal, closeModal } from "./modal";
import { initClipboard } from "./clipboard";
import { initSSE } from "./sse";
import { initHtmxFeedback } from "./htmx";

declare global {
  interface Window {
    __cozytemplInitialized: boolean;
  }
}
window.__cozytemplInitialized = false;

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
      default:
        return;
    }
  });
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
  const sort = document.querySelector<HTMLSelectElement>('select[name="sort"]');

  if (q) q.value = "";
  if (kind) kind.value = "";
  if (sort) sort.value = "name";

  q?.dispatchEvent(new Event("keyup", { bubbles: true }));
  kind?.dispatchEvent(new Event("change", { bubbles: true }));
}

// Forms inside a modal should reset after a successful submission so that
// reopening the modal presents a blank form instead of stale values. The
// server signals success via HX-Redirect, which htmx handles BEFORE the
// form is reset — we listen on htmx:afterRequest for any 2xx response on
// a form that lives under .modal-backdrop and clear it. Error responses
// are left alone so the user can fix the input and retry.
function initFormReset(): void {
  document.body.addEventListener("htmx:afterRequest", (evt) => {
    const detail = (evt as CustomEvent).detail ?? {};
    const xhr = detail.xhr as XMLHttpRequest | undefined;
    if (!xhr || xhr.status < 200 || xhr.status >= 300) return;

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

function initAll(): void {
  initHtmxFeedback();
  initActionDelegation();
  initNamespacePreview();
  initFormReset();
  initBurger();
  initToasts();
  initModals();
  initClipboard();
  initSSE();
  window.__cozytemplInitialized = true;
}

// `defer` scripts run after parsing but can still race DCL; handle both cases.
if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", initAll);
} else {
  initAll();
}
