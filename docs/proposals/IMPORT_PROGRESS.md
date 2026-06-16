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
why the progress bar is worth building. The `drm.Client` and the future
`comic.ConvertCBR` take an optional progress callback.

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
- The library page keeps a small "N importing..." indicator linking to `/imports`
  (so you see activity without leaving the grid), or drop the inline upload
  control there entirely in favor of the dedicated page. (Decide during build.)

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
3. **Import page**: `imports.html` + `imports.js`, the relocated upload control,
   the live list + progress bars. Browser-verified by the user (JS is checked
   statically, per the project's frontend-verification convention).
4. **Progress callback plumbing**: thread an `onProgress` through `pipeline()` and
   the `drm.Client`; the sidecar steps report transitions. (CBR's real percentage
   arrives with COMIC_SUPPORT.md.)
5. Then return to comics: `comic.ConvertCBR` reports `pagesDone/total` into the
   job, lighting up the progress bar end to end.

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
- **Does the library page keep an inline upload control,** or is `/imports` the
  only entry point? Lean toward a dedicated page with a small activity indicator
  on the grid; finalize during build.

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
