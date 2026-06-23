// Reader page logic: load the book into epub.js and wire theme + navigation.
// Shared theme/toggle behavior lives in app.js; this file adds the epub.js
// rendition and hooks the theme toggle to repaint the book iframe.
//
// The page sets window.BOOK_SLUG before loading this script.

(function () {
  const bookSlug = window.BOOK_SLUG;
  let rendition;

  // IMPORTANT: our file URL is /book/{slug}/file with NO ".epub" extension, and
  // epub.js's determineType() sniffs the URL extension; with none, it wrongly
  // treats the book as an exploded directory and renders nothing. So we fetch
  // the bytes ourselves and hand epub.js an ArrayBuffer, which it always opens
  // as a binary archive. (Robust regardless of URL shape or Content-Type.)
  fetch("/book/" + bookSlug + "/file")
    .then(r => {
      if (!r.ok) throw new Error("download failed: " + r.status);
      return r.arrayBuffer();
    })
    .then(buf => {
      const book = ePub(buf);
      rendition = book.renderTo("viewer", { width: "100%", height: "100%", flow: "paginated" });
      // The book iframe is themed from the CURRENT page theme's CSS variables
      // (read live), so it matches whatever app theme is active, dark, light,
      // warm, or any future theme, with no per-theme duplication here. Applied
      // before first paint so the book renders correct the first time.
      applyReaderTheme();
      return rendition.display();
    })
    .then(() => {
      // Persist reading position to the catalog.
      let saveTimer;
      rendition.on("relocated", (loc) => {
        clearTimeout(saveTimer);
        saveTimer = setTimeout(() => {
          fetch(`/api/books/${bookSlug}/read`, {
            method: "PUT",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ cfi: loc.start.cfi, percent: loc.start.percentage || 0 })
          });
        }, 800);
      });
    })
    .catch(err => {
      document.getElementById("viewer").innerHTML =
        '<p style="padding:24px;color:#b00">Could not open this book: ' + err.message + '</p>';
      console.error("reader:", err);
    });

  // Theme the book iframe to match the page. Reads the current theme's CSS
  // variables off <html> so it tracks ANY app theme (the picker button icon is
  // owned by app.js's syncThemeButton; we must NOT touch it here). The
  // `!important` is required: the book's own stylesheet and epub.js's injected
  // CSS otherwise win over a plain rule.
  function applyReaderTheme() {
    if (!rendition || !rendition.themes) return;
    const cs = getComputedStyle(document.documentElement);
    const bg = cs.getPropertyValue("--bg").trim() || "#ffffff";
    const fg = cs.getPropertyValue("--fg").trim() || "#1a1a1a";
    const link = cs.getPropertyValue("--link").trim() || fg;
    // Re-register under a single stable name with the live colors, then select.
    rendition.themes.register("app", {
      "body": { "background": bg + " !important", "color": fg + " !important" },
      "p, div, span, h1, h2, h3, h4, h5, h6, li": { "color": fg + " !important" },
      "a": { "color": link + " !important" }
    });
    rendition.themes.select("app");
  }

  // On a USER toggle, the book iframe is already painted and epub.js's
  // themes.select() does NOT restyle already-rendered content. The reliable fix
  // is to tear down the rendered views (rendition.clear) and re-display the
  // current location, which rebuilds the iframe with the new theme applied.
  function repaintForTheme() {
    applyReaderTheme();
    if (!rendition) return;
    let cfi;
    try {
      const loc = rendition.currentLocation();
      cfi = loc && loc.start && loc.start.cfi;
    } catch (_) { return; } // not rendered yet; first paint will use the theme
    if (!cfi) return;
    if (typeof rendition.clear === "function") rendition.clear();
    rendition.display(cfi);
  }

  // app.js's toggleTheme() calls this after flipping the theme.
  window.onThemeChange = repaintForTheme;

  // Expose nav for the Prev/Next buttons' inline onclick.
  window.readerPrev = () => rendition && rendition.prev();
  window.readerNext = () => rendition && rendition.next();

  document.addEventListener("DOMContentLoaded", applyReaderTheme);
  document.addEventListener("keydown", (e) => {
    if (!rendition) return;
    if (e.key === "ArrowLeft") rendition.prev();
    if (e.key === "ArrowRight") rendition.next();
  });
})();
