// Global htmx request feedback: top progress bar + body cursor.
//
// A single DOM element (#htmx-progress-bar) is driven by htmx's request
// lifecycle events. On beforeRequest we add .loading, which triggers a long
// CSS transition toward 85%; on afterRequest (or error) we switch to .done,
// which snaps to 100% and fades out. Concurrent requests reuse the same bar,
// the counter prevents finishing early if one request completes while
// another is still in flight.
//
// The triggering element also gets htmx's built-in .htmx-request class, which
// is styled in styles.css (spinner overlay on buttons, ring on inputs).

const PROGRESS_DONE_RESET_MS = 420;

let inflight = 0;
let resetTimer: number | null = null;

function bar(): HTMLElement | null {
  return document.getElementById("htmx-progress-bar");
}

function beginProgress(): void {
  const el = bar();
  if (!el) return;

  inflight += 1;

  // One-shot transition from 0 to loading. Reset first if a previous "done"
  // state is still on the element.
  if (resetTimer !== null) {
    window.clearTimeout(resetTimer);
    resetTimer = null;
  }
  el.classList.remove("done");
  // Force reflow so the next class addition re-triggers the transition.
  void el.offsetWidth;
  el.classList.add("loading");
}

function endProgress(): void {
  const el = bar();
  if (!el) return;

  inflight = Math.max(0, inflight - 1);
  if (inflight > 0) return;

  el.classList.remove("loading");
  el.classList.add("done");

  resetTimer = window.setTimeout(() => {
    el.classList.remove("done");
    el.style.transform = "scaleX(0)";
    // Clear inline style on next tick so CSS takes over again.
    window.setTimeout(() => {
      el.style.removeProperty("transform");
    }, 0);
    resetTimer = null;
  }, PROGRESS_DONE_RESET_MS);
}

// htmx fires requests for every hx-* interaction; ignore SSE traffic.
function isSSE(evt: Event): boolean {
  const detail = (evt as CustomEvent).detail ?? {};
  const path: string | undefined = detail.pathInfo?.requestPath || detail.requestConfig?.path;
  return typeof path === "string" && path.startsWith("/api/events");
}

export function initHtmxFeedback(): void {
  document.body.addEventListener("htmx:beforeRequest", (evt) => {
    if (isSSE(evt)) return;
    beginProgress();
  });

  document.body.addEventListener("htmx:afterRequest", (evt) => {
    if (isSSE(evt)) return;
    endProgress();
  });

  // Network / abort / timeout all still need to release the bar.
  document.body.addEventListener("htmx:sendError", (evt) => {
    if (isSSE(evt)) return;
    endProgress();
  });
  document.body.addEventListener("htmx:responseError", (evt) => {
    if (isSSE(evt)) return;
    endProgress();
  });
  document.body.addEventListener("htmx:timeout", (evt) => {
    if (isSSE(evt)) return;
    endProgress();
  });
}
