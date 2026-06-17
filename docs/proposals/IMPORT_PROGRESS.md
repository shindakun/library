# Proposal: import jobs, progress feedback, and a dedicated import page

Status: **proposed, not implemented.** Design + build plan for surfacing import
progress to the UI. Today imports are fire-and-forget (the watcher logs to
stderr; the web layer has no job state). This adds a generic **import-job model**
shared by every import path, a **live status stream (SSE)**, and a **dedicated
import page** with progress. It is a prerequisite for comics, where CBR->CBZ
conversion is a multi-minute operation that needs real feedback (see
COMIC_SUPPORT.md).

## 1. Why now, and why generic

The current pipeline (`ingest.handle`) is fast (epub fulfill/decrypt is seconds),
so "drop a file, refresh the grid" is acceptable. Comics break that: converting a
700 MB CBR is minutes, and silent processing reads as broken. The feedback must
exist before comics land.

It must be **generic across all import paths**, not comic-specific. Every import
is a job with a lifecycle: `.acsm` fulfill, ADEPT decrypt, direct epub import, and
(later) comic convert are all the same shape (queued -> running through steps ->
done | failed | skipped). One job model serves all of them; comics plug in by
reporting progress into it. Retrofitting it per-format would be wasted work.

## 2. The job model (`internal/ingest`)

`handle()` is already the single funnel every import passes through, with clear
transitions (the existing `fmt.Printf "processing/done/skipping"` and `fail()`
lines ARE the state changes). Formalize them into a tracked job.

```go
type JobState string

const (
    StateQueued  JobState = "queued"
    StateRunning JobState = "running"
    StateDone    JobState = "done"
    StateFailed  JobState = "failed"
    StateSkipped JobState = "skipped" // duplicate content
)

type Job struct {
    ID        string    // stable per import (e.g. a ULID or hash of path+mtime)
    Name      string    // original filename
    Format    string    // "acsm" | "epub" | "cbz" | "cbr" (later)
    State     JobState
    Step      string    // human label: "fulfilling", "decrypting", "converting", "indexing"
    Progress  float64   // 0..1 for steps that can report it (CBR convert); else 0
    Detail    string    // optional extra ("page 142/610")
    Err       string    // set when State == failed
    StartedAt time.Time
    EndedAt   time.Time // zero until terminal
    BookSlug  string    // set on success, so the UI can link to the imported book
}
```

A `Jobs` registry on the `Importer` holds them:

```go
type Jobs struct {
    mu   sync.Mutex
    jobs map[string]*Job   // active + recently-finished (capped/TTL'd)
    subs map[chan Event]struct{} // SSE subscribers
}

func (j *Jobs) Start(name, format string) *Job          // -> queued
func (j *Jobs) Update(id string, mutate func(*Job))      // running/step/progress; broadcasts
func (j *Jobs) Finish(id string, state JobState, err error, slug string)
func (j *Jobs) Snapshot() []*Job                         // for the initial page load / poll
func (j *Jobs) Subscribe() (<-chan Event, func())        // SSE; returns an unsubscribe
```

