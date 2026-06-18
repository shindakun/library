# Implementation guide: metadata editing

Status: **implemented** (steps 1-5). Design +
build plan for in-browser metadata editing for both EPUBs and CBZ comics, kept
lightweight (plain HTML forms, a few JSON endpoints, no new frontend
dependencies). Edits land in the DB first, then are embedded into the file
(best-effort, verify-before-swap); a successful embed replaces the original, a
failed one keeps both the original file and the DB edit. The slug stays stable
across embeds (§3). Most comics have no `ComicInfo.xml`, so for them embedding
ADDS the metadata rather than replacing it.

Done: schema + stable slug (1), `catalog.UpdateMetadata` + scan edit-protection
(2), the comic and epub file writers (3a/3b), and the HTTP/UI: the edit form,
`PUT /api/books/{slug}` (DB edit then best-effort embed, reporting embed status),
`catalog.EmbedMetadata` (rewrite-to-temp, verify, swap, rename-on-title-change,
refresh path/hash/size, slug fixed), and the no-JS form fallback (4); cover
override in a separate `data/covers/overrides/` namespace, served in preference
to the extracted cover and validated to decode as an image (5).

## 1. The core tension to resolve first

Today the **EPUB file is the source of truth**. `catalog.Index` reads metadata
out of the epub's OPF on every scan and *overwrites* the catalog row
(`upsertBook` runs `UPDATE books SET title=?, ...`). So a naive "edit the title
in the DB" would be silently clobbered by the next `Scan` (startup, `/api/scan`,
or an import touching the same path).

Any metadata-editing design must answer: **after a user edits a field, where is
the truth, and how does scan avoid stomping it?**

**Model (decided): DB-first, then best-effort embed into the file.** An edit
lands in the catalog immediately (the DB is the display source of truth and scan
must not clobber edited fields), then write-back into the file is attempted
against a copy and swapped in only if it verifies. This is the
"DB-authoritative + write-back" model, but write-back is part of the default
flow, not a deferred power-user action: the user's intent ("edit the book") is to
change the book, and the DB-first step is what makes that safe (a failed rewrite
never loses the edit or corrupts the file). See §3 for the locked decisions
(stable slug, non-destructive embed, display-name-drives-filename) and §7 for the
per-format rewrite mechanics.

The two hard requirements this design must satisfy:

1. **Scan must not stomp edits.** `catalog.Index`/`upsertBook` re-read the file
   on every scan; edited columns are protected via `edited_fields` (§3).
2. **Embed must never corrupt the library.** Rewrite a copy, verify it parses,
   swap atomically, keep the original until the new one is good (§7).

## 2. Editable vs derived fields

From `catalog.Book`:

- **Editable:** Title, Authors, Series, SeriesIndex, Language, Publisher,
  Description, Published, Identifiers (isbn/etc.), Tags (a join table already
  exists in the schema but is unused; editing is where it gets populated).
- **Derived, NOT editable:** Path, FileSize, FileHash, AddedAt, Source, the slug
  (it is the content hash; editing metadata must NOT change it, so book URLs stay
  stable). SortTitle is derived from Title but a manual override is reasonable
  (libraries often want "sort as").

## 3. Schema changes

Additions to `internal/catalog/schema.go` (all via the idempotent
`addColumnIfMissing` helper added for the comics `format` column):

```sql
-- Per-book record of which fields the user has hand-edited, so scan won't
-- overwrite them from the file. JSON array of column names.
ALTER TABLE books ADD COLUMN edited_fields TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN edited_at INTEGER;  -- unix; null = never edited

-- Stable public id. The slug is the content-hash today; embedding edits into the
-- file changes the hash, which would change every edited book's URL/OPDS id.
-- slug_override is set ONCE at import (to the import-time content-hash slug) and
-- never changes, so URLs survive embeds. Book.Slug() prefers it when present.
ALTER TABLE books ADD COLUMN slug_override TEXT;
```

`tags` / `book_tags` already exist; wire them up in the editor.

**Decisions (locked):**

- **Stable slug.** `slug_override` is captured at import and is what `Slug()`
  returns thereafter, so a successful embed (which changes the file hash) does
  NOT change the book's URL or OPDS identity. Bookmarks and e-reader library
  entries survive edits.
