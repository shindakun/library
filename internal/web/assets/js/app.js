// Shared client behavior for the library UI: theme picker, view toggle, and
// client-side table sorting. Loaded with `defer`, so the DOM is ready. The tiny
// pre-paint theme-apply runs inline in each page's <head> to avoid a flash.

// THEMES is the source of truth for the picker. To add a theme, add an entry
// here AND a matching :root[data-theme="id"] block in app.css. icon is shown on the
// picker button when that theme is active.
var THEMES = [
  { id: "dark", label: "Dark", icon: "🌙" },   // 🌙
  { id: "light", label: "Light", icon: "☀️" }, // ☀️
  { id: "warm", label: "Warm", icon: "🕯️" }, // 🕯️ candle
];

// setTheme applies a theme by id, persists it, updates the button, and notifies
// the reader (which repaints the book iframe). Passing null clears the override
// so the page follows the OS preference again.
function setTheme(id) {
  const el = document.documentElement;
  if (id) {
    el.setAttribute("data-theme", id);
    localStorage.setItem("theme", id);
  } else {
    el.removeAttribute("data-theme");
    localStorage.removeItem("theme");
  }
  syncThemeButton();
  if (typeof window.onThemeChange === "function") window.onThemeChange();
}

// currentThemeId returns the active theme id, resolving the OS default when no
// explicit theme is set.
function currentThemeId() {
  const set = document.documentElement.getAttribute("data-theme");
  if (set) return set;
  return matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function syncThemeButton() {
  const btn = document.getElementById("theme-btn");
  if (!btn) return;
  const t = THEMES.find((t) => t.id === currentThemeId()) || THEMES[0];
  btn.textContent = t.icon;
  btn.setAttribute("title", "Theme: " + t.label);
}

// wireThemePicker turns the theme button into a dropdown of themes.
function wireThemePicker() {
  const btn = document.getElementById("theme-btn");
  if (!btn) return;
  let pop = document.getElementById("theme-pop");
  if (!pop) {
    // Wrap the button so the popup anchors to IT (works in both the index header
    // and the reader bar, which have different heights).
    const wrap = document.createElement("span");
    wrap.className = "theme-menu";
    btn.parentElement.insertBefore(wrap, btn);
    wrap.appendChild(btn);

    pop = document.createElement("div");
    pop.id = "theme-pop";
    pop.className = "theme-pop";
    pop.setAttribute("role", "menu");
    pop.hidden = true;
    THEMES.forEach(function (t) {
      const b = document.createElement("button");
      b.type = "button";
      b.setAttribute("role", "menuitemradio");
      // NOTE: use data-theme-id, NOT data-theme: the CSS theme selectors match
      // [data-theme="x"], so a data-theme here would re-theme the menu item to
      // its own palette (unreadable). The selectors are also :root-scoped now.
      b.dataset.themeId = t.id;
      b.textContent = t.icon + "  " + t.label;
      b.addEventListener("click", function () {
        setTheme(t.id);
        markActive();
        pop.hidden = true;
        btn.setAttribute("aria-expanded", "false");
      });
      pop.appendChild(b);
    });
    wrap.appendChild(pop);
  }
  function markActive() {
    const cur = currentThemeId();
    pop.querySelectorAll("button").forEach(function (b) {
      b.setAttribute("aria-checked", String(b.dataset.themeId === cur));
    });
  }
  btn.setAttribute("aria-haspopup", "true");
  btn.setAttribute("aria-expanded", "false");
  btn.addEventListener("click", function (e) {
    e.stopPropagation();
    const open = pop.hidden;
    markActive();
    pop.hidden = !open;
    btn.setAttribute("aria-expanded", String(open));
  });
  document.addEventListener("click", function () { pop.hidden = true; btn.setAttribute("aria-expanded", "false"); });
  document.addEventListener("keydown", function (e) { if (e.key === "Escape") pop.hidden = true; });
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

// --- Audiobook setup mode toggle (first-run form) ---
// Switches the audiobook DRM section between paste-bytes and Audible-login,
// showing one form and its active tab at a time.
function audMode(mode) {
  const bytes = mode === "bytes";
  const bf = document.getElementById("aud-bytes-form");
  const lf = document.getElementById("aud-login-form");
  if (bf) bf.style.display = bytes ? "" : "none";
  if (lf) lf.style.display = bytes ? "none" : "";
  const bt = document.getElementById("aud-tab-bytes");
  const lt = document.getElementById("aud-tab-login");
  if (bt) bt.classList.toggle("active", bytes);
  if (lt) lt.classList.toggle("active", !bytes);
}

// --- Audible login (two-step: password, then 2FA/OTP) ---
// from_login can't be completed in one request when the account has 2FA: the
// one-time code doesn't exist until the password step triggers it. So step 1
// posts the credentials; if the sidecar replies otp_required it parks the login
// and returns a login_id, and we reveal the OTP form. Step 2 posts login_id +
// code to finish. On success the page reloads (the setup card then disappears).
let audLoginId = "";

function audStatus(msg, isError) {
  const el = document.getElementById("aud-login-status");
  if (!el) return;
  el.style.display = msg ? "" : "none";
  el.textContent = msg || "";
  el.style.color = isError ? "var(--danger, #c0392b)" : "var(--faint)";
}

function audPostSetup(form) {
  return fetch("/api/setup/audiobook", {
    method: "POST",
    headers: { Accept: "application/json" },
    body: new URLSearchParams(new FormData(form)),
  }).then(function (r) { return r.json().then(function (j) { return { ok: r.ok, j: j }; }); });
}

function audLogin(ev) {
  ev.preventDefault();
  audStatus("Logging in to Audible...", false);
  audPostSetup(ev.target).then(function (res) {
    if (res.j && res.j.configured) { location.reload(); return; }
    if (res.j && res.j.otp_required) {
      audLoginId = res.j.login_id || "";
      const otpForm = document.getElementById("aud-otp-form");
      const msg = document.getElementById("aud-otp-msg");
      if (msg && res.j.message) msg.textContent = res.j.message;
      if (otpForm) otpForm.style.display = "";
      audStatus("", false);
      const otpInput = otpForm && otpForm.querySelector('input[name="otp"]');
      if (otpInput) otpInput.focus();
      return;
    }
    audStatus((res.j && res.j.error) || "Login failed; use paste instead.", true);
  }).catch(function () { audStatus("Login request failed; use paste instead.", true); });
  return false;
}

function audOtp(ev) {
  ev.preventDefault();
  audStatus("Submitting code...", false);
  const form = ev.target;
  const body = new URLSearchParams(new FormData(form));
  body.set("mode", "otp");
  body.set("login_id", audLoginId);
  fetch("/api/setup/audiobook", {
    method: "POST",
    headers: { Accept: "application/json" },
    body: body,
  }).then(function (r) { return r.json().then(function (j) { return { ok: r.ok, j: j }; }); })
    .then(function (res) {
      if (res.j && res.j.configured) { location.reload(); return; }
      audStatus((res.j && res.j.error) || "Code rejected; try again or use paste.", true);
    }).catch(function () { audStatus("Code submission failed; use paste instead.", true); });
  return false;
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

// Per-book three-dot menu (Edit / Delete) on the grid cards.
function wireBookMenus() {
  function closeAll(except) {
    document.querySelectorAll(".book-menu .menu-pop").forEach(function (pop) {
      if (pop !== except) {
        pop.hidden = true;
        const btn = pop.parentElement.querySelector(".menu-btn");
        if (btn) btn.setAttribute("aria-expanded", "false");
      }
    });
  }

  document.querySelectorAll(".book-menu").forEach(function (menu) {
    const btn = menu.querySelector(".menu-btn");
    const pop = menu.querySelector(".menu-pop");
    const del = menu.querySelector(".menu-delete");
    if (!btn || !pop) return;

    btn.addEventListener("click", function (e) {
      e.stopPropagation();
      const willOpen = pop.hidden;
      closeAll(willOpen ? pop : null);
      pop.hidden = !willOpen;
      btn.setAttribute("aria-expanded", String(willOpen));
    });

    if (del) {
      del.addEventListener("click", function () {
        const slug = menu.getAttribute("data-slug");
        const title = menu.getAttribute("data-title") || "this book";
        if (!window.confirm('Delete "' + title + '"? This removes the file from the library.')) return;
        del.disabled = true;
        fetch("/api/books/" + encodeURIComponent(slug), { method: "DELETE" })
          .then(function (resp) {
            if (!resp.ok && resp.status !== 204) throw new Error("delete failed (" + resp.status + ")");
            // Remove the card from the grid (and its table row if present).
            const card = menu.closest(".card");
            if (card) card.remove();
            const row = document.querySelector('#books-table tr [data-slug="' + slug + '"]');
            if (row) {
              const tr = row.closest("tr");
              if (tr) tr.remove();
            }
          })
          .catch(function (err) {
            del.disabled = false;
            alert(err.message || "Delete failed.");
          });
      });
    }
  });

  // Click-away and Escape close any open menu.
  document.addEventListener("click", function () { closeAll(null); });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") closeAll(null);
  });
}

document.addEventListener("DOMContentLoaded", function () {
  syncThemeButton();
  wireThemePicker();
  // Restore the persisted view now that <body> exists.
  if (localStorage.getItem("view") === "table")
    document.body.setAttribute("data-view", "table");
  syncViewButton();
  document.querySelectorAll("#books-table th").forEach(th =>
    th.addEventListener("click", () => sortTable(th.getAttribute("data-key"))));
  wireBookMenus();
});
