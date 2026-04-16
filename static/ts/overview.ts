// overview.ts wires the client-side filter on the /overview page.
//
// The server pre-groups apps by Kind and emits rows tagged with
// data-overview-name and data-overview-tenant. The filter listens
// on the single `<input data-overview-filter>` and toggles row
// visibility via the `hidden` attribute — a CSS display:none would
// also work, but `hidden` is a single toggle that every CSS rule
// already respects.
//
// Why client-side and not htmx-driven: each keystroke on htmx would
// re-fan-out the per-tenant List calls on the server (one request
// per tenant), which wastes apiserver budget and adds 300+ ms of
// round-trip before the user sees the filtered result. The row set
// is bounded (one tenant admin rarely sees >500 apps) so hiding
// rows locally is instant and cheap. The generic watch proxy
// (issue #4) will eventually push live row updates over SSE;
// that stays compatible with the hide/show filter approach.
export function initOverviewFilter(): void {
  document.addEventListener("input", (evt) => {
    const input = evt.target as HTMLInputElement | null;
    if (!input || input.dataset.overviewFilter === undefined) return;

    applyOverviewFilter(input.value);
  });

  // htmx swaps #main-content when the user lands on /overview via
  // sidebar nav. Re-apply the filter against whatever value the
  // server placed in the input so a filtered URL reload (e.g. from
  // browser back) keeps its filter active visually.
  document.addEventListener("htmx:afterSwap", (evt) => {
    const detail = (evt as CustomEvent).detail;
    if (detail.target?.id !== "main-content") return;

    const input = document.querySelector<HTMLInputElement>(
      "[data-overview-filter]",
    );
    if (input) applyOverviewFilter(input.value);
  });

  // Initial apply on first page load (input value might be
  // pre-populated by the server from ?q=).
  const initial = document.querySelector<HTMLInputElement>(
    "[data-overview-filter]",
  );
  if (initial && initial.value) applyOverviewFilter(initial.value);
}

function applyOverviewFilter(raw: string): void {
  const query = raw.trim().toLowerCase();

  let anyVisible = false;

  const groups = document.querySelectorAll<HTMLElement>(
    "[data-overview-group]",
  );

  groups.forEach((group) => {
    const rows = group.querySelectorAll<HTMLElement>("[data-overview-row]");
    let visibleInGroup = 0;

    rows.forEach((row) => {
      const match = rowMatches(row, query);
      row.hidden = !match;
      if (match) visibleInGroup++;
    });

    // Collapse the whole group card when every row inside is
    // filtered away — the empty table header with no body rows
    // reads as noise to a user scanning for matches.
    group.hidden = visibleInGroup === 0;
    if (visibleInGroup > 0) anyVisible = true;
  });

  const noResults = document.getElementById("overview-no-results");
  if (noResults) noResults.hidden = anyVisible || query === "";
}

function rowMatches(row: HTMLElement, query: string): boolean {
  if (query === "") return true;

  const name = (row.dataset.overviewName ?? "").toLowerCase();
  const tenant = (row.dataset.overviewTenant ?? "").toLowerCase();

  return name.includes(query) || tenant.includes(query);
}
