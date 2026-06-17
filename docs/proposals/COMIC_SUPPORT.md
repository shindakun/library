# Proposal: CBZ/CBR comic support

Status: **proposed, not implemented.** Design + build plan for adding comic
books (CBZ/CBR) alongside EPUBs: import them, catalog them, show covers, read
them in the browser, and serve them over OPDS. The import *flow* is unchanged
(watch, verify, name, move, index, archive); the work is making the catalog and
reader format-aware.

## 0. Decisions locked (read first)

These were open in earlier drafts; they are now decided and the rest of the doc
assumes them:

- **CBR strategy:** convert CBR -> CBZ at import, using the **pure-Go
  `nwaples/rardecode`** module (vendored). Keeps the project's no-cgo /
  no-external-binary property; no `unrar` in the image. Caveat: rardecode does
  not handle encrypted or some exotic RAR variants; such a CBR fails the import
  with a clear error (and lands in `import/failed/`), which is acceptable.
- **Build scope:** **CBZ end-to-end first** (schema -> `internal/comic` ->
  import -> reader -> OPDS), all working and verifiable, THEN add CBR conversion
  on top. CBR is the only hard part; de-risk everything else first.
- **Branch:** built on a fresh `comics` cut from `main` AFTER the import-progress
  feature (v0.2.0) landed, so the CBR conversion step reports real progress.

### What v0.2.0 already gives us (do not rebuild)

The import-progress feature shipped the generic progress plumbing this proposal's
CBR step needs. `internal/ingest` already has
`type progressFunc func(step string, frac float64, detail string)`, `pipeline()`
takes an `onProgress progressFunc`, and `handle()` wires it to the job registry
-> SSE -> the `<progress>` bar on `/imports`. A CBR converter does NOT add any
progress machinery; it just calls
`onProgress("converting", pagesDone/float64(total), "page 142/610")` inside its
extract loop and the bar fills end to end. This is exactly why import-progress
was built first.

## 1. What a comic is, and the one real wrinkle

A comic archive is just **a folder of page images** (JPEG/PNG/WebP), one per
page, named so they sort in reading order. Two container formats:

- **CBZ = a ZIP** of those images. Go's stdlib `archive/zip` reads it with zero
  dependencies, exactly like an EPUB. Easy.
- **CBR = a RAR** of those images. This is the wrinkle: Go has **no stdlib RAR**
  reader, and RAR is a patent-encumbered, proprietary format. Three options
  (§4.2); the recommendation is to **convert CBR to CBZ on import** so the rest
  of the system only ever deals with ZIPs.

There is no embedded metadata standard like the EPUB OPF. The de-facto standard
is an optional **`ComicInfo.xml`** at the archive root (the ComicRack schema:
Series, Number, Title, Writer, Year, etc.). Most comics have only a filename.

## 2. The architectural finding (where "mostly the same" is NOT true)

The import flow is genuinely reusable, but the catalog is **hardcoded to EPUB**:
`catalog.Index` unconditionally calls `epub.Read()` for metadata and
`epub.CoverImage()` for the cover, and there is **no format column**. So the two
load-bearing changes are:

1. A **format discriminator** on each book (epub vs cbz), persisted.
2. `Index` (and the cover handler, and the reader route) **branch on format**
   instead of assuming EPUB.

Everything else (the fsnotify+poll watcher, dedup by content hash, the
`Author/Title.ext` layout, slug-as-content-hash, the cover cache, OPDS paging,
the archive-to-done disposition) carries over unchanged.

**Metadata-shape gap:** `Index` -> `upsertBook` is typed directly to
`*epub.Metadata` (and `books_fts` + the author/series/identifier joins read its
fields). `comic.Read` returns a different struct. Rather than thread two types
through, define `comic.Metadata` with the **same field names** the catalog reads
(Title, Authors, Series, SeriesIndex, Language, Publisher, Description,
Published, HasCover, Identifiers) so a thin adapter maps it onto the existing
`upsertBook` path with no signature churn. Comics simply leave the epub-only
fields (Publisher, Identifiers, Description) empty.

## 3. Schema + model changes

```sql
-- Discriminates how to parse/serve a book. "epub" for existing rows
-- (backfilled by extension on the next scan), "cbz" for comics.
ALTER TABLE books ADD COLUMN format TEXT NOT NULL DEFAULT 'epub';
```

