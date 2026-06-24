<p align="center">
  <img src="assets/library.png" alt="library" width="200">
</p>

# library

A self-hosted ebook (and comic) library: browser reader + an OPDS feed for any
e-reader (verified on the **Xteink X4** running Crosspoint firmware), with an
import pipeline that fulfills Adobe `.acsm` files and strips legacy ADEPT DRM on
the way in. Reads EPUBs and comic archives (CBZ; CBR is converted to CBZ at
import), with a dedicated in-browser comic reader.

See [docs/DESIGN.md](docs/DESIGN.md) for the full design and rationale, and
[docs/DEPLOY.md](docs/DEPLOY.md) for deploying the compose stack on any Docker
host (with notes for a Proxmox LXC). The [docs/](docs/) index lists everything,
including [docs/proposals/](docs/proposals/) for designed-but-unbuilt features.

## Architecture (two containers)

- **`library`**: Go service (single static binary). Catalog (SQLite), browser
  reader (epub.js for books, a built-in image-sequence viewer for comics), OPDS
  1.2 feed, import watcher (the `ingest` package), upload endpoint. Never imports
  Python.
- **`ebook-sidecar`** (optional): quarantined Python worker. Runs
  `acsm-calibre-plugin` (fulfillment) + DeDRM's `ineptepub` (decryption). Reads
  `/secrets` for the Adobe activation + key, and writes it only during first-run
  setup.
- **`audiobook-sidecar`** (optional): quarantined ffmpeg worker that removes
  Audible DRM from a `.aax` (decode with the account activation bytes, copy the
  audio + chapters to a clean `.m4b`). Holds the activation bytes in `/secrets`.
  Setup stores pasted activation bytes (`make audiobook-setup BYTES=...`, or the
  web form). The Go-side import wiring + player are still being built; see
  [docs/proposals/AUDIOBOOK_SUPPORT.md](docs/proposals/AUDIOBOOK_SUPPORT.md).

Each sidecar is **only** needed for its DRM'd content and is independently
optional: the ebook sidecar for `.acsm` loans and ADEPT-encrypted `.epub`, the
audiobook sidecar for `.aax`. Run with one, the other, both, or neither. With a
sidecar disabled (empty `EBOOK_SIDECAR_URL` / `AUDIOBOOK_SIDECAR_URL`), its setup
form is hidden, it is not probed, and its DRM'd inputs are rejected into
`import/failed/` with a clear reason; comics (`.cbz`/`.cbr`) and DRM-free epubs
always import without any sidecar.

Any OPDS client points at `http://<host>:8080/opds` to browse and download over
WiFi (the Xteink X4 is the verified test device, but it is standard OPDS 1.2).
**Feeds are always paged** (30/page) so a memory-constrained reader never
receives an unbounded feed it could choke on (see docs/DESIGN.md §5.4.1).

## Layout

```text
Makefile             drives everything from the repo root (see `make help`)
cmd/library/         main: wires catalog + web + opds + ingest together
internal/epub/       EPUB metadata, cover, ADEPT-DRM detection (zip + OPF, no deps)
internal/comic/      CBZ reader + CBR->CBZ convert (pure-Go rardecode, no cgo)
internal/catalog/    SQLite catalog: index, scan, prune, query (sorts by author>title)
internal/opds/       OPDS 1.2 feed (paging enforced here)
internal/web/        HTTP only: browser UI, reader, upload, file/cover, JSON API
internal/ingest/     import watcher + DRM/comic pipeline (fulfill/decrypt/convert -> index)
internal/drm/        Go client that drives the sidecar
internal/fileutil/   small shared filesystem helpers
docker/              container stack (compose + Dockerfiles)
  docker-compose.yml two-service stack; paths are relative to docker/
  Dockerfile         Go service image (distroless, ~30 MB)
  ebook-sidecar/     Python ebook-DRM worker (worker.py) + CLI setup (setup.py)
data/library/        clean EPUBs + CBZ comics (the library); sorted by author then title
data/covers/         extracted cover-image cache, keyed by slug (derived, safe to wipe)
  covers/overrides/  user-uploaded cover overrides, keyed by slug (authoritative)
data/import/         drop or upload .acsm / .epub / .cbz / .cbr here -> pipeline -> library
  import/work/       sidecar + CBR-convert scratch (NOT watched)
  import/done/       originals archived here on success
  import/failed/     originals here on failure, with a .log sibling
secrets/             Adobe activation + .der key (gitignored, never committed)
```

## Quick start (Makefile)

Everything is driven from the repo root via `make`. Run `make help` for the full
list; the common ones:

