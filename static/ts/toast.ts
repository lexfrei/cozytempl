const TOAST_DISMISS_MS = 4000;

export function initToasts(): void {
  document.addEventListener("htmx:afterSwap", () => {
    document.querySelectorAll<HTMLElement>(".toast[data-auto-dismiss]").forEach((el) => {
      setTimeout(() => {
        el.style.opacity = "0";
        el.style.transform = "translateX(20px)";
        setTimeout(() => el.remove(), 200);
      }, TOAST_DISMISS_MS);
    });
  });
}

export function showToast(type: "success" | "error", message: string): void {
  const container = document.getElementById("toast-container");
  if (!container) return;

  const toast = document.createElement("div");
  toast.className = `toast toast-${type}`;
  toast.setAttribute("data-auto-dismiss", "");
  toast.textContent = message;
  toast.style.transition = "opacity 200ms, transform 200ms";
  container.appendChild(toast);

  setTimeout(() => {
    toast.style.opacity = "0";
    toast.style.transform = "translateX(20px)";
    setTimeout(() => toast.remove(), 200);
  }, TOAST_DISMISS_MS);
}