- **Embed is best-effort, never destructive.** Edits land in the DB first (the
  DB is the display source of truth). Embedding into the file is then attempted
  against a COPY; only a verified-good rewrite is swapped in. On any failure the
  original file is untouched and the DB edit is kept, surfaced as "saved, not yet
  embedded in file." The library can never be corrupted by an edit.
- **Display name only; filename follows.** There is one editable "title"; the
  on-disk filename is regenerated as `Author/Title.<ext>` on embed (via the
  existing `fileutil.LibraryRelPath`). No separate filename field. If embed
  fails, the DB title leads and the file keeps its old name until a later
  successful embed reconciles it.

A `edited_fields` value like `["title","authors"]` means: on the next scan,
`upsertBook` keeps the DB's title and authors and only refreshes the
non-edited columns from the file. This is the load-bearing change.

## 4. Catalog API

Add to `internal/catalog`:

```go
// Edits is the set of user-editable fields. Pointer/optional semantics so a nil
// field means "leave unchanged"; an empty string means "set to empty".
type Edits struct {
    Title       *string
    SortTitle   *string
    Authors     *[]string
    Series      *string
    SeriesIndex *float64
    Language    *string
    Publisher   *string
    Description *string
    Published   *string
    Tags        *[]string
    Identifiers *map[string]string
}

// UpdateMetadata applies edits to the book, marks the changed columns in
// edited_fields, re-syncs the FTS row, and bumps edited_at. Does NOT touch the
// file or the slug.
func (c *Catalog) UpdateMetadata(ctx context.Context, slug string, e Edits) (*Book, error)
```

Implementation notes:

- Look up by slug (stable id), update only the non-nil fields in a transaction.
- Re-run the authors/series/tags join upserts (reuse `upsertNamed`), exactly like
  `upsertBook` does.
- **Re-sync FTS**: delete + re-insert the `books_fts` row (title/authors/
  description) so search reflects the edit. This is already the pattern at
  `upsertBook`.
- Record each changed column name into `edited_fields`.

**Gaps found building step 2 (the clobber surface is wider than "scalar columns"):**

- **Joins are clobbered too.** `upsertBook` DELETEs and re-inserts
  `book_authors`/`book_series`/`book_tags`/`identifiers` from the file on every
  update. So edit-protection must cover the join-backed fields (authors, series,
  tags, identifiers), not just the scalar `books` columns. Track them in
  `edited_fields` by a stable field name (`"authors"`, `"series"`, `"tags"`,
  `"identifiers"`), and have scan skip the DELETE+reinsert for any edited one.
- **When clobber actually fires:** `Index` early-returns when the file hash is
  unchanged (no clobber on a no-op rescan). The dangerous path is a rescan where
  the file *content changed* on a book the user also edited (re-import, external
  replace). That path must honor `edited_fields`; test it explicitly.
- **One field set, two consumers.** Define the editable field names as constants
  used by BOTH `UpdateMetadata` (to record edits) and the scan path (to skip
  them), so they can't drift.
- **Sanitize at the boundary.** `UpdateMetadata` trims and bounds every string
  (length caps, strip control characters), so neither a filename sink (later
  embed) nor the FTS index ever sees raw/hostile input. The HTTP layer (step 4)
  sanitizes again at the edge, but the catalog does not trust its caller.
- **`sort_title`:** if the user edits Title but not SortTitle, derive SortTitle
  from the new Title (via `sortKey`) and do NOT mark `sort_title` edited, so a
  later title-only edit keeps re-deriving. An explicit SortTitle edit marks it.

### Teach scan not to clobber

In `upsertBook` (the UPDATE branch), before overwriting, read the row's
`edited_fields` and skip those columns. Cleanest: build the UPDATE statement
dynamically from "file fields minus edited fields", or fetch the existing row and
COALESCE edited columns to their current values. Either way, a scan of an
unchanged file must be a no-op for edited books (it already early-returns when the
hash matches; the risk is when the file content changes but the user also edited
metadata, so handle that path explicitly).

## 5. HTTP + UI (lightweight)

### Endpoints (`internal/web`)

```text
GET  /book/{slug}/edit     server-rendered edit form (a new template)
PUT  /api/books/{slug}     apply edits (JSON body matching Edits), returns book
POST /book/{slug}/edit     non-JS fallback: HTML form post -> redirect back
```

