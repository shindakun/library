// comic.js: a minimal, dependency-free image-sequence reader for CBZ comics.
//
// The page sets window.BOOK_SLUG and window.START_PAGE before this loads. We
// fetch the page count once, then swap a single <img> as the reader pages
// through. Position (the current page) is persisted to the same read endpoint
// the epub reader uses, with the page number carried in `cfi`.

(function () {
  const slug = window.BOOK_SLUG;
  const img = document.getElementById("comic-page");
  const indicator = document.getElementById("page-indicator");
  const viewer = document.getElementById("viewer");

  let count = 0;
  let page = Math.max(0, window.START_PAGE | 0);
  let saveTimer = null;

  function pageURL(n) {
    return "/book/" + encodeURIComponent(slug) + "/page/" + n;
  }

  function show(n) {
    if (count > 0) n = Math.max(0, Math.min(n, count - 1));
    page = n;
    img.src = pageURL(page);
    img.scrollIntoView({ block: "start" });
    viewer.scrollTop = 0;
    updateIndicator();
    preloadNext();
    saveSoon();
  }

  function updateIndicator() {
    indicator.textContent = count ? page + 1 + " / " + count : "";
  }

  // Preload the next page so a forward tap is instant.
  function preloadNext() {
    if (count && page + 1 < count) {
      const pre = new Image();
      pre.src = pageURL(page + 1);
    }
  }

  function saveSoon() {
    clearTimeout(saveTimer);
    saveTimer = setTimeout(function () {
      const percent = count ? (page + 1) / count : 0;
      fetch("/api/books/" + encodeURIComponent(slug) + "/read", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        // cfi carries the page number for comics; percent is page/count.
        body: JSON.stringify({ cfi: String(page), percent: percent }),
      }).catch(function () {});
    }, 600);
  }

  window.readerPrev = function () { show(page - 1); };
  window.readerNext = function () { show(page + 1); };

  // Fit mode: "width" (default, fills the column, scroll vertically) or "height"
  // (whole page visible, letterboxed). Persisted in localStorage.
  function applyFit() {
    const fit = localStorage.getItem("comicFit") === "height" ? "height" : "width";
    document.body.dataset.fit = fit;
    const btn = document.getElementById("fit-btn");
    if (btn) btn.textContent = fit === "height" ? "Fit ↔" : "Fit ↕";
  }
  window.comicToggleFit = function () {
    const next = (localStorage.getItem("comicFit") === "height") ? "width" : "height";
    localStorage.setItem("comicFit", next);
    applyFit();
  };

  // Keyboard: left/up = prev, right/down/space = next.
  document.addEventListener("keydown", function (e) {
    if (e.key === "ArrowLeft" || e.key === "ArrowUp") {
      e.preventDefault();
      window.readerPrev();
    } else if (e.key === "ArrowRight" || e.key === "ArrowDown" || e.key === " ") {
      e.preventDefault();
      window.readerNext();
    }
  });

  // Tap left/right thirds of the image to page (touch-friendly).
  viewer.addEventListener("click", function (e) {
    const x = e.clientX - viewer.getBoundingClientRect().left;
    if (x < viewer.clientWidth * 0.33) window.readerPrev();
    else if (x > viewer.clientWidth * 0.66) window.readerNext();
  });

  img.addEventListener("error", function () {
    indicator.textContent = "page failed to load";
  });

  applyFit();

  fetch("/book/" + encodeURIComponent(slug) + "/pages")
    .then(function (r) {
      if (!r.ok) throw new Error("pages: " + r.status);
      return r.json();
    })
    .then(function (data) {
      count = (data && data.count) | 0;
      if (!count) {
        indicator.textContent = "no pages";
        return;
      }
      show(page); // clamps START_PAGE into range
    })
    .catch(function (err) {
      indicator.textContent = "could not open comic";
      // eslint-disable-next-line no-console
      console.error("comic:", err);
    });
})();
