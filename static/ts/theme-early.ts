// theme-early.ts is a standalone, non-deferred entry point that
// runs in the document <head> BEFORE the stylesheet loads. Its
// only job is to read the persisted theme choice from localStorage
// and stamp data-theme on the <html> element so the user's chosen
// palette is active by the time the first paint hits the screen.
// Without this, a user who picked light mode would see a flash of
// the dark theme on every page load while the deferred main bundle
// caught up — a small UX paper cut that a11y folks rightly flag.
//
// Shares the STORAGE_KEY constant semantics with theme.ts but is
// deliberately a separate entry point — main.ts is defer-loaded
// and ships roughly 65 KB of code, too heavy to block the head.

const KEY = "cozytempl-theme";

try {
  const raw = localStorage.getItem(KEY);
  if (raw === "dark" || raw === "light") {
    document.documentElement.setAttribute("data-theme", raw);
  }
} catch {
  // localStorage throws in some privacy contexts. Fall back to
  // whatever prefers-color-scheme says; the stylesheet handles
  // that case via a media query.
}