Keep it consistent with the existing dual JSON/redirect pattern used by
`apiUpload` (JSON for fetch clients, 303 redirect for plain form posts).

### Form (plain HTML, no deps)

- A new `edit.html` template (or a section on the reader/detail page) with text
  inputs for each editable field. Authors/Tags as comma-separated inputs parsed
  server-side. Description as a `<textarea>`.
- A small `js/edit.js` (matching the css/js/vendor split) that PUTs the form as
  JSON via `fetch` for an inline save, with the HTML form post as the no-JS
  fallback. This mirrors how the upload control already works.
- Link to it from the grid/table (an "Edit" affordance per book) and from the
  reader bar.

No htmx/datastar needed: this is one form and one PUT. If bulk editing across
many books is wanted later, that is the point to reconsider a hypermedia library,
not now.

**Gaps found building step 4 (the embed glue lives in the catalog, not web):**

- **No single-book file-replace+relocate API existed.** `Reorganize` has the
  move+path-update+empty-dir-cleanup pattern but is bulk. Embedding needs a
  single-book operation that: builds the writer input from the edited row, writes
  a temp file, re-parses it to verify, atomically swaps it in (renaming to the new
  `Author/Title.<ext>` if the title changed), and updates `path` + `file_hash`
  (NOT the slug: `slug_override` is fixed). Added `catalog.EmbedMetadata(slug)`
  that orchestrates this and branches on format to the right writer
  (`epub.WriteMetadata` / `comic.WriteComicInfo`).
- **The hash changes, so the cover cache key is unaffected** (cache keys on the
  stable slug), but `file_hash`/`file_size` must be refreshed or a later scan
  sees a "changed" file and re-indexes (harmless but wasteful, and would re-run
  edit-protection). Update both in the same statement as `path`.
- **Embed is best-effort, decoupled from the DB edit, AND asynchronous.**
  `PUT /api/books/{slug}` does the DB `UpdateMetadata` synchronously (fast), then
  runs `EmbedMetadata` in a BACKGROUND goroutine and returns immediately with the
  updated book + a `jobId`. Embedding a large comic re-zips every page (hundreds
  of MB), which would block the request for many seconds and read as a hang, so
  it must not be synchronous. The background embed reports into the existing
  import-job registry (`ingest.Jobs`), and the edit page follows it on the
  existing `/api/imports/stream` SSE feed: a progress bar driven by
  `WriteComicInfo`'s new `onProgress(done, total)` callback (threaded through
  `EmbedMetadata`). Terminal state maps to done / skipped (not embedded, DB edit
  stands) / failed. The background goroutine uses `context.Background()`, not the
  request context, since the response returns first. A failed embed never 500s.
- **Edge sanitization + parsing.** The handler parses authors/tags from
  comma-separated input, rejects an over-large body, and validates the JSON shape;
  the catalog sanitizes again (it does not trust the caller). The slug in the URL
  is used only to look up a row (never touches the filesystem), so it is not a
  path-traversal sink; the regenerated filename comes from the sanitized title
  through `fileutil.LibraryRelPath` (which `SafeFilename`s both segments).
- **Concurrency with the import watcher.** Embedding renames/replaces a library
  file while the watcher may be mid-scan. Both are short SQLite transactions and
  the watcher re-derives from disk; an embed that renames is equivalent to a
  Reorganize move, which already coexists with scans. No new lock needed, but the
  embed reads the row, writes the file, then updates the row in one tx so the DB
  never points at a missing path.

## 6. Cover editing (separate, avoids zip surgery)

Covers are served by `GET /book/{slug}/cover`: from the extracted cache
(`data/covers/<slug>.<ext>`) if present, else extracted live. To allow replacing
a cover WITHOUT rewriting the archive, add a user-set override.

**Gap found building step 5: override vs cache COLLIDE.** The original plan
("store an override at `data/covers/<slug>.jpg`") puts the override at the exact
path the extraction cache uses, so the two are indistinguishable: a `clean-db`
rescan's `cacheCover` would overwrite the user's override with the extracted
cover, and "is this a deliberate override or a derived cache file?" is
unanswerable. They must live in separate namespaces:

- Derived cache: `data/covers/<slug>.<ext>` (safe to wipe, regenerated on scan).
- User override: `data/covers/overrides/<slug>.<ext>` (authoritative, NOT wiped,
  never written by the extractor).

