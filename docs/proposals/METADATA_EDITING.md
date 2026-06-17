# Implementation guide: metadata editing

Status: **in progress.** Design + build plan for in-browser metadata editing for
both EPUBs and CBZ comics, kept lightweight (plain HTML forms, a few JSON
endpoints, no new frontend dependencies). Edits land in the DB first, then are
embedded into the file (best-effort, verify-before-swap); a successful embed
replaces the original, a failed one keeps both the original file and the DB edit.
The slug stays stable across embeds (§3). Most comics have no `ComicInfo.xml`, so
for them embedding ADDS the metadata rather than replacing it.

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
  `upsertBook` lines ~199-200.
- Record each changed column name into `edited_fields`.

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

## 6. Cover editing (separate, avoids zip surgery)

Covers are read live from the epub (`epub.CoverImage` on each `GET
/book/{slug}/cover`). To allow replacing a cover WITHOUT rewriting the zip:

- Store an override at `data/covers/<slug>.jpg` (a new `data/covers/` dir,
  gitignored like the rest of `data/`).
- `GET /book/{slug}/cover`: serve the override if it exists, else fall back to
  `epub.CoverImage`.
- `PUT /book/{slug}/cover`: accept an uploaded image, write it to the override
  path (re-encode/resize with stdlib `image/*` to bound size).
- Set `has_cover` accordingly.

This keeps covers editable without touching the book file, consistent with the
DB-authoritative model.

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

### EPUB: rewrite the OPF

- Open the epub zip, parse the OPF, replace the `dc:` elements + meta entries
  with the catalog's current values, rewrite the cover if overridden, write a new
  zip.

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
3. Write-back (format-specific, verify-before-swap): `comic.WriteComicInfo`
   (add/replace ComicInfo.xml, store-copy pages) and `epub`-OPF rewrite. Each
   rewrites a temp copy, re-parses it, swaps atomically, regenerates the filename
   from the new title, and updates `path` (slug unchanged). Tests: round-trip a
   CBZ with NO ComicInfo (it gets added) and one WITH it (replaced); embed
   failure leaves original + DB edit intact.
4. HTTP + UI: `GET /book/{slug}/edit` form, `PUT /api/books/{slug}` (DB edit then
   attempt embed; report embed status), non-JS form fallback; an Edit affordance
   on the grid/table and reader bar.
5. Cover override (dir + serve-override + upload endpoint).

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
