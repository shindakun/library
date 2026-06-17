# Implementation guide: metadata editing

Status: **proposed, not implemented.** This guide is a design + build plan for
in-browser metadata editing, kept deliberately lightweight (plain HTML forms,
a few JSON endpoints, no new frontend dependencies, no file-rewriting in v1).
Applies to both formats: the catalog row is format-neutral, so the v1
DB-authoritative editor covers EPUBs and CBZ comics alike. Optional Phase 2
write-back is format-specific (epub OPF, or a comic's `ComicInfo.xml`); see §7.

## 1. The core tension to resolve first

Today the **EPUB file is the source of truth**. `catalog.Index` reads metadata
out of the epub's OPF on every scan and *overwrites* the catalog row
(`upsertBook` runs `UPDATE books SET title=?, ...`). So a naive "edit the title
in the DB" would be silently clobbered by the next `Scan` (startup, `/api/scan`,
or an import touching the same path).

Any metadata-editing design must answer: **after a user edits a field, where is
the truth, and how does scan avoid stomping it?**

Three models, with the recommendation:

| Model | Edits live in | Pros | Cons |
| --- | --- | --- | --- |
| DB-authoritative (RECOMMENDED v1) | catalog row; scan skips edited fields | lightweight, safe, instant, no zip surgery | DB and file drift; edits not in the exported epub until written back |
| File write-back | rewrite the epub OPF | file is portable/self-describing | heavy, risks corrupting the zip, slow; needs careful OPF surgery |
| DB-authoritative + opt-in write-back | DB now, file on explicit "embed" action | best of both | two code paths; write-back is the hard part |

**v1 = DB-authoritative.** Editing writes to the catalog and the scan indexer
learns to leave user-edited fields alone. Write-back to the epub is a separate,
later phase (§6) because OPF-in-zip rewriting is the risky, heavy part and should
never be the default path for a "keep it lite" project.

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

Minimal additions to `internal/catalog/schema.go`:

```sql
-- Per-book record of which fields the user has hand-edited, so scan won't
-- overwrite them from the file. JSON array of column names, or a bitfield.
ALTER TABLE books ADD COLUMN edited_fields TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN edited_at INTEGER;  -- unix; null = never edited
```

`tags` / `book_tags` already exist; wire them up in the editor.

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

## 7. Optional Phase 2: write-back to the file

Only if/when the user wants edits embedded in the file itself (for export or
reading on a device that reads embedded metadata). Both formats follow the same
shape: a deliberate `POST /book/{slug}/embed` action (never automatic), rewrite
the archive to a temp file, validate it parses, atomically swap, and re-hash
(which changes the slug, so update it and redirect; the slug change is the cost
of embedding, surface it). The DB-authoritative editor (§1-6) is format-neutral
and already covers comics; only this write-back step is format-specific.

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
pipeline's "verify before commit" step.

Defer this. The DB-authoritative editor (§1-6) delivers the feature for both
formats; write-back is a power-user add-on.

## 8. Build order

1. Schema: add `edited_fields` / `edited_at`. Wire `tags`/`book_tags`.
2. `catalog.UpdateMetadata` + teach `upsertBook` to skip edited fields. Unit
   tests: edit a field, scan the unchanged-and-changed file, assert the edit
   survives and FTS reflects it.
3. Edit form template + endpoints; the inline-save JS with form fallback.
4. Cover override (dir + serve-override + upload endpoint).
5. (Later) file write-back behind an explicit action, verify-before-swap:
   epub OPF rewrite and CBZ `ComicInfo.xml` write/replace (the comic side reuses
   `internal/comic`, which already reads ComicInfo.xml; this adds the writer).

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