```sh
# Develop
make build           # build the version-stamped Go binary to ./bin/library
make test            # run Go tests
make check           # vet + lint + test (mirrors CI)
make lint            # golangci-lint
make hooks           # install the git pre-commit hook (gofmt/vet/lint/markdownlint)

# Run the local stack (dev: builds images, macOS/Podman)
make ebook-setup     # ONE-TIME: authorize Adobe + export key into ./secrets (or use the web form)
make audiobook-setup BYTES=...  # ONE-TIME: store Audible activation bytes (or use the web form)
make up              # build images + start the stack (LAN IP auto-detected)
make ps              # stack status
make logs            # follow logs
make down            # stop + remove the stack
make time-sync       # fix the Podman VM clock if ADEPT fulfillment fails (see below)

# Release + production (see docs/DEPLOY.md)
make release VERSION=v0.1.0   # tag + push; CI builds GHCR images and cuts the release
make prod-up                  # start the prod stack from GHCR images
make prod-deploy              # pull newest images and restart in place
```

`make up` auto-detects your LAN IP and bakes it into the OPDS links so e-readers
can reach them. Override with `make up LIBRARY_BASE_URL=http://192.168.1.20:8080`.

After `make up`, point an OPDS client (the X4 or another e-reader) at
`http://<lan-ip>:8080/opds`. The browser library is at `http://<lan-ip>:8080/`.

## Run locally without containers (catalog + reader + OPDS, no DRM)

```sh
make run             # go run on the host; serves :8080
# drop .epub files in data/library/, then `curl -XPOST :8080/api/scan` or restart
```

DRM imports need the sidecar; use `make up` for the full stack.

## Local development on Podman (macOS, via `docker compose`)

The stack runs on any Docker host (the author's is a Proxmox LXC; see
[docs/DEPLOY.md](docs/DEPLOY.md)). **Local development** runs the same compose
stack under rootless Podman on macOS, which has a few practical wrinkles:

- Use **`docker compose`**, not `podman compose`. On this setup `podman compose`
  delegates to Docker Desktop's compose plugin, which can't reach a daemon and
  fails. `docker compose` talks to Podman through the Docker-compatible socket at
  `/var/run/docker.sock` (symlinked to the Podman machine socket) and works. The
  Makefile uses `docker compose` by default; override with `make up COMPOSE=...`.

Three Podman realities are baked into `docker/docker-compose.yml`:

1. **`userns_mode: keep-id`** on both services. Rootless Podman remaps the
   container user to a high subordinate UID on the host. Without `keep-id`, files
   handed between the sidecar, the Go service, and you on the host get owned by a
   UID the others can't touch, breaking the import pipeline. `keep-id` maps your
   host UID straight through so all three agree on ownership.
2. **Explicit `libnet` bridge network** so `aardvark-dns` resolves the service
   name `ebook-sidecar` reliably (some rootless default-network combos have flaky
   DNS).
3. **SELinux relabel suffixes** `:z` (shared) / `:Z` (private) on bind mounts, for
   Fedora/RHEL hosts. Harmless no-ops on non-SELinux systems.

### Podman VM clock drift (ADEPT "request expired")

The Podman machine VM's clock drifts behind real time when the Mac sleeps. Fedora
CoreOS ships chrony with `makestep 1.0 3` (step only the first 3 updates), so after
a large sleep-induced jump it refuses to correct and the container runs minutes
behind. Adobe then rejects fulfillment with **`E_ADEPT_REQUEST_EXPIRED`** (the
signed request looks stale; it is *not* a login or credential problem).

`make up` runs `check-clock` and warns if the VM is skewed. To fix it:

```sh
make time-sync       # installs a chrony drop-in (always-step) + forces a resync
```

This persists across `podman machine stop/start` (but not across `podman machine
rm`, so re-run it after recreating the machine).

## Importing books

Three ways in; all converge on the same pipeline and catalog:

- **Upload via the web UI**: the dedicated import page (`/imports`, linked from
  the library header) accepts `.acsm`, `.epub`, `.cbz`, or `.cbr`, and shows
  live per-file progress over SSE. The file is staged atomically into
  `data/import/`.
- **Drop a file** into `data/import/` directly.
- Either way, the watcher (fsnotify + a 5s polling fallback; the poll covers
  hosts where inotify events do not cross the bind mount, e.g. the macOS Podman
  VM) runs the right path:

| Dropped/uploaded        | Pipeline                                              |
|-------------------------|-------------------------------------------------------|
| `.acsm` (Adobe loan)    | fulfill (Adobe) → ADEPT epub → decrypt → library      |
| `.epub` with ADEPT DRM  | decrypt → library                                     |
| `.epub`, DRM-free       | imported directly (sidecar never touched)             |
| `.cbz` comic            | imported directly (sidecar never touched)             |
| `.cbr` comic            | converted to `.cbz` → library (sidecar never touched) |

The clean file is verified as parseable, named from its title (e.g.
`Book Title.epub` / `Comic Title.cbz`), and indexed. Comics carry no DRM, so the
sidecar is never touched; a `.cbr` is converted to a `.cbz` (pure-Go RAR
extract + re-zip) and only the `.cbz` enters the library. Originals from the DRM
and CBR paths archive to `import/done/`; failures go to `import/failed/` with a
`.log`. The scratch in `import/work/` is wiped after each job. **`.acsm` files
are time-sensitive:** library loans carry an `<expiration>`; fulfill before then.

