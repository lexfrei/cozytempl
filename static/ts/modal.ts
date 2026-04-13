// stripCreateKindFromURL removes the createKind query param from the
// browser's URL bar without reloading. The param is consumed once on
// page load to auto-open the create-app modal; once the modal is
// dismissed the URL should reflect the actual page state so refresh /
// share links no longer reopen the form.
export function stripCreateKindFromURL(): void {
  if (typeof window === "undefined" || !window.location.search.includes("createKind")) {
    return;
  }
  const url = new URL(window.location.href);
  if (!url.searchParams.has("createKind")) return;
  url.searchParams.delete("createKind");
  window.history.replaceState(null, "", url.pathname + (url.search ? url.search : "") + url.hash);
}

export function initModals(): void {
  // Close modal on backdrop click
  document.addEventListener("click", (e) => {
    const target = e.target as HTMLElement;
    if (target.classList.contains("modal-backdrop")) {
      target.style.display = "none";
      if (target.id === "create-app-modal") stripCreateKindFromURL();
    }
  });

  // Close modal on Escape
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      document.querySelectorAll<HTMLElement>(".modal-backdrop").forEach((el) => {
        el.style.display = "none";
        if (el.id === "create-app-modal") stripCreateKindFromURL();
      });
    }
  });
}

export function openModal(id: string): void {
  const el = document.getElementById(id);
  if (el) el.style.display = "flex";
}

export function closeModal(id: string): void {
  const el = document.getElementById(id);
  if (el) el.style.display = "none";
  if (id === "create-app-modal") stripCreateKindFromURL();
}
