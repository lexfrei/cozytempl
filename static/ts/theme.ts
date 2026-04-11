// Theme toggle + early application.
//
// This module runs two tricks:
//
//   1. applyStoredTheme() — reads localStorage["cozytempl-theme"]
//      and sets <html data-theme="...">. Must run BEFORE first paint
//      to avoid a flash of the wrong theme. The layout/base.templ
//      loads a small non-deferred <script> that calls this, so the
//      import at the top of main.ts is effectively a no-op on
//      subsequent runs — safe idempotent.
//
//   2. initThemeToggle() — wires the toggle button in the header
//      to cycle dark → light → system, persists the choice, and
//      fires the applyStoredTheme pathway again so the change
//      takes effect without a reload.
//
// The theme values that can live in storage:
//   - "dark" — explicit dark
//   - "light" — explicit light
//   - anything else (or missing) → follow prefers-color-scheme

const STORAGE_KEY = "cozytempl-theme";

type Theme = "dark" | "light" | "system";

function readStoredTheme(): Theme {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw === "dark" || raw === "light") return raw;
  } catch {
    // localStorage throws in some privacy contexts; fall through.
  }
  return "system";
}

function writeStoredTheme(theme: Theme): void {
  try {
    if (theme === "system") {
      localStorage.removeItem(STORAGE_KEY);
    } else {
      localStorage.setItem(STORAGE_KEY, theme);
    }
  } catch {
    // Ignore write failures — the in-memory data-theme still changes.
  }
}

export function applyStoredTheme(): void {
  const theme = readStoredTheme();
  const root = document.documentElement;
  if (theme === "system") {
    root.removeAttribute("data-theme");
  } else {
    root.setAttribute("data-theme", theme);
  }
}

function nextTheme(current: Theme): Theme {
  // Cycle: dark → light → system → dark ...
  if (current === "dark") return "light";
  if (current === "light") return "system";
  return "dark";
}

function labelForTheme(theme: Theme): string {
  if (theme === "dark") return "Switch to light theme";
  if (theme === "light") return "Switch to system theme";
  return "Switch to dark theme";
}

function iconForTheme(theme: Theme): string {
  // Unicode icons — no extra assets, CSP-safe. The theme the button
  // shows is the CURRENT one; clicking cycles forward.
  if (theme === "dark") return "\u263D"; // crescent moon
  if (theme === "light") return "\u2600"; // sun
  return "\u25D1"; // circle half-filled = system / auto
}

function updateToggle(btn: HTMLElement, theme: Theme): void {
  btn.textContent = iconForTheme(theme);
  btn.setAttribute("aria-label", labelForTheme(theme));
  btn.setAttribute("title", labelForTheme(theme));
}

export function initThemeToggle(): void {
  // Apply the current preference once more in case the early
  // inline script wasn't loaded (e.g. a test harness that skips
  // it). applyStoredTheme is idempotent.
  applyStoredTheme();

  const btn = document.getElementById("theme-toggle");
  if (!btn) return;

  updateToggle(btn, readStoredTheme());

  btn.addEventListener("click", () => {
    const current = readStoredTheme();
    const next = nextTheme(current);
    writeStoredTheme(next);
    applyStoredTheme();
    updateToggle(btn, next);
  });
}
