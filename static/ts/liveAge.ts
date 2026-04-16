// LiveAge ticker.
//
// The server emits <time data-age-start="RFC3339"> elements
// for Kubernetes-style age columns (app CreatedAt, event
// LastSeen, etc.). This module re-reads the data attribute
// on a 1-second interval and rewrites the element's text so
// a row that appeared "5s" ago becomes "6s", "7s", ..., "1m",
// without any server round-trip. The wire-up is intentionally
// document-wide and idempotent: new rows arriving via htmx or
// SSE are picked up on the next tick because the selector is
// re-evaluated every iteration.
//
// Single-unit output matches kubectl: "2m", "5h", "3d". The
// humaniser lives here in TypeScript (server also has its own
// Go copy in internal/view/partial/live_age.templ) — the
// first-paint value is server-rendered so a user on a slow
// JS-gate doesn't stare at a raw timestamp, and the ticker
// takes over on the first interval firing. Both humanisers
// must stay in sync; a shared test (Go) pins the boundary
// cases and the TS test (future) would pin the TS side.

const TICK_INTERVAL_MS = 1000;
const SELECTOR = "[data-age-start]";

let tickerHandle: ReturnType<typeof setInterval> | null = null;

// humanizeAge mirrors the Go side byte-for-byte: <1s → "0s",
// <60s → "Ns", <60m → "Nm", <24h → "Nh", <365d → "Nd", else
// "Ny". If the Go side flips a boundary this function must
// move too — the server renders the first-paint value and the
// client paints every tick after, and a divergence would make
// the column visibly jump on the first tick.
export function humanizeAge(deltaMs: number): string {
  if (deltaMs < 1000) {
    return "0s";
  }

  const seconds = Math.floor(deltaMs / 1000);
  if (seconds < 60) {
    return `${seconds}s`;
  }

  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) {
    return `${minutes}m`;
  }

  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    return `${hours}h`;
  }

  const days = Math.floor(hours / 24);
  if (days < 365) {
    return `${days}d`;
  }

  const years = Math.floor(days / 365);

  return `${years}y`;
}

function tick(now: number): void {
  const elements = document.querySelectorAll<HTMLElement>(SELECTOR);

  elements.forEach((el) => {
    const raw = el.dataset.ageStart;
    if (!raw) return;

    const started = Date.parse(raw);
    if (Number.isNaN(started)) return;

    const next = humanizeAge(now - started);
    // Only touch the DOM if the text actually changed.
    // Rewriting identical text on every tick churns layout
    // engines on tables with hundreds of rows.
    if (el.textContent !== next) {
      el.textContent = next;
    }
  });
}

export function initLiveAge(): void {
  if (tickerHandle !== null) return;

  // Paint once immediately so a row added between ticks does
  // not show its stale server-rendered value for up to a
  // second. Subsequent ticks run on a plain setInterval.
  tick(Date.now());

  tickerHandle = setInterval(() => {
    tick(Date.now());
  }, TICK_INTERVAL_MS);
}
