// imports.js: live import-job list driven by the /api/imports/stream SSE feed.
//
// Each SSE event is a complete Job object carrying a stable `id`. We keep a
// Map<id, rowElement> and upsert by id: known id -> update that row in place,
// new id -> append a row. The server sends a snapshot burst on connect, so a
// late-joining page is immediately correct, then streams deltas.

(function () {
  const list = document.getElementById("job-list");
  const empty = document.getElementById("job-empty");
  const rows = new Map(); // id -> <li>

  function syncEmpty() {
    empty.style.display = rows.size ? "none" : "";
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
      li = document.createElement("li");
      rows.set(job.id, li);
      render(li, job);
      place(li);
    } else {
      render(li, job);
      place(li); // re-sort if startedAt ordering changed (it won't, but cheap)
    }
    syncEmpty();
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

  syncEmpty();
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
