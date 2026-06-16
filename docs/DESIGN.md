# Library: Design Document

A self-hosted ebook library. Stores EPUBs, serves a browser-based reader, exposes an
OPDS catalog the Xteink X4 (Crosspoint firmware) browses over WiFi, and ingests
Adobe-DRM books by fulfilling `.acsm` files and stripping the legacy ADEPT DRM on import.

- **Status:** Implemented and verified end-to-end on rootless Podman. Pipeline
  (fulfill → decrypt → catalog), browser reader, OPDS feed, web upload, and the
  two-container compose stack all working. Not yet validated against real X4
  hardware. See §9 (resolved decisions and future work) and this project's README.md.
- **Author:** steve
- **Date:** 2026-06-15
- **Stack:** Go service (single static binary) + a quarantined Python "DRM sidecar"
  container. Rootless Podman on a home server/NAS on the LAN, driven by `docker
  compose` (which talks to Podman's socket) via a root Makefile.

---

## 1. Goals & non-goals

### Goals

1. **Browser library + reader.** Catalog every EPUB; read any of them in a browser.
2. **Xteink X4 / Crosspoint.** No device-side code. The X4's built-in OPDS client points
   at our `/opds` feed and browses/downloads the whole library over WiFi.
3. **Adobe DRM import.** Drop an `.acsm` (or an already-fulfilled ADEPT EPUB) into an
   import folder; the system fulfills the download, strips ADEPT DRM, and the clean EPUB
   lands in the library. Almost all DRM here is **Adobe Digital Editions (ADEPT)**.

### Non-goals

- Not a Kindle/KFX pipeline. Amazon DRM is out of scope (and, per 2026 reality, largely
  dead for recent purchases anyway). This is an ADEPT back-catalog liberator.
- Not multi-tenant. Single user, single Adobe authorization.
- Not public-internet-facing in v1. LAN-only; auth/TLS are a later concern.
- No reimplementation of DRM logic in Go. That stays in the proven Python tools.

---

## 2. The DRM toolchain

Both upstream repos were cloned and read at the source level rather than from
summaries. The findings below drive the sidecar design.

### 2.1 What `acsm-calibre-plugin` actually does

`Leseratte10/acsm-calibre-plugin` is a pure-Python reimplementation of `libgourou`. It
turns an `.acsm` file into an EPUB/PDF **without Adobe Digital Editions**. Crucially, it
ships **standalone scripts that run with no Calibre GUI and no Calibre at all**: they
depend only on the bundled `libadobe*` modules plus `lxml`:

- `register_ADE_account.py`: one-time: authorize an Adobe ID (or anonymous), producing
  `activation.xml`, `device.xml`, `devicesalt`.
- `fulfill.py URLLink.acsm`: fulfills the `.acsm`: contacts Adobe, downloads the book,
  and (for EPUB) writes `META-INF/rights.xml` into the zip. **The output is still
  ADEPT-encrypted**: fulfillment ≠ decryption.
- `get_key_from_Adobe.py`: exports the account's decryption key as
  `adobekey_<mail>_uuid_<uuid>.der`. (Confirmed in source: it logs in, calls
  `exportAccountEncryptionKeyDER`, writes a `.der`.)

So `fulfill.py` gets us a **DRM-laden EPUB**, and `get_key_from_Adobe.py` gets us the
**`.der` key** that decrypts it.

### 2.2 What `DeDRM_tools` (noDRM fork) does with that

`noDRM/DeDRM_tools` contains `DeDRM_plugin/ineptepub.py` + `adobekey.py`: the ADEPT EPUB
decryptor. It consumes exactly the `.der` key produced above. Two ways to run it:

- **Inside Calibre (CLI, headless):** `calibre-customize --add DeDRM_plugin.zip`, drop the
  `.der` into `dedrm.json` under `$CALIBRE_CONFIG_DIRECTORY/plugins/`, then
  `calibredb add book.epub --with-library=...` decrypts on import. (Confirmed in
  `CALIBRE_CLI_INSTRUCTIONS.md`.)
- **Standalone:** `ineptepub.py` can decrypt an EPUB given the `.der` directly, no Calibre
  process at all.

### 2.3 The "does it auto-remove DRM?" nuance

The acsm-calibre-plugin README states: when used **inside Calibre with noDRM's DeDRM
fork installed**, the key export → import step "will happen automatically." That convenience only exists in
the *GUI plugin pairing*. The sidecar does not get it for free, and does not need it: the
key is driven explicitly (§4.2). The two repos compose directly (acsm-calibre-plugin
fulfills, DeDRM strips) with the sidecar controlling the seam.

### 2.4 Two integration strategies (we pick B)

> **Strategy A, "Calibre as the engine."** Run full Calibre headless. Install both plugins
> via `calibre-customize`. Fulfill `.acsm` through the ACSM *input* plugin, decrypt through
> DeDRM, manage everything with `calibredb`.
>
> - **Pro:** least custom code; the acsm-calibre-plugin's "automatic" key handoff
>   (its README, §2.3 above) works.
> - **Con:** drags in all of Calibre (~hundreds of MB), its own metadata DB and library
>   format that would fight our Go catalog, and a heavier, GUI-shaped dependency. The
>   ACSM-as-input-format path is GUI-centric and awkward to trigger per-file headlessly.
>
> **Strategy B, "Scripts as tools" (CHOSEN).** Ignore Calibre entirely. The sidecar runs
> only the three standalone `acsm-calibre-plugin` scripts + DeDRM's `ineptepub.py`, all
> pure-Python (`lxml`, `pycryptodome`). The Go service orchestrates them as plain CLI steps:
> `fulfill.py` → `ineptepub.py`. No Calibre, no `calibredb`, no second metadata store.
>
> - **Pro:** tiny image, no competing DB, every step is a clean `exec` with files in/out,
>   Go stays the single source of truth.
> - **Con:** we own the orchestration (it's ~3 shell steps) and the `.der` plumbing.

We pick **B**: it keeps the Python contamination to a thin, well-understood corner and
leaves the Go service authoritative. Calibre's value-add (its library manager) is exactly
the thing we're replacing.

---

## 3. Architecture

```text
                     ┌──────────────────────────────────────────────┐
   Browser ────────▶ │  library (Go, single static binary)          │
                     │   ├─ Reader UI        epub.js over embed.FS  │
   X4 / Crosspoint ──┤   ├─ OPDS 1.2 feed    GET /opds, /opds/search │ ◀─ X4 browses+downloads (WiFi)
     (OPDS client)   │   ├─ REST/HTML API    /api/*                 │
                     │   ├─ Ingest watcher   fsnotify on import/    │
                     │   └─ SQLite catalog   modernc.org/sqlite     │
                     └───────┬───────────────────────────────┬──────┘
                             │ files in/out                   │ exec, rarely
                  ┌──────────▼──────────┐          ┌──────────▼──────────────────-───┐
                  │ /data/library/*.epub│          │ drm-sidecar (python, --rm)      │
                  │ /data/catalog.db    │          │  fulfill.py → ineptepub.py      │
                  │ /data/import/  (in) │          │  mounts: /secrets (activation,  │
                  │ /secrets/  (.der,   │          │          .der), /work (job tmp) │
                  │   activation.xml…)  │          └─────────────────────────────────┘
                  └─────────────────────┘
```

Two containers, one job each, composed with `docker-compose`. The Go binary **never
imports Python**; it invokes the sidecar per job. Because DRM import is occasional, the
sidecar is run `--rm` on demand, with no long-lived Python process.

---

## 4. The DRM ingest pipeline

### 4.1 One-time setup (manual, done once)

Run inside the sidecar image, interactively, the first time:

1. `register_ADE_account.py` → authorizes a (throwaway) Adobe ID, writes `activation.xml`,
   `device.xml`, `devicesalt` into `/secrets`.
2. `get_key_from_Adobe.py` → writes `adobekey_…uuid….der` into `/secrets`.
3. Back up `/secrets` somewhere safe. These files are the whole authorization; losing them
   burns an activation.

`/secrets` is a host bind-mount, `0700`, never in the image, never in git.

### 4.2 Per-import flow (automatic)

A file enters `/data/import/` either by a direct drop or via the web upload
endpoint (`POST /api/upload`, staged atomically). The watcher uses **fsnotify
plus a 5-second polling fallback**: inotify events do NOT cross the macOS →
Podman-VM bind mount, so polling is what actually makes imports fire there; the
in-flight dedupe keeps the two paths from racing. Imports are serialized so the
shared `work/` scratch can be wiped cleanly after each job.

```text
file lands in import/ (dropped or uploaded)
   │
   ├─ *.acsm ──────▶ sidecar fulfill ─▶ <Title>.epub (ADEPT) ─▶ sidecar decrypt ─┐
   │                                                                             │
   ├─ *.epub, ADEPT ───────────────────────────────────────▶ sidecar decrypt ────┤
   │                                                                             │
   └─ *.epub, no DRM ─────────────────────────────────────────────────────(as-is)┤
                                                                                 ▼
              Go: verify it parses (epub.IsADEPTEncrypted decided the route above)
                        │
                        ▼
              name from title, move to /data/library/Author/Title.epub, index it
                        │
                        ▼
              move original out of import/ to import/done/ (or import/failed/ on error)
```

The Go side is the orchestrator and the bookkeeper; the sidecar is two dumb file→file
transforms. Each invocation is `docker run --rm -v /secrets:/secrets:ro -v <jobtmp>:/work
drm-sidecar <step> <args>`.

### 4.3 Failure handling

- A book that won't decrypt (wrong/foreign account key) → moved to `import/failed/`, logged,
  surfaced in the UI. Never silently dropped.
- Fulfillment errors (expired `.acsm`, Adobe-side refusal) → same, with the Adobe response
  captured in the job log.
- The sidecar is the only component allowed to touch `/secrets`, and only read-only.

---

## 5. Go service design

Packages: `internal/catalog` (SQLite: index/scan/prune/reorganize/query),
`internal/epub` (metadata, cover, ADEPT detection), `internal/opds` (the paged
feed), `internal/web` (HTTP only: UI, reader, upload, file/cover, JSON API),
`internal/ingest` (the import watcher + DRM pipeline, independent of HTTP),
`internal/drm` (Go client driving the sidecar), and `internal/fileutil` (shared
filename/path helpers, incl. the `Author/Title.epub` layout). `cmd/library` wires
them together.

### 5.1 Dependencies (deliberately small, mostly stdlib)

| Concern | Choice | Why |
| --- | --- | --- |
| HTTP | stdlib `net/http` + `http.ServeMux` (1.22 routing) | No framework needed. |
| DB | `modernc.org/sqlite` | Pure-Go, no cgo → static binary, tiny image. |
| EPUB metadata | `archive/zip` + `encoding/xml` on the OPF | An EPUB is a zip + OPF manifest; reading title/author/cover/identifiers is ~100 lines and avoids a dependency. |
| Watcher | `github.com/fsnotify/fsnotify` | Standard. |
| OPDS / feeds | hand-rolled `encoding/xml` structs | OPDS 1.2 is Atom; ~150 lines, full control. |
| Frontend reader | `epub.js`, served from `embed.FS` | No node, no build step. |
| Covers | stdlib `image/*` + a resize | Thumbnails for the grid. |

### 5.2 Data model (SQLite)

```sql
CREATE TABLE books (
  id            INTEGER PRIMARY KEY,
  title         TEXT NOT NULL,
  sort_title    TEXT,
  path          TEXT NOT NULL UNIQUE,   -- relative to /data/library, e.g. "Author/Title.epub"
  file_size     INTEGER,
  file_hash     TEXT,                   -- sha256, dedupe + change detection
  cover_path    TEXT,                   -- relative to cover cache
  language      TEXT,
  published     TEXT,
  description    TEXT,
  added_at      INTEGER NOT NULL,
  source        TEXT                    -- 'acsm' | 'epub-import' | 'scan'
);
CREATE TABLE authors  (id INTEGER PRIMARY KEY, name TEXT UNIQUE);
CREATE TABLE book_authors (book_id INT, author_id INT, PRIMARY KEY(book_id,author_id));
CREATE TABLE series   (id INTEGER PRIMARY KEY, name TEXT UNIQUE);
CREATE TABLE book_series (book_id INT, series_id INT, idx REAL);
CREATE TABLE tags     (id INTEGER PRIMARY KEY, name TEXT UNIQUE);
CREATE TABLE book_tags (book_id INT, tag_id INT, PRIMARY KEY(book_id,tag_id));
CREATE TABLE identifiers (book_id INT, scheme TEXT, value TEXT);  -- isbn, uuid…
CREATE VIRTUAL TABLE books_fts USING fts5(title, authors, description, content='');
CREATE TABLE read_state (book_id INT PRIMARY KEY, percent REAL, cfi TEXT, updated_at INT);
```

`read_state.cfi` holds an epub.js CFI so browser reading position survives reloads.
(Crosspoint syncs progress via its own KOReader-sync mechanism, independent of this.)

### 5.3 HTTP surface

```text
GET  /                      catalog UI (grid, search, filter by author/series/tag)
GET  /read/{id}             epub.js reader for a book
GET  /api/books             JSON list (paged, ?q= search, ?author= ?series= ?tag=)
GET  /api/books/{id}        JSON detail
GET  /book/{id}/file        the raw EPUB (download / reader fetch)
GET  /book/{id}/cover       cover thumbnail
PUT  /api/books/{id}/read   update read position {percent, cfi}
POST /api/scan              rescan /data/library for new/changed files

# --- OPDS (what the X4 consumes) ---
GET  /opds                  root navigation feed (acquisition links to subsections)
GET  /opds/new              recently added (acquisition feed)
GET  /opds/authors          navigation feed → per-author acquisition feeds
GET  /opds/series           navigation feed → per-series acquisition feeds
GET  /opds/all              full acquisition feed (paginated, rel=next/prev)
GET  /opds/search?q=        OpenSearch results as an acquisition feed
GET  /opds/opensearch.xml   OpenSearch description doc
```

### 5.4 OPDS specifics (the X4 contract)

This is the load-bearing interop point, so it gets called out explicitly:

- **OPDS 1.2** (Atom-based). Crosspoint's client supports in-catalog search, next/prev
  pagination, multiple servers, relative paths, and KOReader-compatible download filenames
  (per its docs).
- Acquisition entries use `rel="http://opds-spec.org/acquisition"` with
  `type="application/epub+zip"` pointing at `/book/{id}/file`.
- Cover/thumbnail links use `rel="http://opds-spec.org/image"` and `…/image/thumbnail`.
- Search advertised via `rel="search" type="application/opensearchdescription+xml"`.
- Download filenames set via `Content-Disposition` so Crosspoint names files sanely.

#### 5.4.1 PAGING IS MANDATORY (X4 must never get an unbounded feed)

The X4 runs on an **ESP32C3 with very little RAM**. Handing its OPDS client one giant
acquisition feed (every book in the library serialized into a single Atom document) can
make the firmware **hang or OOM while parsing**. This is a hard correctness requirement,
not a nicety. The design enforces it two ways:

1. **The root feed (`/opds`) is navigation-only.** It lists *no books*, only a few links
   to bounded subsections (Recently Added, All Books, Search). So the entry point is tiny
   regardless of library size.
2. **Every acquisition feed is capped at `PageSize` (currently 30) entries**, with
   `<link rel="next">` / `<link rel="previous">` so the device pulls one bounded page at a
   time. `/opds/all`, `/opds/new`, and search results all funnel through a single
   `acquisitionPage` chokepoint in `internal/opds` that enforces the cap; there is no code
   path that emits an uncapped book list. The cap fetches `PageSize+1` rows to detect
   whether a next page exists, then trims.

`PageSize` is intentionally conservative; tune it **only** after testing on the real
device. Do not remove the cap as an "optimization"; it is the contract with the hardware.

**Verified (2026-06-15):** seeded 35 books, `/opds/all` returned exactly 30 entries with a
`next` link to `?page=1`; `?page=1` returned the remaining 5 with a `previous` link and no
`next`. The X4 therefore never receives more than 30 entries per request.

### 5.5 On-disk layout, scan, prune, reorganize

Books live under `/data/library` organized as **`Author/Title.epub`** (first author;
no-author books go under `Unknown Author`). Names are filename-sanitized, and a
collision gets a `(2)` suffix. The catalog `path` column stores this relative path.

Three catalog operations keep the DB and disk in sync:

- **Scan** (`POST /api/scan`, or on boot): walk `/data/library` (recursively, so the
  `Author/` subdirs are covered), hash files, insert new / update changed. Idempotent
  by path+hash. Then **prune**.
- **Prune** (runs at the end of every scan): delete catalog rows whose file no longer
  exists on disk, so removing a book from `/data/library` removes it from the catalog
  (join rows go via `ON DELETE CASCADE`). This is what keeps a deleted file from
  leaving a dangling, 404-ing row.
- **Reorganize** (`-reorganize` flag on boot): move any book not already at its
  canonical `Author/Title.epub` path there and update the stored path. Used to migrate
  a previously-flat library; a no-op once everything is in place.

Both ingest paths (scan of an existing folder, and the §4 import pipeline) funnel
through the same indexer, and the import pipeline writes new books directly to the
`Author/Title.epub` location.

Listing order: the library view (web `/`, OPDS `/opds/all`) sorts by **author, then
title**; the OPDS "Recently Added" feed sorts newest-first.

---

## 6. Deployment

```yaml
# docker-compose.yml (sketch)
services:
  library:
    build: ./library          # Go binary, scratch/distroless base
    ports: ["8080:8080"]
    volumes:
      - ./data:/data           # books, catalog.db, import/
    environment:
      - LIBRARY_DATA=/data
      - DRM_SIDECAR_IMAGE=library-drm-sidecar
    # mounts the docker socket OR talks to sidecar via a tiny exec helper;
    # see §6.1 for the "Go runs docker run" boundary.
  # sidecar is NOT a long-running service; it's `docker run --rm`'d on demand.
```

### 6.1 How Go invokes the sidecar (the one design wrinkle)

A container shouldn't casually mount the Docker socket. Two clean options:

- **(a) Sidecar as a tiny always-on HTTP worker** on the internal compose network: Go POSTs
  a job (`{op:"fulfill"|"decrypt", file}`), it shells the Python script and returns the
  output path. No socket mount; clearest boundary. Slightly more than "run --rm" but
  trivial (a 30-line Flask/`http.server` wrapper).
- **(b) Bundle the Python tools into the same image** behind a subprocess call. Simplest to
  run, but reintroduces Python into the "Go" container we wanted pure.

**Recommendation: (a).** Keeps Python fully quarantined, no socket, clean job contract. The
sidecar is idle ~all the time (DRM import is occasional), so an always-on worker costs
nothing meaningful.

### 6.2 Running under Podman (the deployment target)

The host runs **rootless Podman**, not Docker. The compose file is Docker-compatible, but
three Podman realities are designed in. They are not cosmetic; (1) is load-bearing for
the import pipeline.

1. **Rootless UID mapping → `userns_mode: keep-id` (the one that bites).** Rootless Podman
   runs containers in a user namespace that remaps the container's user to a *high
   subordinate UID* on the host (e.g. container UID 0 → host UID 100000+). Our pipeline
   hands files between two containers and the host: the sidecar writes a clean EPUB into the
   shared `data/` volume, the Go service moves and indexes it, and the host user reads/drops
   files. Without `keep-id`, each side writes files owned by a UID the others can't touch,
   and the hand-off silently fails with permission errors. `keep-id` maps the host UID
   straight through both containers so all three agree on ownership. The one-time
   `setup.py` run must also pass `--userns=keep-id`, or the secrets it writes to `/secrets`
   end up owned by a remapped UID.

2. **Service-name DNS → explicit bridge network.** The Go service reaches the sidecar at
   `http://drm-sidecar:7000`. Container-to-container name resolution is provided by
   `aardvark-dns`, which is reliable on an explicitly-declared custom network. Some rootless
   Podman / podman-compose version combos have had DNS regressions on the *implicit* default
   network, so we declare `libnet` and attach both services to it.

3. **SELinux relabeling → `:z` / `:Z` mount suffixes.** On Fedora/RHEL hosts, bind mounts
   need an SELinux relabel or the container can't read them. `:Z` (private) is used for
   `secrets/` since only the sidecar touches it; `:z` (shared) is used for `data/` since
   both containers mount it. On non-SELinux hosts the suffixes are harmless no-ops.

Everything else (`podman compose up`, image build, ports) mirrors Docker. The
socket-mount concern from §6.1 never arises because we chose the always-on worker (option
a), so no container needs the Podman socket.

---

## 7. Build order

1. **Go skeleton + SQLite + EPUB metadata scan** of a real folder → catalog populated.
   Verify against actual EPUBs from the library, not synthetic zips.
2. **OPDS feed** → point the **actual X4** at it; confirm it browses, searches, downloads
   over WiFi. Do this *early*: the whole device story rests on Crosspoint accepting our
   feed, and a spec is not a guarantee.
3. **Browser reader UI** (catalog grid + epub.js + read-position persistence).
4. **DRM sidecar + import watcher** last: most annoying to set up, occasional in use.
   Validate end-to-end with one real `.acsm` from a defunct store and one real ADEPT EPUB.

---

## 8. Risks & realities

- **DeDRM/ADEPT is a back-catalog tool, not a future pipeline.** It works on the legacy
  ADEPT files you already own from dead stores; it is not a forward-looking system. This
  matches the stated use case (mostly-clean library + occasional legacy import).
- **One Adobe authorization decrypts only its own books.** Files fulfilled to a *different*
  Adobe account need that account's `.der`. Multi-account legacy collections need multiple
  keys in the pipeline; v1 assumes one.
- **`.acsm` files expire.** Fulfillment must happen reasonably soon after download.
- **X4 OPDS quirks unknown until tested.** Hence step 2's early hardware check.
- **Legal/personal:** DRM removal of content you own, for personal device interop, on a
  LAN-only host. Keep `/secrets` private; don't expose the service publicly in v1.

---

## 9. Resolved decisions and future work

Decisions settled during implementation:

1. **Sidecar boundary (§6.1):** always-on worker (option a). Python stays fully
   quarantined, no Docker socket, clean job contract.
2. **`.acsm` entry point:** both supported, via dropping a file into `import/`, or upload
   via the web UI; both converge on the same pipeline.
3. **Adobe accounts:** single account. A multi-account legacy collection would need
   one `.der` per account in the pipeline; out of scope for v1.

Open for later:

- **Cover/metadata enrichment:** EPUB-embedded metadata only in v1. Fetching from an
  external source (Google Books / OpenLibrary) for sparse files is a possible
  enhancement.
- **X4 hardware validation:** the OPDS feed is spec-correct and paged, but has not
  yet been consumed by a real Crosspoint device.
