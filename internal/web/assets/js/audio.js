// Audiobook player. The page sets window.BOOK_SLUG and window.START_SECONDS
// before this loads. We use the browser's native <audio controls> for transport
// (play/pause/seek/speed) and add: a chapter list (click to seek, current
// chapter highlighted), resume from the saved position, +/-30s skip, and
// debounced position saving via the same /read endpoint the other readers use
// (the elapsed seconds carried in `cfi`, percent = seconds/duration).
(function () {
  const slug = window.BOOK_SLUG;
  const player = document.getElementById("player");
  const list = document.getElementById("chapters");
  const nowChapter = document.getElementById("now-chapter");
  if (!slug || !player) return;

  let chapters = []; // [{title, start}]
  let duration = 0;
  let current = -1; // index of the highlighted chapter
  let saveTimer = null;

  function fmt(secs) {
    secs = Math.max(0, Math.floor(secs || 0));
    const h = Math.floor(secs / 3600);
    const m = Math.floor((secs % 3600) / 60);
    const s = secs % 60;
    const mm = (h ? String(m).padStart(2, "0") : String(m));
    const ss = String(s).padStart(2, "0");
    return h ? h + ":" + mm + ":" + ss : mm + ":" + ss;
  }

  function renderChapters() {
    list.innerHTML = "";
    chapters.forEach(function (c, i) {
      const li = document.createElement("li");
      li.className = "chapter";
      li.setAttribute("data-i", String(i));
      const t = document.createElement("span");
      t.className = "chapter-title";
      t.textContent = c.title || ("Chapter " + (i + 1));
      const at = document.createElement("span");
      at.className = "chapter-at";
      at.textContent = fmt(c.start);
      li.appendChild(t);
      li.appendChild(at);
      li.addEventListener("click", function () {
        player.currentTime = c.start;
        player.play().catch(function () {});
      });
      list.appendChild(li);
    });
  }

  // The current chapter is the last one whose start <= t.
  function chapterAt(t) {
    let idx = -1;
    for (let i = 0; i < chapters.length; i++) {
      if (chapters[i].start <= t + 0.001) idx = i; else break;
    }
    return idx;
  }

  function highlight(idx) {
    if (idx === current) return;
    const prev = list.querySelector(".chapter.active");
    if (prev) prev.classList.remove("active");
    if (idx >= 0) {
      const el = list.querySelector('.chapter[data-i="' + idx + '"]');
      if (el) el.classList.add("active");
      nowChapter.textContent = chapters[idx] ? (chapters[idx].title || "Chapter " + (idx + 1)) : "";
    } else {
      nowChapter.textContent = "";
    }
    current = idx;
  }

  function saveSoon() {
    clearTimeout(saveTimer);
    saveTimer = setTimeout(function () {
      const t = player.currentTime || 0;
      const total = duration || player.duration || 0;
      const percent = total ? t / total : 0;
      fetch("/api/books/" + encodeURIComponent(slug) + "/read", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        // cfi carries the elapsed seconds for audio; percent is seconds/duration.
        body: JSON.stringify({ cfi: String(Math.floor(t)), percent: percent }),
      }).catch(function () {});
    }, 1000);
  }

  window.audioSkip = function (delta) {
    const t = (player.currentTime || 0) + delta;
    player.currentTime = Math.max(0, t);
  };

  player.addEventListener("timeupdate", function () {
    highlight(chapterAt(player.currentTime || 0));
    saveSoon();
  });
  // Save promptly on pause and when leaving the page, so position isn't lost.
  player.addEventListener("pause", saveSoon);
  window.addEventListener("pagehide", function () {
    const t = player.currentTime || 0;
    const total = duration || player.duration || 0;
    const body = JSON.stringify({ cfi: String(Math.floor(t)), percent: total ? t / total : 0 });
    // sendBeacon survives unload; fall back to a best-effort fetch.
    if (navigator.sendBeacon) {
      navigator.sendBeacon("/api/books/" + encodeURIComponent(slug) + "/read",
        new Blob([body], { type: "application/json" }));
    }
  });

  // Restore the saved position once the audio can seek.
  let restored = false;
  function restore() {
    if (restored) return;
    const start = Math.max(0, window.START_SECONDS || 0);
    if (start > 0 && isFinite(player.duration)) {
      player.currentTime = Math.min(start, player.duration - 1);
    }
    restored = true;
  }
  player.addEventListener("loadedmetadata", function () {
    if (!duration) duration = player.duration || 0;
    restore();
  });

  // Fetch chapters + duration, then build the list.
  fetch("/book/" + encodeURIComponent(slug) + "/chapters")
    .then(function (r) { return r.ok ? r.json() : { chapters: [], duration: 0 }; })
    .then(function (data) {
      chapters = (data && data.chapters) || [];
      duration = (data && data.duration) || duration;
      renderChapters();
      highlight(chapterAt(player.currentTime || 0));
    })
    .catch(function () {});
})();
