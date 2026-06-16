// Shared client behavior for the library UI: theme toggle, view toggle, and
// client-side table sorting. Loaded with `defer`, so the DOM is ready. The tiny
// pre-paint theme-apply runs inline in each page's <head> to avoid a flash.

function toggleTheme() {
  const el = document.documentElement;
  const dark = el.getAttribute("data-theme") !== "dark";
  el.setAttribute("data-theme", dark ? "dark" : "light");
  localStorage.setItem("theme", dark ? "dark" : "light");
  syncThemeButton();
  // The reader overrides this to also repaint the book iframe.
  if (typeof window.onThemeChange === "function") window.onThemeChange();
}

function syncThemeButton() {
  const btn = document.getElementById("theme-btn");
  if (btn) btn.textContent =
    document.documentElement.getAttribute("data-theme") === "dark" ? "☀️" : "🌙";
}

// --- View mode (index): grid | table, persisted ---

function toggleView() {
  const b = document.body;
  const table = b.getAttribute("data-view") !== "table";
  b.setAttribute("data-view", table ? "table" : "grid");
  localStorage.setItem("view", table ? "table" : "grid");
  syncViewButton();
}

function syncViewButton() {
  const btn = document.getElementById("view-btn");
  if (btn) btn.textContent =
    document.body.getAttribute("data-view") === "table" ? "Grid" : "Table";
}

// --- Client-side column sort (index table) ---
// The whole library is on the page, so sorting is a DOM reorder, no server
// round-trip. Sort keys live in each row's data-* attributes.

const sortState = { key: null, dir: 1 };

function sortTable(key) {
  const tbody = document.querySelector("#books-table tbody");
  if (!tbody) return;
  sortState.dir = sortState.key === key ? -sortState.dir : 1;
  sortState.key = key;
  const numeric = key === "added";
  const rows = Array.from(tbody.querySelectorAll("tr"));
  rows.sort((a, b) => {
    let va = a.getAttribute("data-" + key) || "";
    let vb = b.getAttribute("data-" + key) || "";
    if (numeric) return (Number(va) - Number(vb)) * sortState.dir;
    return va.localeCompare(vb, undefined, { sensitivity: "base" }) * sortState.dir;
  });
  rows.forEach(r => tbody.appendChild(r));
  document.querySelectorAll("#books-table th").forEach(th => {
    const arrow = th.querySelector(".arrow");
    if (!arrow) return;
    arrow.textContent = th.getAttribute("data-key") === key
      ? (sortState.dir === 1 ? "▲" : "▼") : "";
  });
}

document.addEventListener("DOMContentLoaded", function () {
  syncThemeButton();
  // Restore the persisted view now that <body> exists.
  if (localStorage.getItem("view") === "table")
    document.body.setAttribute("data-view", "table");
  syncViewButton();
  document.querySelectorAll("#books-table th").forEach(th =>
    th.addEventListener("click", () => sortTable(th.getAttribute("data-key"))));
});
