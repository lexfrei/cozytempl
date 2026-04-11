// Global htmx request feedback: top progress bar.
//
// A single DOM element (#htmx-progress-bar) is toggled via a `.loading`
// class that CSS maps to an indeterminate sliding-highlight animation.
// Concurrent requests reuse the same bar — the bar is only removed when
// the last in-flight request finishes. No fade-out animation: the bar
// collapses from height 3 to height 0 instantly, which keeps behavior
// robust across fast successive requests (no CSS transition state
// to unwind).
//
// The triggering element also gets htmx's built-in .htmx-request class,
// which is styled in styles.css (spinner overlay on buttons, ring on
// inputs).

let inflight = 0;

function bar(): HTMLElement | null {
  return document.getElementById("htmx-progress-bar");
}

function beginProgress(): void {
  const el = bar();
  if (!el) return;

  inflight += 1;
  if (inflight === 1) {
    el.classList.add("loading");
  }
}

function endProgress(): void {
  const el = bar();
  if (!el) return;

  inflight = Math.max(0, inflight - 1);
  if (inflight === 0) {
    el.classList.remove("loading");
  }
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
