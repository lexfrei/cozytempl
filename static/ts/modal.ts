export function initModals(): void {
  // Close modal on backdrop click
  document.addEventListener("click", (e) => {
    const target = e.target as HTMLElement;
    if (target.classList.contains("modal-backdrop")) {
      target.style.display = "none";
    }
  });

  // Close modal on Escape
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      document.querySelectorAll<HTMLElement>(".modal-backdrop").forEach((el) => {
        el.style.display = "none";
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
}
