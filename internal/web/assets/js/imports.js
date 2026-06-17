// imports.js: live import-job list driven by the /api/imports/stream SSE feed.
//
// Each SSE event is a complete Job object carrying a stable `id`. We keep a
// Map<id, rowElement> and upsert by id: known id -> update that row in place,
// new id -> append a row. The server sends a snapshot burst on connect, so a
// late-joining page is immediately correct, then streams deltas.
//
// Uploads are async (no navigation). The server queues the file but the job is
// created later, by the watcher, so we show an immediate optimistic "queued"
// placeholder row keyed by filename and reconcile it with the real job the
// first time an SSE event for that filename arrives.

(function () {
  const list = document.getElementById("job-list");
  const empty = document.getElementById("job-empty");
  const rows = new Map(); // job id -> <li> (one row per real job, bound for life)
  // filename -> queue of UNBOUND placeholder <li>s awaiting a real job. A queue
  // (not a single slot) so re-uploading the same filename makes a distinct row
  // each time; each placeholder is adopted by exactly one job id, oldest first.
  const pending = new Map();

  function pendingCount() {
    let n = 0;
    for (const q of pending.values()) n += q.length;
    return n;
  }

  function pushPending(name, li) {
    const q = pending.get(name);
    if (q) q.push(li);
    else pending.set(name, [li]);
  }

  // takePending removes and returns the oldest unbound placeholder for name, or
  // null if none. Used by upsert() to adopt a placeholder for a newly-seen job.
  function takePending(name) {
    const q = pending.get(name);
    if (!q || !q.length) return null;
    const li = q.shift();
    if (!q.length) pending.delete(name);
    return li;
  }

  // dropPending removes a specific placeholder (e.g. a failed upload) from its
  // queue so it is never adopted by a later job.
  function dropPending(name, li) {
    const q = pending.get(name);
    if (!q) return;
    const i = q.indexOf(li);
    if (i >= 0) q.splice(i, 1);
    if (!q.length) pending.delete(name);
  }

  function syncEmpty() {
    empty.style.display = rows.size || pendingCount() ? "none" : "";
  }

  // Newest jobs on top. startedAt is an RFC3339 string; compare lexically
  // (works for same-zone timestamps) with id as a tiebreak.
  function order(a, b) {
    const ta = a.dataset.started || "";
    const tb = b.dataset.started || "";
    if (ta !== tb) return ta < tb ? 1 : -1;
    return Number(b.dataset.id) - Number(a.dataset.id);
  }

  function place(li) {
    // Insert li so the list stays sorted newest-first.
    for (const sib of list.children) {
      if (sib !== li && order(li, sib) < 0) {
        list.insertBefore(li, sib);
        return;
      }
    }
    list.appendChild(li);
  }

  function nameCell(job) {
    if (job.state === "done" && job.bookSlug) {
      const a = document.createElement("a");
      a.href = "/read/" + encodeURIComponent(job.bookSlug);
      a.textContent = job.name;
      return a;
    }
    return document.createTextNode(job.name);
  }

  function statusLine(job) {
    if (job.state === "failed") return job.error || "import failed";
    if (job.state === "skipped") return job.detail || "already in library (duplicate)";
    if (job.state === "done") return job.detail || "imported";
    // queued / running
    const parts = [];
    if (job.step) parts.push(job.step);
    if (job.detail) parts.push(job.detail);
    return parts.join(" · ") || job.state;
  }

  function render(li, job) {
    li.dataset.id = job.id;
    li.dataset.started = job.startedAt || "";
    li.className = "job";

    const name = document.createElement("span");
    name.className = "job-name";
    name.appendChild(nameCell(job));

    const fmt = document.createElement("span");
    fmt.className = "job-format";
    fmt.textContent = job.format || "?";

    const state = document.createElement("span");
    state.className = "job-state " + job.state;
    state.textContent = job.state;

    const head = document.createElement("div");
    head.className = "job-head";
    head.append(name, fmt, state);

    const line = document.createElement("div");
    line.className = job.state === "failed" ? "job-error" : "job-step";
    line.textContent = statusLine(job);

    li.replaceChildren(head, line);

    // Progress bar only while running. Determinate if the step reports a
    // fraction; otherwise indeterminate (no `value` attribute).
    if (job.state === "running" || job.state === "queued") {
      const bar = document.createElement("progress");
      if (job.progress > 0) {
        bar.max = 1;
        bar.value = job.progress;
      }
      li.appendChild(bar);
    }
  }

  function upsert(job) {
    if (!job || !job.id) return;
    let li = rows.get(job.id);
    if (!li) {
      // First time we see this job id. Adopt the oldest UNBOUND placeholder for
      // its filename (the upload that triggered it) so the row doesn't flicker;
      // otherwise make a fresh one. We never reuse an already-bound row, so a
      // re-upload of the same filename can't clobber a finished job's row.
      li = takePending(job.name) || document.createElement("li");
      rows.set(job.id, li);
    }
    render(li, job);
    place(li);
    syncEmpty();
  }

  // renderPending fills a placeholder <li> for a file that has no real job yet
  // (still uploading, or uploaded and waiting for the watcher). state is one of
  // "uploading" | "queued" | "failed"; it is rendered in the same shape as a
  // real job row so the in-progress upload looks exactly like a log row. frac,
  // when >= 0, makes the bar determinate (the upload byte fraction); pass -1 for
  // an indeterminate bar.
  function renderPending(li, name, state, step, isError, frac) {
    li.className = "job pending";
    li.dataset.started = "9999"; // sort to the very top until the real job lands
    const head = document.createElement("div");
    head.className = "job-head";
    const nm = document.createElement("span");
    nm.className = "job-name";
    nm.textContent = name;
    const st = document.createElement("span");
    st.className = "job-state " + (isError ? "failed" : state);
    st.textContent = isError ? "failed" : state;
    head.append(nm, st);
    const line = document.createElement("div");
    line.className = isError ? "job-error" : "job-step";
    line.textContent = step;
    li.replaceChildren(head, line);
    if (!isError) {
      const bar = document.createElement("progress");
      if (typeof frac === "number" && frac >= 0) {
        bar.max = 1;
        bar.value = frac;
      }
      li.appendChild(bar);
    }
  }

  // setPendingProgress updates just the bar of an existing pending row to a new
  // upload fraction (0..1) without rebuilding the row, so it animates smoothly.
  function setPendingProgress(li, step, frac) {
    const line = li.querySelector(".job-step");
    if (line) line.textContent = step;
    let bar = li.querySelector("progress");
    if (!bar) {
      bar = document.createElement("progress");
      li.appendChild(bar);
    }
    bar.max = 1;
    bar.value = frac;
  }

  // newPendingRow creates a fresh unbound placeholder <li> for a just-started
  // upload, enqueues it under name, and returns it. Each upload gets its own row
  // (never reuses an existing one), so concurrent or repeat uploads of the same
  // filename are distinct lines, each adopted by its own job.
  function newPendingRow(name) {
    const li = document.createElement("li");
    // Set the sort key BEFORE placing: an unbound upload sorts to the very top
    // (newest) so the row appears at the top immediately, not at the bottom
    // until the real job's timestamp lands.
    li.dataset.started = "9999";
    pushPending(name, li);
    place(li);
    syncEmpty();
    return li;
  }

  // rekeyPending moves a placeholder from one name-queue to another (the server
  // may sanitize the uploaded filename; the SSE job uses the staged name).
  function rekeyPending(li, from, to) {
    if (from === to) return;
    dropPending(from, li);
    pushPending(to, li);
  }

  function connect() {
    const es = new EventSource("/api/imports/stream");
    es.onmessage = function (ev) {
      try {
        upsert(JSON.parse(ev.data));
      } catch (e) {
        /* ignore malformed event */
      }
    };
    // EventSource auto-reconnects on error; the server replays a snapshot on
    // each connect, so reconnection self-heals the list. Nothing to do here.
  }

  // Intercept the upload form so a submit does NOT navigate away: post via
  // fetch (Accept: application/json -> the server returns {"queued"} instead of
  // a redirect) and show the file's progress as a row in the same list as the
  // import logs, from "uploading" through "queued" until the real SSE job
  // adopts the row. This is the whole point of the dedicated page: stay put and
  // watch progress, with the upload itself rendered like every other entry.
  function wireUpload() {
    const form = document.getElementById("upload-form");
    if (!form) return;
    const input = form.querySelector('input[type="file"]');
    const btn = form.querySelector('button[type="submit"]');

    form.addEventListener("submit", function (ev) {
      ev.preventDefault();
      if (!input || !input.files || !input.files.length) return;
      const localName = input.files[0].name;
      const body = new FormData(form);
      if (btn) btn.disabled = true;

      // Show a fresh "uploading" row immediately (its own line, even for a
      // re-upload of the same filename), with a determinate bar starting at 0.
      const li = newPendingRow(localName);
      let curName = localName; // the queue this row currently lives under
      renderPending(li, localName, "uploading", "uploading…", false, 0);
      form.reset();

      // XMLHttpRequest (not fetch) because only XHR exposes upload byte progress,
      // which is what fills the bar as the file streams to the server.
      const xhr = new XMLHttpRequest();
      xhr.open("POST", form.action);
      xhr.setRequestHeader("Accept", "application/json");

      xhr.upload.onprogress = function (e) {
        if (e.lengthComputable) {
          const frac = e.total ? e.loaded / e.total : 0;
          const pct = Math.round(frac * 100);
          setPendingProgress(li, "uploading… " + pct + "%", frac);
        }
      };

      xhr.onload = function () {
        if (btn) btn.disabled = false;
        if (xhr.status < 200 || xhr.status >= 300) {
          const msg = (xhr.responseText || "").trim() || "upload failed (" + xhr.status + ")";
          dropPending(curName, li); // never adopt a failed upload
          renderPending(li, curName, "failed", msg, true);
          return;
        }
        let staged = localName;
        try {
          const data = JSON.parse(xhr.responseText);
          if (data && data.queued) staged = data.queued;
        } catch (e) {
          /* keep localName */
        }
        // Move the row to the staged-name queue so the real SSE job (which uses
        // that name) adopts it, and flip it to "queued" with a full bar until the
        // import job takes over.
        rekeyPending(li, curName, staged);
        curName = staged;
        renderPending(li, staged, "queued", "uploaded, waiting to import…", false, 1);
      };

      xhr.onerror = function () {
        if (btn) btn.disabled = false;
        dropPending(curName, li);
        renderPending(li, curName, "failed", "upload failed (network error)", true);
      };

      xhr.send(body);
    });
  }

  syncEmpty();
  wireUpload();
  if (window.EventSource) {
    connect();
  } else {
    // No SSE support: fall back to a one-shot snapshot fetch.
    fetch("/api/imports")
      .then((r) => r.json())
      .then((jobs) => (jobs || []).forEach(upsert))
      .catch(() => {});
  }
})();
