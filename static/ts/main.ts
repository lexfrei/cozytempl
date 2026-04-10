import { initToasts } from "./toast";
import { initModals, openModal, closeModal } from "./modal";
import { initClipboard } from "./clipboard";
import { initSSE } from "./sse";

// Expose modal functions globally for onclick handlers in templ
declare global {
  interface Window {
    openModal: (id: string) => void;
    closeModal: (id: string) => void;
  }
}
window.openModal = openModal;
window.closeModal = closeModal;

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

// Init all modules
document.addEventListener("DOMContentLoaded", () => {
  initBurger();
  initToasts();
  initModals();
  initClipboard();
  initSSE();
});
