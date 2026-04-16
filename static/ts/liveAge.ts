// LiveAge ticker.
//
// The server emits <time data-age-start="RFC3339"
// data-server-now="RFC3339"> elements for Kubernetes-style
// age columns. This module re-reads the data attribute on a
// 1-second interval and rewrites the element's text so a row
// that appeared "5s" ago becomes "6s", "7s", …, "1m", without
// any server round-trip. The wire-up is document-wide and
// idempotent: new rows arriving via htmx or SSE are picked up
// on the next tick because the selector is re-evaluated every
// iteration.
//
// Single-unit output matches kubectl: "2m", "5h", "3d". The
// humaniser lives here in TypeScript (server also has its own
// Go copy in internal/view/partial/live_age.templ) — the
// first-paint value is server-rendered so a user on a slow
// JS-gate doesn't stare at a raw timestamp, and the ticker
// takes over on the first interval firing. Both humanisers
// must stay in sync; the Go side pins the boundaries via
// TestHumanizeAgeBoundaries and the TS side via its own
// sibling test (liveAge.test.ts).
//
// Two real-world concerns handled:
//
//  1. Clock skew. A user whose laptop drifted by 40s would
//     see the column jump on the first tick if the ticker
//     computed deltas against the browser clock alone. On
//     init we read data-server-now from the first visible
//     live-age element and compute a fixed offset; every
//     tick uses `Date.now() - offset` as the reference
//     "now", so the first tick agrees with the first paint.
//
//  2. Idle tabs. A user who opens the dashboard, locks their
//     screen for 8 hours, and comes back should not have
//     paid for 28,800 DOM walks. The ticker early-returns
//     when document.hidden is true; visibilitychange
//     re-runs one tick on resume so the column catches up
//     to the current time instead of jumping second-by-
//     second back to present.

const TICK_INTERVAL_MS = 1000;
const SELECTOR = "[data-age-start]";
const SERVER_NOW_SELECTOR = "[data-server-now]";

let tickerHandle: ReturnType<typeof setInterval> | null = null;

// serverClockOffsetMs is `serverNow - clientNow` at init
// time, in milliseconds. Positive means the server is ahead
// of the client; the ticker subtracts this from Date.now() to
// produce a "server-wall-clock now" that agrees with what the
// server used for the first-paint values. Recomputed on every
// visibilitychange resume because long sleeps can drift the
// client clock (e.g. phones / laptops across time zones).
let serverClockOffsetMs = 0;

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

// refreshServerClockOffset re-reads the first live-age
// element's data-server-now and sets serverClockOffsetMs.
// Called on init and on visibilitychange resume so a laptop
// that was asleep across a timezone change does not carry a
// stale offset forever.
function refreshServerClockOffset(): void {
  const anchor = document.querySelector<HTMLElement>(SERVER_NOW_SELECTOR);
  if (!anchor) {
    serverClockOffsetMs = 0;

    return;
  }

  const raw = anchor.dataset.serverNow;
  if (!raw) {
    serverClockOffsetMs = 0;

    return;
  }

  const serverMs = Date.parse(raw);
  if (Number.isNaN(serverMs)) {
    serverClockOffsetMs = 0;

    return;
  }

  serverClockOffsetMs = serverMs - Date.now();
}

// titleForTimestamp formats the absolute timestamp into the
// USER's locale and timezone. Server-side the Go renderer has
// no way to know either: it runs in whatever container
// timezone (usually UTC) and whatever locale the OS was built
// with. Formatting client-side on init means a user in Berlin
// sees "15.01.2026, 10:30:00 MEZ" instead of the server's
// "Jan 15, 2026, 10:30:00 AM UTC".
function titleForTimestamp(raw: string): string {
  const ms = Date.parse(raw);
  if (Number.isNaN(ms)) return raw;

  try {
    return new Date(ms).toLocaleString(undefined, {
      year: "numeric",
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      timeZoneName: "short",
    });
  } catch {
    // Browsers that choke on unusual options fall back to the
    // default locale string.
    return new Date(ms).toLocaleString();
  }
}

// populateTitles walks every live-age element and sets the
// title= attribute if it isn't set yet. Idempotent so it can
// run on init AND on each tick without double-setting.
function populateTitles(): void {
  const elements = document.querySelectorAll<HTMLElement>(SELECTOR);

  elements.forEach((el) => {
    if (el.title) return;

    const raw = el.dataset.ageStart;
    if (!raw) return;

    el.title = titleForTimestamp(raw);
  });
}

function tick(clientNow: number): void {
  if (typeof document !== "undefined" && document.hidden) {
    return;
  }

  const referenceNow = clientNow + serverClockOffsetMs;
  const elements = document.querySelectorAll<HTMLElement>(SELECTOR);

  elements.forEach((el) => {
    const raw = el.dataset.ageStart;
    if (!raw) return;

    const started = Date.parse(raw);
    if (Number.isNaN(started)) return;

    const next = humanizeAge(referenceNow - started);
    // Only touch the DOM if the text actually changed.
    // Rewriting identical text on every tick churns layout
    // engines on tables with hundreds of rows.
    if (el.textContent !== next) {
      el.textContent = next;
    }

    // New elements showing up via htmx swap may not have had
    // populateTitles run on them yet; fill the tooltip now.
    if (!el.title) {
      el.title = titleForTimestamp(raw);
    }
  });
}

function handleVisibilityChange(): void {
  if (document.hidden) return;

  // Returning from hidden: clocks may have drifted during
  // sleep (suspended laptops, cellular handoffs). Re-read
  // the server-now offset and run one catch-up tick so the
  // column jumps straight to the right value instead of
  // marching second-by-second.
  refreshServerClockOffset();
  tick(Date.now());
}

export function initLiveAge(): void {
  if (tickerHandle !== null) return;

  refreshServerClockOffset();
  populateTitles();

  // Paint once immediately so a row added between ticks does
  // not show its stale server-rendered value for up to a
  // second. Subsequent ticks run on a plain setInterval.
  tick(Date.now());

  tickerHandle = setInterval(() => {
    tick(Date.now());
  }, TICK_INTERVAL_MS);

  document.addEventListener("visibilitychange", handleVisibilityChange);
}