**Migration gap (found while building, not in the original draft):** `migrate()`
today is a single `db.Exec(schema)` where `schema` is all
`CREATE TABLE IF NOT EXISTS`, i.e. idempotent. A bare `ALTER TABLE ... ADD
COLUMN` is **not** idempotent: it errors with "duplicate column" on every run
after the first and would break startup. So:

- Fresh DBs: add `format TEXT NOT NULL DEFAULT 'epub'` directly in the `books`
  `CREATE TABLE` (covers new installs with no ALTER).
- Existing DBs: guard the `ALTER` by checking `PRAGMA table_info(books)` for the
  column first, and only `ALTER` when absent. This is the project's first real
  schema migration; keep it a tiny helper (`addColumnIfMissing`) rather than
  pulling in a migration framework.

Existing rows default to `epub`, which is correct (everything imported so far is
an epub), so no data backfill pass is needed beyond the column default.

`Book` gains a `Format string` field. Touch points for that field are wider than
"Index branches": the explicit `SELECT ... FROM books` hydrate list and its
`Scan`, the `INSERT` and `UPDATE` in `upsertBook`, the `file` download handler
(Content-Type + filename extension), and the OPDS `bookEntry` acquisition type
all read or write format and must be updated together (§5-§7).

The on-disk layout becomes `Author/Title.<ext>` where ext is `.epub` or `.cbz`
(`fileutil.LibraryRelPath` hardcodes `.epub` today; extend it to take the
extension). The **slug stays the content hash**,
so comic URLs are stable like book URLs, and dedup-by-hash works as-is.

## 4. The comic package (`internal/comic`)

A sibling to `internal/epub`, same shape: parse metadata + cover from the
archive, no third-party deps for the CBZ path.

### 4.1 Reading a CBZ

```go
package comic

// Metadata is what the catalog needs (parallels epub.Metadata).
type Metadata struct {
    Title    string
    Series   string
    Number   string   // issue number, if known
    Authors  []string // writer(s) from ComicInfo.xml
    PageCount int
    HasCover bool
}

// Read opens a .cbz (a ZIP), counts image pages, and pulls metadata from
// ComicInfo.xml if present, else derives Title from the filename.
func Read(path string) (*Metadata, error)

// Pages returns the sorted list of image entry names inside the archive (the
// reading order). Used by the reader to page through.
func Pages(path string) ([]string, error)

// PageImage returns one page's bytes + mime by index (the reader fetches these).
func PageImage(path string, index int) ([]byte, string, error)

// CoverImage returns the first page (sorted) as the cover, parallel to
// epub.CoverImage, so the existing cover cache works unchanged.
func CoverImage(path string) ([]byte, string, error)
```

Page ordering: sort entry names with a natural/numeric-aware comparison
(`page2.jpg` before `page10.jpg`), filtering to image extensions and skipping
`ComicInfo.xml` and directories. This is the one fiddly bit; get it right and
test it (zero-padded vs not, nested folders).

### 4.2 The CBR question (DECIDED: convert on import, pure-Go rardecode)

Decision recorded in §0. For the record, the options that were weighed:

| Option | How | Trade-off |
| --- | --- | --- |
| **Convert to CBZ on import (CHOSEN)** | At import, detect CBR, extract with `nwaples/rardecode` (pure Go), re-zip to CBZ, keep only the CBZ | Rest of the system only ever sees ZIPs; one-time cost at import; no external binary; fails clearly on encrypted/exotic RAR |
| Read CBR live | A `comic` reader that handles RAR on every read | No conversion, but cover/page paths all handle RAR; reader stops being pure-ZIP |
| Reject CBR | Only accept CBZ | Zero work, pushes converting onto the user |

Why convert-on-import with `rardecode`: it confines RAR to one spot (the import
step), keeps the catalog/reader/cover/OPDS paths pure-ZIP, stays pure-Go (no cgo,
no `unrar` binary, no image bloat), and matches the "normalize at the boundary"
pattern the DRM pipeline already uses (fulfill + decrypt produce a clean
artifact; CBR->CBZ produces a clean artifact). Comics have no DRM, so this does
**not** touch the Python sidecar. The conversion runs inside `pipeline()` and
reports progress via the existing `onProgress("converting", done/total, ...)`
hook (§0). Keep the original CBR out of the library; archive it to `done/` like
any other source. A CBR `rardecode` cannot read (encrypted/exotic) fails the
import with a clear error and lands in `import/failed/`.

## 5. Import flow (what changes, what does not)

`importable()` gains `.cbz` and `.cbr`. The pipeline branches early on format:

