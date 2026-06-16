<p align="center">
  <img src="assets/library.png" alt="library" width="200">
</p>

# library

A self-hosted ebook library: browser reader + OPDS feed for the **Xteink X4
(Crosspoint firmware)**, with an import pipeline that fulfills Adobe `.acsm`
files and strips legacy ADEPT DRM on the way in.

See [docs/DESIGN.md](docs/DESIGN.md) for the full design and rationale, and
[docs/DEPLOY.md](docs/DEPLOY.md) for deploying the compose stack on any Docker
host (with notes for a Proxmox LXC).

## Architecture (two containers)

- **`library`**: Go service (single static binary). Catalog (SQLite), browser
  reader (epub.js), OPDS 1.2 feed, import watcher (the `ingest` package), upload
  endpoint. Never imports Python.
- **`drm-sidecar`**: quarantined Python worker. Runs `acsm-calibre-plugin`
  (fulfillment) + DeDRM's `ineptepub` (decryption). Touches `/secrets` read-only.

The X4 points its OPDS client at `http://<host>:8080/opds` and browses/downloads
over WiFi. **OPDS feeds are always paged** (30/page) so the device never receives
an unbounded feed it could choke on (see docs/DESIGN.md §5.4.1).

## Layout

```text
Makefile             drives everything from the repo root (see `make help`)
cmd/library/         main: wires catalog + web + opds + ingest together
internal/epub/       EPUB metadata, cover, ADEPT-DRM detection (zip + OPF, no deps)
internal/catalog/    SQLite catalog: index, scan, prune, query (sorts by author>title)
internal/opds/       OPDS 1.2 feed (paging enforced here)
internal/web/        HTTP only: browser UI, reader, upload, file/cover, JSON API
internal/ingest/     import watcher + DRM pipeline (fulfill -> decrypt -> index)
internal/drm/        Go client that drives the sidecar
internal/fileutil/   small shared filesystem helpers
docker/              container stack (compose + Dockerfiles)
  docker-compose.yml two-service stack; paths are relative to docker/
  Dockerfile         Go service image (distroless, ~16 MB)
  sidecar/           Python DRM worker (worker.py) + one-time setup (setup.py)
data/library/        clean EPUBs (the library); sorted by author then title in the UI
data/import/         drop or upload .acsm / .epub here -> pipeline -> library
  import/work/       sidecar scratch (NOT watched)
  import/done/       originals archived here on success
  import/failed/     originals here on failure, with a .log sibling
secrets/             Adobe activation + .der key (gitignored, never committed)
```

## Quick start (Makefile)

Everything is driven from the repo root via `make`. Run `make help` for the list.

```sh
make build           # build the Go binary to ./bin/library
make test            # run Go tests
make drm-setup       # ONE-TIME: authorize Adobe + export key into ./secrets (interactive)
make up              # build images + start the stack (LAN IP auto-detected)
make ps              # stack status
make logs            # follow logs
make down            # stop + remove the stack
make time-sync       # fix the Podman VM clock if ADEPT fulfillment fails (see below)
```

`make up` auto-detects your LAN IP and bakes it into the OPDS links so the X4 can
reach them. Override with `make up LIBRARY_BASE_URL=http://192.168.1.20:8080`.

After `make up`, on the X4: add an OPDS server pointing at
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
   name `drm-sidecar` reliably (some rootless default-network combos have flaky
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

- **Upload via the web UI**: the Import control on the library page accepts
  `.acsm` or `.epub`. The file is staged atomically into `data/import/`.
- **Drop a file** into `data/import/` directly.
- Either way, the watcher (fsnotify + a 5s polling fallback; the poll covers
  hosts where inotify events do not cross the bind mount, e.g. the macOS Podman
  VM) runs the right path:

| Dropped/uploaded        | Pipeline                                            |
|-------------------------|-----------------------------------------------------|
| `.acsm` (Adobe loan)    | fulfill (Adobe) → ADEPT epub → decrypt → library    |
| `.epub` with ADEPT DRM  | decrypt → library                                   |
| `.epub`, DRM-free       | imported directly (sidecar never touched)           |

The clean epub is verified as parseable, named from its title (e.g.
`Book Title.epub`), and indexed. Originals from the DRM path archive to
`import/done/`; failures go to `import/failed/` with a `.log`. The sidecar scratch
in `import/work/` is wiped after each job. **`.acsm` files are time-sensitive:**
library loans carry an `<expiration>`; fulfill before then.

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

The suite is hermetic: it builds synthetic EPUBs (real zips with OPF) and temp
SQLite DBs in-test, so it needs no fixtures, network, or running sidecar. Coverage
focuses on the logic that has bitten us or that protects the device/data:

- **catalog**: scan/index idempotency, **prune** of deleted files (incl. cascade
  of join rows), author>title sort ordering, FTS search.
- **opds**: the X4 paging invariant: feeds capped at `PageSize`, correct
  next/prev boundaries, root-is-navigation-only.
- **epub**: metadata parse, ADEPT-DRM detection (incl. *not* flagging IDPF font
  obfuscation), identifier/scheme guessing.
- **drm**: sidecar client against a mock server: success, non-epub rejection,
  error propagation, unreachable sidecar.
- **web**: upload handler: extension rejection, atomic staging, form redirect.
- **ingest / fileutil**: `uniquePath`, `importable`, the Create+Write dedupe,
  cross-device move, filename sanitizing.

## Status

Working and verified end-to-end via the compose stack:

- **Catalog / reader / OPDS**: EPUB metadata parse, scan/index/prune, OPDS nav +
  paged acquisition feeds (next/prev boundaries verified), cover/file/search.
- **DRM import**: a real OverDrive/Libby `.acsm` fulfilled against Adobe and
  decrypted (ADEPT stripped, content confirmed readable), then indexed.
- **Direct import**: a DRM-free epub imports without touching the sidecar;
  byte-identical duplicates are skipped.
- **Web upload**: `.acsm`/`.epub` upload routes through the same pipeline.
- **Browser UI**: grid + sortable table views (persisted), dark mode, clickable
  author search; covers cached to `data/covers` for fast grid loads.
- **Library view** sorts by author, then title; "Recently Added" (OPDS) stays
  newest-first.
- **The two-container stack** builds and runs via `make up`; the Go service reaches
  the sidecar by service name over `libnet`.
- **Test suite**: hermetic Go tests across catalog, opds, epub, drm, web, ingest;
  green under `-race`.

- **Browser reader**: `epub.js` + `jszip` are vendored under
  `internal/web/assets/vendor/` and embedded in the binary; the reader renders
  books, persists reading position, and recolors for dark mode.

Built but not yet verified against a real run:

- **Web first-run setup**: the form + sidecar `/setup` are wired and render
  correctly, but the Adobe registration path has not been exercised end-to-end
  (the CLI `setup.py` path is the proven one).

Not yet done:

- **Not yet tested on the actual X4**: the OPDS feed is spec-correct and paged,
  but Crosspoint's parser hasn't consumed it from real hardware.
