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
  const rows = new Map(); // job id -> <li>
  const pending = new Map(); // filename -> placeholder <li> awaiting its real job

  function syncEmpty() {
    empty.style.display = rows.size || pending.size ? "none" : "";
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
      // First time we see this job id. If we have an optimistic placeholder for
      // its filename, adopt that element so the row doesn't flicker; otherwise
      // make a fresh one.
      li = pending.get(job.name);
      if (li) pending.delete(job.name);
      else li = document.createElement("li");
      rows.set(job.id, li);
    }
    render(li, job);
    place(li);
    syncEmpty();
  }

  // renderPending fills a placeholder <li> for a file that has no real job yet
  // (still uploading, or uploaded and waiting for the watcher). state is one of
  // "uploading" | "queued" | "failed"; it is rendered in the same shape as a
  // real job row so the in-progress upload looks exactly like a log row.
  function renderPending(li, name, state, step, isError) {
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
    // No progress bar on a failed row; otherwise an indeterminate one.
    if (!isError) li.appendChild(document.createElement("progress"));
  }

  // pendingRow returns the placeholder <li> for name, creating it if needed.
  // Keyed by name so the upload flow can transition one row through its phases
  // and so upsert() can later adopt it when the real SSE job arrives.
  function pendingRow(name) {
    let li = pending.get(name);
    if (!li) {
      // If the real job already arrived (snapshot raced us), reuse nothing.
      for (const r of rows.values()) {
        const nm = r.querySelector(".job-name");
        if (nm && nm.textContent === name) return r;
      }
      li = document.createElement("li");
      pending.set(name, li);
      place(li);
      syncEmpty();
    }
    return li;
  }

  // rekeyPending moves a placeholder from one name to another (the server may
  // sanitize the uploaded filename; the SSE job uses the staged name).
  function rekeyPending(from, to) {
    if (from === to) return;
    const li = pending.get(from);
    if (!li) return;
    pending.delete(from);
    pending.set(to, li);
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

      // Show an "uploading" row immediately, keyed by the local filename.
      const li = pendingRow(localName);
      renderPending(li, localName, "uploading", "uploading…", false);
      form.reset();

      fetch(form.action, {
        method: "POST",
        headers: { Accept: "application/json" },
        body: body,
      })
        .then(function (resp) {
          if (!resp.ok) {
            return resp.text().then(function (t) {
              throw new Error(t.trim() || "upload failed (" + resp.status + ")");
            });
          }
          return resp.json();
        })
        .then(function (data) {
          // The server may have sanitized the filename; rekey the row to the
          // staged name so the real SSE job (which uses that name) adopts it.
          const staged = data.queued || localName;
          rekeyPending(localName, staged);
          renderPending(pendingRow(staged), staged, "queued", "uploaded, waiting to import…", false);
        })
        .catch(function (err) {
          renderPending(li, localName, "failed", err.message || "upload failed", true);
          pending.delete(localName); // leave the failed row, but stop reconciling it
        })
        .finally(function () {
          if (btn) btn.disabled = false;
        });
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