```text
*.epub / *.acsm   -> existing DRM/direct path (unchanged)
*.cbz             -> verify it is a valid ZIP of images -> import
*.cbr             -> convert to .cbz (extract + re-zip) -> verify -> import
```

The shared tail is identical to today: verify (parse the archive), dedup by
content hash, name `Author/Title.cbz`, move into the library, index, archive the
original to `done/`. The DRM sidecar is never touched for comics (they are
DRM-free, like the existing direct-epub-import branch).

**Gaps found in `handle()`/`pipeline()` while building (step 3):**

- `pipeline()`'s non-acsm branch unconditionally calls
  `epub.IsADEPTEncrypted(hostPath)`. A `.cbz` is a ZIP but not an epub; that call
  errors. So `pipeline()` must branch on format **first**: a comic is a pure
  passthrough (return hostPath unchanged, sidecar untouched), exactly like a
  DRM-free epub but without the epub inspection.
- `handle()`'s verify step hardcodes `epub.Read(cleanHostPath)` to both validate
  the file and get `Authors`/`Title` for the destination name. For a `.cbz` this
  must be `comic.Read`. Both return the same field names (by design), but they
  are different types, so verify branches on format and yields just the
  `(authors []string, title string)` the destination naming needs.
- `LibraryRelPath` hardcodes `.epub`; the comic destination must end `.cbz`. It
  now takes the extension.
- `importable()` and `sourceFor()` are epub/acsm-only; add `.cbz`
  (`sourceFor` -> "comic-import").

`catalog.Index` branches: `.epub` -> `epub.Read`/`epub.CoverImage` as now;
`.cbz` -> `comic.Read`/`comic.CoverImage` (via a small format-neutral metadata
adapter so `upsertBook` is unchanged). `cacheCover` likewise branches on the
file extension. It sets the `format` column from `formatForPath` (already in
place from step 1). The cover cache is otherwise unchanged: it keys on slug and
stores whatever bytes the format's `CoverImage` returns.

## 6. The reader (browser)

epub.js cannot render comics. Add a separate, dependency-free **image-sequence
viewer** served at the existing reader route, chosen by format:

- `GET /read/{slug}` renders `reader.html` for epubs (epub.js) or a new
  `comic.html` for cbz, based on `book.Format`.
- New endpoints for the comic viewer:
  - `GET /book/{slug}/pages` -> JSON `{count: N}` (page count).
  - `GET /book/{slug}/page/{n}` -> the nth page image (0-based), served straight
    from the archive via `comic.PageImage` with the right image mime.
- `comic.js` (matching the css/js/vendor split): a minimal pager (prev/next,
  keyboard arrows, fit-width/fit-height, page indicator). Reuses the dark-mode
  toggle from `app.js`. No framework; this is a handful of `<img>` swaps.

**Gaps found wiring the reader (step 4):**

- `reader()` always renders `reader.html`; it must branch on `book.Format`
  (epub -> `reader.html`, cbz -> `comic.html`).
- `file()` (`GET /book/{slug}/file`) hardcodes `application/epub+zip` and a
  `.epub` download name. It must serve `application/vnd.comicbook+zip` / `.cbz`
  for comics (also needed by OPDS in §7). Branch on format.
- `cover()`'s cache-MISS path hardcodes `epub.CoverImage`, so a comic indexed
  before its cover was cached would 404. Use a format-neutral cover extractor
  (exported from the catalog) instead.
- Reading-position persistence: there is a `PUT /api/books/{slug}/read`
  (`{percent, cfi}`) but NO GET to read it back; the epub reader only saves,
  never restores. For comics, restoring the page matters more. Store the page
  index in `read_state` (page number in `cfi` as a string, `percent` =
  page/count) and surface the saved page to `comic.html` at render time (embed
  it in the page like `BOOK_SLUG`), so no new GET endpoint is required.

Reading-position persistence reuses the existing `read_state` table (page number
in `cfi`, fractional `percent`); no schema change.

## 7. OPDS

Comics are first-class in OPDS. The acquisition link uses the comic media type:

- `type="application/vnd.comicbook+zip"` for the `.cbz` acquisition link
  (instead of `application/epub+zip`).

The feed code already builds entries per book; it just needs to emit the right
`type` based on `book.Format`. Comic-aware OPDS clients (Chunky, Panels,
KOReader, etc.) then download them. **Unverified:** whether the Xteink X4 /
Crosspoint reads comics over OPDS at all (the X4 is an e-ink e-reader; CBZ
support varies). Treat browser reading as the primary comic surface and flag X4
comic support as to-be-tested.

