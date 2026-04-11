// Click-to-reveal auto-hide.
//
// The connection tab on the app-detail page renders credentials as
// placeholder dots. A click on the reveal button fires an htmx GET
// that swaps the real value into a [data-reveal-target] span. This
// module listens for that swap and starts a timer so the credential
// goes back to dots after the window defined on the parent's
// data-reveal-timeout attribute (default 30 seconds).
//
// Without this timer the revealed value would stay in the DOM for
// the entire page lifetime, undoing the whole point of click-to-
// reveal. Audit logging on the server side records the reveal
// request, but the client is responsible for limiting the window
// of exposure.

const DEFAULT_REVEAL_WINDOW_MS = 30000;
const PLACEHOLDER = "\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022";

export function initReveal(): void {
  document.body.addEventListener("htmx:afterSwap", (evt) => {
    const detail = (evt as CustomEvent).detail ?? {};
    const target = detail.target as HTMLElement | undefined;
    if (!target) return;
    if (!target.matches("[data-reveal-target]")) return;

    const parent = target.closest<HTMLElement>(".connection-secret");
    if (!parent) return;

    const raw = parent.dataset.revealTimeout;
    const parsed = raw ? Number.parseInt(raw, 10) : NaN;
    const window = Number.isFinite(parsed) && parsed > 0 ? parsed : DEFAULT_REVEAL_WINDOW_MS;

    // A visible "active" class lets the CSS hint the user that the
    // value is temporarily on screen (subtle background change).
    parent.classList.add("connection-secret-revealed");

    window_setTimeout(() => {
      target.textContent = PLACEHOLDER;
      parent.classList.remove("connection-secret-revealed");
    }, window);
  });
}

// Thin wrapper around setTimeout that exists only because we named
// the timer-window variable "window" above and shadowed the global;
// isolating the call keeps the linter honest.
function window_setTimeout(fn: () => void, ms: number): number {
  return setTimeout(fn, ms);
}
