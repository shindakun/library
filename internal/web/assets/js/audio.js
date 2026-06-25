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
  let lastSaved = -1; // wall-clock ms of the last save (for throttling)
  let restored = false; // true once the saved position has been applied

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
    if (list) {
      const prev = list.querySelector(".chapter.active");
      if (prev) prev.classList.remove("active");
      if (idx >= 0) {
        const el = list.querySelector('.chapter[data-i="' + idx + '"]');
        if (el) el.classList.add("active");
      }
    }
    if (nowChapter) {
      nowChapter.textContent = (idx >= 0 && chapters[idx]) ? (chapters[idx].title || "Chapter " + (idx + 1)) : "";
    }
    current = idx;
  }

  // Persist the current position. cfi carries the elapsed seconds (mirroring how
  // comics store the page number there); percent is seconds/duration. We do NOT
  // save before the saved position has been restored, otherwise the very first
  // timeupdate (at ~0s) would clobber the real saved spot with 0.
  function saveNow() {
    if (!restored) return;
    const t = player.currentTime || 0;
    const total = duration || player.duration || 0;
    const percent = total ? t / total : 0;
    lastSaved = nowMs();
    fetch("/api/books/" + encodeURIComponent(slug) + "/read", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ cfi: String(Math.floor(t)), percent: percent }),
    }).catch(function () {});
  }

  function nowMs() {
    return (window.performance && performance.now) ? performance.now() : Date.now();
  }

  // THROTTLE (not debounce): timeupdate fires ~4x/sec, far faster than any
  // useful save interval. A debounce that resets on every tick never fires until
  // playback stops, the original bug where position only saved on pause. Instead
  // save at most once every SAVE_INTERVAL_MS while time advances.
  var SAVE_INTERVAL_MS = 5000;
  function saveThrottled() {
    if (!restored) return;
    if (lastSaved < 0 || nowMs() - lastSaved >= SAVE_INTERVAL_MS) saveNow();
  }

  window.audioSkip = function (delta) {
    const t = (player.currentTime || 0) + delta;
    player.currentTime = Math.max(0, t);
  };

  player.addEventListener("timeupdate", function () {
    highlight(chapterAt(player.currentTime || 0));
    saveThrottled();
  });
  // Save promptly (not throttled) on pause and after a seek, so a deliberate
  // stop or scrub is captured even if the user closes the page right after.
  player.addEventListener("pause", saveNow);
  player.addEventListener("seeked", saveNow);
  // Final save on leave: sendBeacon survives unload. Guarded by `restored` like
  // the others so an immediate close before restore can't persist 0.
  window.addEventListener("pagehide", function () {
    if (!restored) return;
    const t = player.currentTime || 0;
    const total = duration || player.duration || 0;
    const body = JSON.stringify({ cfi: String(Math.floor(t)), percent: total ? t / total : 0 });
    if (navigator.sendBeacon) {
      navigator.sendBeacon("/api/books/" + encodeURIComponent(slug) + "/read",
        new Blob([body], { type: "application/json" }));
    }
  });

  // Restore the saved position once the audio knows its duration and can seek.
  // loadedmetadata can fire before duration is finite for a streamed file, so we
  // also try on durationchange/canplay and only mark restored once we've actually
  // applied it (or confirmed there's nothing to restore).
  function restore() {
    if (restored) return;
    const start = Math.max(0, Number(window.START_SECONDS) || 0);
    if (start <= 0) { restored = true; return; } // nothing saved; allow saving
    if (!isFinite(player.duration) || player.duration <= 0) return; // not ready; retry later
    player.currentTime = Math.min(start, player.duration - 1);
    restored = true;
  }
  player.addEventListener("loadedmetadata", restore);
  player.addEventListener("durationchange", restore);
  player.addEventListener("canplay", restore);
  player.addEventListener("loadedmetadata", function () {
    if (!duration) duration = player.duration || 0;
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