## 8. Performance note (covers + pages)

The cover cache already saves re-opening the archive per grid view. For the
reader, decompressing a large CBZ on every page request is wasteful; an optional
optimization is a **per-comic page cache** (extract pages to
`data/comics/<slug>/` on first open, serve flat files, like the cover cache).
Start without it (serve pages straight from the ZIP); add it if reading feels
slow. Keep it a derived cache (safe to wipe), consistent with `data/covers`.

## 9. Build order

**CBZ end-to-end first (steps 1-5), then CBR (step 6).** Each step is
independently verifiable; CBR is isolated last because it is the only hard part.

1. (DONE) Schema `format` column + `Book.Format`; backfill existing rows to
   `epub` by extension on scan. Teach `Index` to branch on format (epub vs cbz).
   The on-disk layout becomes `Author/Title.<ext>` (extend `fileutil.LibraryRelPath`
   to take the extension).
2. (DONE) `internal/comic`: `Read`, `Pages`, `PageImage`, `CoverImage` for CBZ.
   Unit tests with synthetic CBZ fixtures (a zip of tiny images + a ComicInfo.xml
   case and a filename-only case). Nail the page-ordering sort (natural/numeric,
   zero-padded vs not, nested dirs, skip non-image + `ComicInfo.xml`).
3. (DONE) Import: `importable()` accepts `.cbz`; the pipeline routes a `.cbz`
   through a no-DRM branch into the shared tail (verify -> dedup -> name -> move
   -> index -> archive). Verified end to end (`TestImportComicEndToEnd`). No
   sidecar.
4. (DONE) Reader: `reader()` selects `comic.html` for cbz; the comic viewer
   endpoints `GET /book/{slug}/pages` (count) and `GET /book/{slug}/page/{n}`
   (image) serve straight from the archive; `comic.js` is a dependency-free pager
   (prev/next, arrow keys, tap-thirds, fit width/height, page indicator) that
   persists the page via the existing read endpoint (page number in `cfi`) and
   restores it on open (embedded as `START_PAGE`). `file()` now serves
   `application/vnd.comicbook+zip` / `.cbz` for comics, and the cover cache-miss
   path is format-neutral. Verified: comic template renders (not epub), `/pages`
   count, `/page/{n}` bytes + mime, out-of-range 404, comic download media type.
   Browser behavior (paging, fit, resume) is for the user to confirm.
5. (DONE) OPDS: `bookEntry` emits `application/vnd.comicbook+zip` on the
   acquisition link for cbz (via `acquisitionType(format)`), so comic-aware
   clients recognize it. GAP found here (not in earlier drafts): `catalog.Scan`
   filtered to `.epub` ONLY, so a rescan (or a CBZ dropped straight into the
   library) never cataloged comics, even though the import path indexed them via
   `Index` directly. The lesson: a "single funnel" (`Index`) can still be gated
   by each caller's own pre-filter; check the callers, not just the funnel. Fixed
   with a shared `indexableExt` (epub + cbz). Tests: comic acquisition type;
   `TestScanIndexesComics`.
6. CBR: add `.cbr` to `importable()`; convert CBR -> CBZ inside `pipeline()`
   using `nwaples/rardecode`, reporting progress through the existing
   `onProgress("converting", done/total, ...)` hook so the `/imports` bar fills.
   The converted CBZ flows through the same tail as a native CBZ. Verify with a
   real CBR (watch the progress bar). Confirm an encrypted/unreadable CBR fails
   cleanly into `import/failed/`.
7. (Optional) per-comic page cache if reading is slow.

## 10. Risks / notes

- **CBR/RAR is the only hard part.** Everything else is "EPUB, but the parser is
  a different package." Decide §4.2 before building; convert-on-import keeps the
  blast radius to the import step.
- **Page-ordering correctness** is the subtle bug surface (numeric sort, nested
  dirs, non-image entries). Test it hard.
- **No DRM for comics** -> the sidecar is irrelevant; do not route comics through
  fulfill/decrypt.
- **X4 comic support is unverified** -> browser is the primary reading surface
  for comics until tested on hardware.
- **Stay dependency-free where possible.** CBZ needs nothing beyond stdlib; only
  CBR pulls in a RAR extractor, and only at the import boundary.
- Comics often lack clean author/title metadata; filename parsing plus optional
  ComicInfo.xml is the realistic source. Metadata editing (the other proposal)
  would let the user fix it.
