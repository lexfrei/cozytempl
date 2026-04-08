import { showToast } from "./toast";

export function initClipboard(): void {
  document.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>("[data-copy]");
    if (!btn) return;

    const text = btn.getAttribute("data-copy") ?? "";
    navigator.clipboard.writeText(text).then(
      () => showToast("success", "Copied to clipboard"),
      () => showToast("error", "Failed to copy"),
    );
  });
}
