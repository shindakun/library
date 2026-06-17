package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Editable field names. These are the single source of truth shared by
// UpdateMetadata (which records edits) and the scan path (which skips edited
// fields), so the two can never drift. The names double as the keys stored in
// the books.edited_fields JSON array.
const (
	fieldTitle       = "title"
	fieldSortTitle   = "sort_title"
	fieldLanguage    = "language"
	fieldPublisher   = "publisher"
	fieldDescription = "description"
	fieldPublished   = "published"
	fieldAuthors     = "authors"
	fieldSeries      = "series"
	fieldTags        = "tags"
	fieldIdentifiers = "identifiers"
)

// maxFieldLen bounds any single edited string so a hostile or accidental
// megabyte of text can't bloat the row, the FTS index, or a later filename.
const maxFieldLen = 2000

// Edits is the set of user-editable fields. A nil pointer means "leave
// unchanged"; a non-nil pointer sets the field (an empty string/slice clears it).
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

// sanitizeField trims, strips control characters, and length-bounds a single
// user-supplied string. The catalog never trusts its caller: edited values flow
// into the FTS index and (on embed) into filenames and XML, so they are cleaned
// here regardless of any HTTP-layer sanitizing.
func sanitizeField(s string) string {
	s = strings.Map(func(r rune) rune {
		// Drop ASCII control chars (incl. NUL, newlines, tabs) and the DEL char;
		// keep everything printable, including non-ASCII letters.
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > maxFieldLen {
		s = strings.TrimSpace(s[:maxFieldLen])
	}
	return s
}

func sanitizeList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if cleaned := sanitizeField(v); cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return out
}

// markEdited adds names to a JSON-encoded edited_fields list, de-duplicated and
// sorted (stable), and returns the new encoding.
func markEdited(existing string, names ...string) string {
	set := map[string]bool{}
	for _, n := range decodeEdited(existing) {
		set[n] = true
	}
	for _, n := range names {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	b, _ := json.Marshal(out)
	return string(b)
}

// decodeEdited parses the edited_fields JSON array; a malformed/empty value
// yields no edited fields (fail safe: scan refreshes from the file).
func decodeEdited(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(s), &out) != nil {
		return nil
	}
	return out
}

func editedSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, n := range decodeEdited(s) {
		m[n] = true
	}
	return m
}

