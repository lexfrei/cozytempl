import { initToasts } from "./toast";
import { initModals, openModal, closeModal } from "./modal";
import { initClipboard } from "./clipboard";
import { initSSE } from "./sse";
import { initHtmxFeedback } from "./htmx";

// Expose modal functions globally for onclick handlers in templ
declare global {
  interface Window {
    openModal: (id: string) => void;
    closeModal: (id: string) => void;
    clearAppFilters: () => void;
    __cozytemplInitialized: boolean;
  }
}
window.openModal = openModal;
window.closeModal = closeModal;
window.clearAppFilters = clearAppFilters;
window.__cozytemplInitialized = false;

// clearAppFilters resets the q/kind/sort inputs on the tenant detail
// page and re-issues the fragment fetch via htmx. Called by the Clear
// button inlined in tenant.templ — server-side conditional visibility
// is not an option because filter changes go through fragment swaps
// and don't re-render the button.
function clearAppFilters(): void {
  const q = document.querySelector<HTMLInputElement>('input[name="q"]');
  const kind = document.querySelector<HTMLSelectElement>('select[name="kind"]');
  const sort = document.querySelector<HTMLSelectElement>('select[name="sort"]');

  if (q) q.value = "";
  if (kind) kind.value = "";
  if (sort) sort.value = "name";

  // Trigger the htmx refetch by firing the same event the inputs
  // would fire themselves. Use keyup on q so the "keyup changed"
  // trigger fires (with its 300ms delay smoothed out by empty value
  // == previous value no-op).
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