Every `Update`/`Finish` broadcasts an `Event` to all subscribers (non-blocking
send; drop to a slow subscriber's buffer rather than stalling the import).

Retention: keep finished jobs for a short window (e.g. last 50, or 10 min) so the
page shows "just completed" without growing unbounded. Pure in-memory; jobs do not
survive a restart (acceptable: a restart means re-scan, and the library state is
the source of truth).

## 3. Threading it through `handle()`

Minimal, surgical changes at the points that already log:

```text
handle(path):
  job = Jobs.Start(base(path), formatOf(path))      // queued -> visible immediately
  Jobs.Update(job, running, step="processing")
  pipeline(path, onProgress=func(step, frac, detail){ Jobs.Update(job, step, frac, detail) })
    .acsm  -> step "fulfilling" (sidecar; coarse: started/done)
    ADEPT  -> step "decrypting"
    cbr    -> step "converting", frac = pagesDone/pageCount   <-- the long one
  verify -> step "verifying"
  dedup-hit -> Jobs.Finish(skipped); return
  index  -> step "indexing"
  Jobs.Finish(done, slug=...)
  on any error -> Jobs.Finish(failed, err)
```

The sidecar jobs (fulfill/decrypt) are coarse-grained (the Python worker returns
one result); they report step transitions, not a percent. The **CBR conversion is
the one path with a real percentage** (pages extracted / total), which is exactly
why the progress bar is worth building. The progress callback that carries that
fraction (`progressFunc(step, frac, detail)`) is implemented and generic on this
branch (see §6 step 4); a future `comic.Convert` is just another caller of it. The
fraction flows callback -> `j.Progress` -> SSE -> the `<progress>` element with no
format-specific code in between.

## 4. HTTP surface (`internal/web`)

```text
GET  /imports                 the import page (server-rendered shell)
GET  /api/imports             JSON snapshot of current jobs (initial load; also a
                              poll fallback if SSE drops)
GET  /api/imports/stream      SSE: text/event-stream, one event per job update
POST /api/upload              (existing) returns {"queued": <filename>}
```

Note: `POST /api/upload` cannot return a job *id*: the job is created later by
the watcher when it picks the file up (async), not by the upload handler. The
file-to-job correlation key is therefore the **filename** (which upload already
returns). The UI keys job rows by name to highlight a just-uploaded file once its
job appears via SSE. (Revised from "return the job id".)

### SSE specifics

- `Content-Type: text/event-stream`, flush after each event, honor
  `r.Context().Done()` to drop the subscriber when the client disconnects.
- Each event is the JSON of the changed `Job`. The client updates that row.
- On connect, send the current snapshot first (so a late-joining page is
  immediately correct), then stream deltas.
- Stdlib only: `http.Flusher` + the `Jobs.Subscribe` channel. No dependency.

## 5. The import page (`internal/web/assets`)

Move import off the library grid into its own page, cleaner and purpose-built:

- `GET /imports` renders `imports.html`: a drop zone + file picker (the existing
  upload control, relocated) and a **live job list**.
- `js/imports.js`: opens the SSE stream, renders one row per job with
  name, format, state, current step, and a **progress bar** (driven by
  `job.Progress` for CBR; indeterminate/striped for coarse steps). Done rows link
  to the imported book; failed rows show the error and a link to `import/failed/`.
- Reuses `app.css`/`app.js` (theme, layout). No framework; this is a list that
  mutates on SSE events, plus one `<progress>` element per row.
- Decided during build: the inline upload control is dropped from the library
  grid entirely; the grid header now carries an "Import" nav button to `/imports`,
  which owns the upload control and the live list.

## 6. Build order

1. **Job model** (DONE): `internal/ingest/jobs.go` (`Job`, `Jobs` registry with
   `Start/Update/Finish/Snapshot/Subscribe`, count+TTL retention, copy-on-
   broadcast, drop-on-slow-subscriber); `Importer.JobRegistry()` lazy accessor;
   wired through `handle()` and `pipeline()` (fulfilling/decrypting/verifying/
   indexing steps; done/failed/skipped terminals; success records the book slug).
   The job is created BEFORE `pipelineMu` is taken, so a waiting import shows as
   "queued". `jobs_test.go` covers lifecycle, unknown-id no-ops, subscribe/
   broadcast, copy-isolation, unsubscribe, slow-subscriber-no-block, and all three
   retention paths; race-clean.
2. **API** (DONE): `GET /api/imports` (snapshot JSON) + `GET /api/imports/stream`
   (SSE: snapshot burst on connect, then one `data:` event per job update, 25s
   keepalive comments, stops on client disconnect). `Server` holds a
   `*ingest.Jobs` (passed from `main` via `importer.JobRegistry()`, created before
   `web.New`). Tested with a real httptest listener: snapshot endpoint, empty
   case, and the live SSE stream (snapshot + post-connect update + disconnect).
   Race-clean.
3. **Import page** (DONE): `GET /imports` renders `imports.html` (header with the
   relocated upload control + a back-to-library link); the library grid now links
   to it via an "Import" nav button instead of an inline upload form.
   `js/imports.js` opens `/api/imports/stream`, keeps a `Map<id, row>`, upserts
   one `<li>` per job by stable id (snapshot burst seeds it; deltas update in
   place), renders name/format/state/step and a `<progress>` bar while running
   (determinate when `job.progress > 0`, indeterminate otherwise). Done rows link
   to `/read/<slug>`; failed rows show the error; skipped rows note the duplicate.
   `EventSource` auto-reconnects and the server replays a snapshot on connect, so
   reconnection self-heals; a no-SSE fallback does a one-shot `/api/imports` fetch.
   Styles appended to `css/app.css`. Verified statically: `/imports` renders 200
   with the `job-list` mount and `imports.js`/`app.css` serving 200
   (`TestImportsPageServes`); JS behavior is for the user to confirm in-browser,
   per the project's frontend-verification convention.
4. **Generic fractional-progress plumbing** (DONE, on THIS branch): the progress
   path is now generic enough that a long, fraction-reporting step (CBR->CBZ
   conversion being the motivating one) lights up the bar with NO further changes
   to the job model, the SSE layer, or the UI. The contract is the existing
   `progressFunc(step string, frac float64, detail string)`: `handle()` builds one
   `onProgress` that writes `j.Step/j.Progress/j.Detail` and broadcasts, and
   `pipeline()` already receives it. A converter calls
   `onProgress("converting", pagesDone/float64(total), "page 142/610")` in its
   loop; the bar follows. Hardening done here so it survives the CBR frame rate:
   - `Jobs.broadcast` now **coalesces to newest** on a full subscriber buffer
     (evict oldest, enqueue newest) instead of dropping the newest. A slow client
     during a hundreds-of-frames-per-second conversion converges on the current
     fraction rather than stalling on a stale one, and the terminal frame is
     never stranded behind a backlog of superseded progress.
   - `Jobs.Finish` zeroes `Progress` on `failed`/`skipped` (and sets `1` on
     `done`), so no consumer ever sees a stale "60% done" on a job that failed
     mid-progress.
   - The SSE keepalive tick re-emits the snapshot of any **non-terminal** job, a
     belt-and-suspenders reconcile: a progress bar can be at most ~25s stale even
     if a live frame was coalesced away, and finished rows are never re-sent.
   Tests: `TestProgressStreamConvergesOnLatest` (500 frames, slow drain, ends at
   1.0), `TestTerminalFrameSurvivesFullBuffer` (terminal survives a saturated
   buffer), and the `failed`-job progress-zeroing assertion. Race-clean.

   NOTE: the actual `.cbr`/`.cbz` accept-filter, `sourceFor` classification, and
   the `comic.Convert` step itself are intentionally NOT here: they live on the
   comics branch (COMIC_SUPPORT.md). This branch delivers only the generic
   progress infrastructure they plug into. (Corrects an earlier draft that
   deferred this plumbing to the comics branch: keeping it generic and landing it
   here is the whole point.)

## 7. Decisions / open questions

- **SSE vs poll:** SSE chosen (real-time, server->client only, stdlib, no
  dependency). `GET /api/imports` stays as the initial-load + reconnect fallback.
- **Job identity:** needs to be stable for the life of one import so SSE updates
  target the right row. **Implemented** as a process-local atomic counter
  (`strconv` of an incrementing `atomic.Uint64`), not a ULID: ids only need
  uniqueness within one process run and do not persist, so no dependency is
  warranted. (Revised from the original "ULID" note.)
- **Concurrency:** imports are already serialized by `pipelineMu` (one at a time),
  so at most one job is `running`; others are `queued`. The page shows the queue.
  If import parallelism is ever added, the job model already supports N running.
- **Retention policy** (count vs TTL) is a tuning detail; start with "last 50 or
  10 minutes, whichever is larger."
- **Library page inline upload control:** resolved. `/imports` is the single
  upload entry point; the grid links to it with an "Import" nav button (no inline
  upload form). A "N importing..." activity indicator on the grid is a possible
  later nicety, not built yet.

## 8. Risks / notes

- **SSE through proxies:** if the service is ever fronted by a buffering proxy,
  SSE needs `X-Accel-Buffering: no` / flush; fine for the direct LAN deploy.
- **Slow subscribers:** never block an import on a stuck SSE client; buffered
  channel + drop-oldest, and the snapshot endpoint lets a reconnecting client
  resync.
- **Restart loses job history:** acceptable; jobs are transient UI state, the
  catalog is the durable truth.
- **This unblocks comics** but is independently useful: even for epubs, a visible
  "fulfilling -> decrypting -> done" beats silent processing, especially for the
  time-sensitive `.acsm` loans.
