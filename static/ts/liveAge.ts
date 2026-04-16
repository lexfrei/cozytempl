// LiveAge ticker.
//
// The server emits <time data-age-start="RFC3339"> elements
// for Kubernetes-style age columns. A single
// data-server-now attribute on <body> (set by the base
// layout) carries the server's wall clock at render time
// and is the only clock-skew anchor the ticker reads. This
// module re-reads the per-element data-age-start on a
// 1-second interval and rewrites the element's text so a
// row that appeared "5s" ago becomes "6s", "7s", …, "1m",
// without any server round-trip. The wire-up is
// document-wide and idempotent: new rows arriving via htmx
// or SSE are picked up on the next tick because the
// selector is re-evaluated every iteration.
//
// Single-unit output matches kubectl: "2m", "5h", "3d". The
// humaniser lives here in TypeScript (server also has its
// own Go copy in internal/view/partial/live_age.templ) —
// the first-paint value is server-rendered so a user on a
// slow JS-gate doesn't stare at a raw timestamp, and the
// ticker takes over on the first interval firing. Both
// humanisers must stay in sync; the Go side pins the
// boundaries via TestHumanizeAgeBoundaries and the TS side
// via its own sibling test (liveAge.test.ts).
//
// Two real-world concerns handled:
//
//  1. Clock skew. A user whose laptop drifted by 40s would
//     see the column jump on the first tick if the ticker
//     computed deltas against the browser clock alone. On
//     init we read <body data-server-now> and compute a
//     fixed offset; every tick uses `Date.now() + offset`
//     as the reference "now", so the first tick agrees
//     with the first paint.
//
//     The offset is only refreshed on visibilitychange
//     resume — not on every tick, and not on htmx
//     #main-content swaps (which keep <body> in place).
//     On multi-hour sessions this means the offset is
//     frozen against the client clock at initial page
//     load. In practice NTP-synced clocks do not drift
//     meaningfully across hours, and the cost of
//     re-reading the marker on every tick (another
//     querySelector walk) outweighs the win. If a future
//     telemetry signal shows long-lived pages drifting,
//     the fix is to re-read on tick N%60 rather than
//     every tick.
//
//  2. Idle tabs. Modern browsers already throttle hidden-
//     tab setInterval callbacks to about once per minute,
//     so the raw firing rate is already modest. What the
//     ticker still wants to avoid is the DOM walk itself:
//     a tab open for a long time on a page with hundreds
//     of rows would wake querySelectorAll every tick even
//     while the user is not looking. document.hidden → no
//     tick; a visibilitychange listener re-runs a single
//     catch-up tick on resume so the column jumps straight
//     to the right value instead of marching second-by-
//     second from the last paint.

const TICK_INTERVAL_MS = 1000;
const SELECTOR = "[data-age-start]";

// The clock-skew anchor lives on <body>; a querySelector
// against this selector returns either the <body> or (if
// a future change moves the marker) whichever element
// carries it. Scoping it document-wide keeps the ticker
// agnostic to where the marker is rendered.
const SERVER_NOW_SELECTOR = "[data-server-now]";

let tickerHandle: ReturnType<typeof setInterval> | null = null;
let visibilityListenerAttached = false;

// serverClockOffsetMs is `serverNow - clientNow` at init
// time, in milliseconds. Positive means the server is ahead
// of the client; the ticker adds this to Date.now() to
// produce a "server-wall-clock now" that agrees with what
// the server used for the first-paint values. A missing or
// malformed server-now marker leaves the offset at its last
// known value — zeroing would re-introduce the skew jump
// this logic exists to prevent.
let serverClockOffsetMs = 0;

// humanizeAge mirrors the Go side byte-for-byte: <1s → "0s",
// <60s → "Ns", <60m → "Nm", <24h → "Nh", <365d → "Nd", else
// "Ny". If the Go side flips a boundary this function must
// move too — the server renders the first-paint value and
// the client paints every tick after, and a divergence
// would make the column visibly jump on the first tick.
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

// refreshServerClockOffset re-reads the first element
// carrying data-server-now (in production: <body>) and
// updates serverClockOffsetMs. If the anchor is missing or
// malformed the previous offset is preserved — an earlier
// revision zeroed out, which threw away a valid offset
// every time the anchor row disappeared (e.g. if the
// marker moved to a deletable table row in a refactor) and
// re-introduced the client-clock-jump this function exists
// to prevent.
function refreshServerClockOffset(): void {
  const anchor = document.querySelector<HTMLElement>(SERVER_NOW_SELECTOR);
  if (!anchor) return;

  const raw = anchor.dataset.serverNow;
  if (!raw) return;

  const serverMs = Date.parse(raw);
  if (Number.isNaN(serverMs)) return;

  serverClockOffsetMs = serverMs - Date.now();
}

// titleForTimestamp formats the absolute timestamp into the
// USER's locale and timezone. Server-side the Go renderer
// has no way to know either: it runs in whatever container
// timezone (usually UTC) and whatever locale the OS was
// built with. Formatting client-side on init means a user
// in Berlin sees "15.01.2026, 10:30:00 MEZ" instead of the
// server's "Jan 15, 2026, 10:30:00 AM UTC".
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

    // Populate the locale-aware tooltip lazily. The
    // attribute is server-omitted (server does not know the
    // user's timezone) so the first time the ticker sees an
    // element it fills title=; idempotent on later ticks.
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
  // marching second-by-second back to present.
  refreshServerClockOffset();
  tick(Date.now());
}

export function initLiveAge(): void {
  if (tickerHandle !== null) return;

  refreshServerClockOffset();

  // Paint once immediately so a row added between ticks
  // does not show its stale server-rendered value for up
  // to a second. Subsequent ticks run on a plain
  // setInterval. tick also handles the title=populate so
  // we do not need a separate walk.
  tick(Date.now());

  tickerHandle = setInterval(() => {
    tick(Date.now());
  }, TICK_INTERVAL_MS);

  if (!visibilityListenerAttached) {
    document.addEventListener("visibilitychange", handleVisibilityChange);
    visibilityListenerAttached = true;
  }
}

// stopLiveAge tears down the interval and visibility
// listener. Exposed for tests and for any future SPA-style
// navigation that re-initialises modules; not called from
// the current initAll() path.
export function stopLiveAge(): void {
  if (tickerHandle !== null) {
    clearInterval(tickerHandle);
    tickerHandle = null;
  }

  if (visibilityListenerAttached) {
    document.removeEventListener("visibilitychange", handleVisibilityChange);
    visibilityListenerAttached = false;
  }
}