`GET /book/{slug}/cover` resolution order becomes: **override -> cache ->
extract-live**. The cache code (`CoverCachePath`, `cacheCover`, `CacheCoverData`)
must skip the `overrides/` subdir, and never write into it.

- `PUT /book/{slug}/cover`: accept an uploaded image, validate it actually decodes
  as an image (stdlib `image.DecodeConfig`, bound the size), write it to the
  override path. Set `has_cover`. The slug keys the override, so it survives
  re-imports and embeds (slug is stable).
- `DELETE /book/{slug}/cover` (optional): remove the override, falling back to the
  derived cover. Nice-to-have; can defer.

This keeps covers editable without touching the book file, consistent with the
DB-authoritative model. The override is keyed on the stable slug, so an embed that
rewrites the file (changing its hash) does not orphan the override.

## 7. Write-back: embedding edits into the file

Part of the default edit flow (not deferred): after the DB edit lands, embed it
into the file so the change is in the book/comic itself. Both formats follow the
same shape: rewrite the archive to a temp file, validate it parses, atomically
swap, then regenerate the on-disk filename from the new title
(`fileutil.LibraryRelPath`) and update `path`. The slug does NOT change: it comes
from `slug_override` (§3), set at import and stable across embeds. The
catalog-side editor (§1-6) is format-neutral and covers both formats; only this
rewrite step is format-specific.

It runs after the DB write and is best-effort: a failed rewrite leaves the
original file and the DB edit both intact (surface "saved, not yet embedded").

### EPUB: rewrite the OPF (surgically, NOT a struct round-trip)

**Gap / hard requirement found in step 3:** the obvious approach (unmarshal the
OPF into `opfPackage`, mutate, re-marshal) is **lossy and destructive**: that
struct captures only a subset of the OPF (a few `dc:` fields + a thin manifest);
re-marshaling drops the spine, guide, unknown metadata, namespaces, attributes,
and element order, producing an OPF that can break the book. The package's parse
structs exist for *reading*, and must not be used to *write*.

The safe method is **surgical text editing of the raw OPF bytes**:

- Read the OPF entry's bytes. Locate the `<metadata>...</metadata>` span.
- Within it, for each editable field, replace the existing `<dc:title>` (etc.)
  element's text, or insert the element if absent (e.g. a comic-less-style epub
  missing `dc:description`). Replace ALL `<dc:creator>` elements with the edited
  author list; same for identifiers. Leave everything outside the targeted
  elements byte-for-byte unchanged.
- All inserted/replaced values are XML-escaped via `encoding/xml`
  (`xml.EscapeText`), never string-concatenated, so an edited value like
  `</dc:title><script>` cannot break out (ties into the sanitize requirement).
- Re-zip: copy every original entry through unchanged EXCEPT the OPF, which is
  replaced with the edited bytes. Preserve the `mimetype` entry first and stored
  (uncompressed) per the EPUB spec.
- This is fiddlier than the comic path; if the metadata block can't be located or
  edited safely, FAIL the embed (keep the original + DB edit) rather than risk a
  corrupt book.

**Confirmed against real epubs on disk (step 3b):**

- The OPF path varies (`OEBPS/content.opf`, a publisher-specific name, ...): get
  it from `container.xml` (the parser's `findOPFPath` already does), never assume.
- `dc:` is the common literal prefix, but elements carry attributes that MUST be
  preserved when only the text changes: `<dc:creator id="author_0">`,
  `<dc:creator opf:file-as="..." opf:role="aut">`, `<dc:identifier
  opf:scheme="ISBN">`, `<dc:date opf:event="publication">`. So for single-valued
  fields, replace only the element's inner TEXT, keeping the open tag (and its
  attributes) intact. Indentation/whitespace also varies (tabs vs spaces).
- Multi-valued elements: `<dc:creator>` (authors) and `<dc:subject>` repeat.
  Editing authors = remove ALL existing `<dc:creator>` elements, then insert one
  plain `<dc:creator>name</dc:creator>` per edited author. The file-as/role attrs
  are dropped on an explicit author edit (acceptable: the user changed authors).
- v1 scope: title, language, publisher, description, date (single-valued, text
  replace or insert-before-`</metadata>`) and creators (authors; remove-all +
  re-insert). Identifiers/subjects are left as-is in the file in v1 (the DB still
  carries edited identifiers; embedding them is a later refinement).