// UpdateMetadata applies edits to the book identified by slug, records the
// changed fields in edited_fields (so a later scan won't overwrite them from the
// file), re-syncs the FTS row, and bumps edited_at. It does NOT touch the file or
// the slug. Returns the updated book.
func (c *Catalog) UpdateMetadata(ctx context.Context, slug string, e Edits) (*Book, error) {
	b, err := c.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	// Capture the pre-edit FTS terms so we can remove them from the contentless
	// FTS index (which needs the OLD values to delete a row).
	oldTitle, oldAuthors, oldDesc := b.Title, strings.Join(b.Authors, " "), b.Description

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var edited []string
	set := func(col string, field string, val any) error {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE books SET %s=? WHERE id=?", col), val, b.ID); err != nil {
			return err
		}
		edited = append(edited, field)
		return nil
	}

	if e.Title != nil {
		title := sanitizeField(*e.Title)
		if err := set("title", fieldTitle, title); err != nil {
			return nil, err
		}
		b.Title = title
		// If the user did not also set an explicit sort title, re-derive it from
		// the new title and leave sort_title UN-edited so it keeps tracking title.
		if e.SortTitle == nil {
			st := sortKey(title)
			if _, err := tx.ExecContext(ctx, `UPDATE books SET sort_title=? WHERE id=?`, st, b.ID); err != nil {
				return nil, err
			}
			b.SortTitle = st
		}
	}
	if e.SortTitle != nil {
		st := sanitizeField(*e.SortTitle)
		if err := set("sort_title", fieldSortTitle, st); err != nil {
			return nil, err
		}
		b.SortTitle = st
	}
	if e.Language != nil {
		v := sanitizeField(*e.Language)
		if err := set("language", fieldLanguage, v); err != nil {
			return nil, err
		}
		b.Language = v
	}
	if e.Publisher != nil {
		v := sanitizeField(*e.Publisher)
		if err := set("publisher", fieldPublisher, v); err != nil {
			return nil, err
		}
		b.Publisher = v
	}
	if e.Description != nil {
		v := sanitizeField(*e.Description)
		if err := set("description", fieldDescription, v); err != nil {
			return nil, err
		}
		b.Description = v
	}
	if e.Published != nil {
		v := sanitizeField(*e.Published)
		if err := set("published", fieldPublished, v); err != nil {
			return nil, err
		}
		b.Published = v
	}

	if e.Authors != nil {
		authors := sanitizeList(*e.Authors)
		if err := replaceJoin(ctx, tx, b.ID, "book_authors", "author_id", "authors", authors); err != nil {
			return nil, err
		}
		b.Authors = authors
		edited = append(edited, fieldAuthors)
	}
	if e.Series != nil {
		series := sanitizeField(*e.Series)
		idx := b.SeriesIndex
		if e.SeriesIndex != nil {
			idx = *e.SeriesIndex
		}
		if err := setSeries(ctx, tx, b.ID, series, idx); err != nil {
			return nil, err
		}
		b.Series, b.SeriesIndex = series, idx
		edited = append(edited, fieldSeries)
	} else if e.SeriesIndex != nil && b.Series != "" {
		// Index-only change on an existing series.
		if err := setSeries(ctx, tx, b.ID, b.Series, *e.SeriesIndex); err != nil {
			return nil, err
		}
		b.SeriesIndex = *e.SeriesIndex
		edited = append(edited, fieldSeries)
	}
	if e.Tags != nil {
		tags := sanitizeList(*e.Tags)
		if err := replaceJoin(ctx, tx, b.ID, "book_tags", "tag_id", "tags", tags); err != nil {
			return nil, err
		}
		edited = append(edited, fieldTags)
	}
	if e.Identifiers != nil {
		ids := map[string]string{}
		for k, v := range *e.Identifiers {
			k, v = sanitizeField(k), sanitizeField(v)
			if k != "" && v != "" {
				ids[k] = v
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM identifiers WHERE book_id=?`, b.ID); err != nil {
			return nil, err
		}
		for k, v := range ids {
			if _, err := tx.ExecContext(ctx, `INSERT INTO identifiers (book_id, scheme, value) VALUES (?,?,?)`, b.ID, k, v); err != nil {
				return nil, err
			}
		}
		b.Identifiers = ids
		edited = append(edited, fieldIdentifiers)
	}

	if len(edited) == 0 {
		return b, nil // nothing changed; don't bump edited_at
	}

	// Record edited fields + timestamp.
	newEdited := markEdited(b.EditedFields, edited...)
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE books SET edited_fields=?, edited_at=? WHERE id=?`, newEdited, now, b.ID); err != nil {
		return nil, err
	}
	b.EditedFields = newEdited
	b.EditedAt = time.Unix(now, 0)

	// Re-sync FTS so search reflects the edit. books_fts is contentless, so the
	// old row's terms must be removed with the 'delete' command (which needs the
	// OLD column values) before inserting the new ones.
	if err := ftsReplace(ctx, tx, b.ID, oldTitle, oldAuthors, oldDesc, b.Title, strings.Join(b.Authors, " "), b.Description); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return b, nil
}

// replaceJoin clears a name-backed join (authors/tags) for a book and re-inserts
// the given names, upserting into the name table.
func replaceJoin(ctx context.Context, tx *sql.Tx, bookID int64, joinTable, fk, nameTable string, names []string) error {
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE book_id=?", joinTable), bookID); err != nil {
		return err
	}
	for _, name := range names {
		nid, err := upsertNamed(ctx, tx, nameTable, name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("INSERT OR IGNORE INTO %s (book_id, %s) VALUES (?,?)", joinTable, fk), bookID, nid); err != nil {
			return err
		}
	}
	return nil
}

// ftsReplace updates the contentless books_fts index for one rowid. A contentless
// FTS5 table cannot be DELETEd from normally; the documented way to remove a row
// is the special 'delete' command carrying the row's OLD column values, after
// which the new terms are inserted. Passing the correct old values is what keeps
// stale terms (e.g. a pre-edit title) from lingering in search.
func ftsReplace(ctx context.Context, tx *sql.Tx, rowid int64, oldTitle, oldAuthors, oldDesc, newTitle, newAuthors, newDesc string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO books_fts (books_fts, rowid, title, authors, description) VALUES ('delete', ?, ?, ?, ?)`,
		rowid, oldTitle, oldAuthors, oldDesc); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO books_fts (rowid, title, authors, description) VALUES (?,?,?,?)`,
		rowid, newTitle, newAuthors, newDesc)
	return err
}

// setSeries replaces a book's single series + index.
func setSeries(ctx context.Context, tx *sql.Tx, bookID int64, series string, idx float64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM book_series WHERE book_id=?`, bookID); err != nil {
		return err
	}
	if series == "" {
		return nil
	}
	sid, err := upsertNamed(ctx, tx, "series", series)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO book_series (book_id, series_id, idx) VALUES (?,?,?)`, bookID, sid, idx)
	return err
}