### Removing books

Delete the `.epub` from `data/library/` and the catalog self-corrects: the next scan
(on startup, or `curl -XPOST :8080/api/scan`) prunes any row whose file is gone, so
the catalog always matches what's on disk. There is no separate "delete from DB"
step.

## Tests

```sh
make test            # go test ./...
go test -race ./internal/ingest/ ./internal/catalog/   # concurrency-sensitive paths
```

The suite is hermetic: it builds synthetic EPUBs and CBZs (real zips) and temp
SQLite DBs in-test, so it needs no fixtures, network, or running sidecar. Coverage
focuses on the logic that has bitten us or that protects the device/data:

- **catalog**: scan/index idempotency (epub + cbz), `format`-column migration,
  **prune** of deleted files (incl. cascade of join rows), author>title sort, FTS.
- **opds**: the paging invariant (protects memory-constrained clients): feeds
  capped at `PageSize`, correct next/prev boundaries, root-is-navigation-only,
  comic acquisition media type.
- **epub**: metadata parse, ADEPT-DRM detection (incl. *not* flagging IDPF font
  obfuscation), identifier/scheme guessing.
- **comic**: CBZ metadata + natural page-ordering (zero-padded, nested, scrambled),
  ComicInfo.xml vs filename fallback, CBR->CBZ convert (real-RAR, ordering, fail-
  clean; skip-guarded on the `rar` CLI).
- **drm**: sidecar client against a mock server: success, non-epub rejection,
  error propagation, unreachable sidecar.
- **web**: upload handler: extension rejection/acceptance (incl. `.cbz`), atomic
  staging, form redirect; import status API + SSE stream.
- **ingest / fileutil**: `uniquePath`, `importable` (incl. comic + dotfile skip),
  the Create+Write dedupe, cross-device move, filename sanitizing, comic import
  end-to-end.

## Status

Working and verified end-to-end via the compose stack:

- **Catalog / reader / OPDS**: EPUB metadata parse, scan/index/prune, OPDS nav +
  paged acquisition feeds (next/prev boundaries verified), cover/file/search.
- **DRM import**: a real OverDrive/Libby `.acsm` fulfilled against Adobe and
  decrypted (ADEPT stripped, content confirmed readable), then indexed.
- **Direct import**: a DRM-free epub imports without touching the sidecar;
  byte-identical duplicates are skipped.
- **Comics**: `.cbz` imports directly and `.cbr` is converted to `.cbz` at import
  (verified on a real 743 MB / 366-page CBR: lossless, correctly ordered); comics
  catalog with covers, read in a built-in image-sequence viewer, and serve over
  OPDS with the comic media type.
- **Web upload**: `.acsm`/`.epub`/`.cbz`/`.cbr` upload routes through the same
  pipeline, with live per-file progress on the `/imports` page (SSE).
- **Browser UI**: grid + sortable table views (persisted), dark mode, clickable
  author search; covers cached to `data/covers` for fast grid loads. Each book
  has a three-dot menu with **Edit** and **Delete**.
- **Metadata editing**: edit a book's title/authors/series/language/publisher/
  description in the browser. Edits save to the catalog instantly, then embed
  into the file in the background (OPF for epubs, `ComicInfo.xml` for comics,
  added if absent) with a live progress bar; a stable slug keeps URLs fixed
  across the rewrite. A scan never clobbers an edited field.
- **Cover override**: upload a replacement cover (kept in
  `data/covers/overrides/`, served in preference to the extracted one) without
  rewriting the book file.
- **Delete**: removes the catalog row, the library file, and any cover, behind a
  confirmation.
- **Library view** sorts by author, then title; "Recently Added" (OPDS) stays
  newest-first.
- **The two-container stack** builds and runs via `make up`; the Go service reaches
  the sidecar by service name over `libnet`.
- **Test suite**: hermetic Go tests across catalog, comic, opds, epub, drm, web,
  ingest (incl. metadata edit/embed, cover override, delete); green under `-race`.

- **Browser reader**: `epub.js` + `jszip` are vendored under
  `internal/web/assets/vendor/` and embedded in the binary; the reader renders
  books, persists reading position, and recolors for dark mode. Comics use a
  built-in, dependency-free image-sequence viewer (prev/next, fit width/height,
  keyboard + tap paging, position persisted to the same read endpoint).

- **Xteink X4**: verified against the real device, it browses the OPDS feed and
  downloads books over WiFi.

Built but not yet verified against a real run:

- **Web first-run setup (Adobe/ebook)**: the form + sidecar `/setup` are wired
  and render correctly, but the Adobe registration path has not been exercised
  end-to-end (the CLI `setup.py` path is the proven one). The **audiobook**
  setup card (paste activation bytes) HAS been verified end-to-end against the
  live stack: a paste-bytes POST wrote `secrets/audible_activation_bytes` and the
  card then disappeared. The audiobook login-retrieval mode is still a stub.