- The matcher is tag-name + namespace-prefix aware but tolerant of attributes and
  whitespace; anything it can't confidently edit -> FAIL the embed (DB edit
  survives). Re-`epub.Read` the rewritten file before the caller swaps it in.

### CBZ (comic): write/replace `ComicInfo.xml`

- This is *easier* than the epub path: there is no OPF to surgically edit, just a
  single `ComicInfo.xml` at the archive root. Serialize the catalog's current
  values into the ComicRack schema (`Title`, `Series`, `Number`, `Writer`,
  `Summary`, `LanguageISO`, `Year`), and write the zip with that entry replaced
  (or added if absent), copying the page images through unchanged.
- `internal/comic` already *reads* `ComicInfo.xml`; this adds the writer side.
  Keep it the same "verify before swap" discipline: re-`comic.Read` the rewritten
  CBZ before swapping it in.
- A nice property: rewriting only the `ComicInfo.xml` entry and store-copying the
  (already-compressed) page images is cheap, so embedding a comic's metadata is
  fast even for a large volume.

Both are the heavy, risky part (rewriting a zip can corrupt a file). Do it to a
temp file, validate it parses (`epub.Read` / `comic.Read`) before swapping, and
keep the original until the new one is verified. Treat it like the import
pipeline's "verify before commit" step. ComicInfo.xml often does not exist in the
source comic, so for most comics this ADDS the entry rather than replacing it.

## 8. Build order

1. Schema: `edited_fields`, `edited_at`, `slug_override` (idempotent
   `addColumnIfMissing`); backfill `slug_override` for existing rows to their
   current content-hash slug on the next scan/migrate. Make `Book.Slug()` prefer
   `slug_override`. Wire `tags`/`book_tags`. Tests: migration, slug stability.
2. `catalog.UpdateMetadata(slug, Edits)` (DB write, join upserts, FTS re-sync,
   record `edited_fields`) + teach `upsertBook` to skip edited columns on scan.
   Tests: edit survives a rescan of the unchanged AND the changed file; FTS
   reflects it.
3. Write-back, split by risk (format-specific, verify-before-swap):
   - **(3a, this step) Comic:** `comic.WriteComicInfo` (add/replace ComicInfo.xml,
     store-copy pages, write to a temp file, re-`comic.Read` to verify). Small and
     safe. Tests: round-trip a CBZ with NO ComicInfo (added) and one WITH it
     (replaced); XML-escaping of hostile values; verify-before-swap.
   - **(3b, next step) EPUB:** surgical OPF byte-edit (see §7) + re-zip preserving
     every other entry and the stored `mimetype`. Riskier (varied OPF
     prefixes/structure); its own focused step, verified against real epubs.
   - Embed failure on either format leaves the original file AND the DB edit
     intact. Both writers operate on a temp copy and swap only after the rewrite
     re-parses. The caller regenerates the filename from the new title and updates
     `path` (slug unchanged); that swap/rename/path-update glue lands in step 4.
4. HTTP + UI: `GET /book/{slug}/edit` form, `PUT /api/books/{slug}` (DB edit then
   attempt embed; report embed status), non-JS form fallback; an Edit affordance
   on the grid/table and reader bar.
5. Cover override: a separate `data/covers/overrides/` namespace (NOT the cache
   path, which collides), cover resolution order override->cache->extract, a
   `PUT /book/{slug}/cover` upload (validate it decodes as an image), and the
   cache code taught to ignore the overrides subdir.

## 9. Risks / notes

- **Scan clobber is the whole ballgame.** Get the edited-fields skip right and
  test it hard; everything else is a form.
- **Keep the slug stable on edit.** It is the content hash; metadata edits must
  not change it or every edited book's URL breaks. (Only write-back, §7, changes
  the file and thus the slug, deliberately.)
- **FTS must re-sync on every edit** or search silently goes stale.
- **Stay dependency-free.** Plain form + one PUT keeps the frontend lean, matching
  the rest of the UI; do not reach for a frontend framework for this.
- **Concurrency.** Edits and the import watcher both write the catalog; the DB is
  single-writer (SQLite) and operations are short transactions, so this is fine,
  but route edits through the same `*catalog.Catalog` the rest of the app uses.
