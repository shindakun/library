// Package catalog is the SQLite-backed book catalog: the single source of
// truth for the library. It indexes EPUB files on disk and answers queries for
// the web UI and the OPDS feed.
package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steve/library/internal/epub"
	"github.com/steve/library/internal/fileutil"
	_ "modernc.org/sqlite"
)

// Book is a catalog row plus its joined authors/series/tags.
type Book struct {
	ID          int64
	Title       string
	SortTitle   string
	Authors     []string
	Series      string
	SeriesIndex float64
	Language    string
	Publisher   string
	Description string
	Published   string
	Path        string // relative to the books root
	FileSize    int64
	FileHash    string
	HasCover    bool
	AddedAt     time.Time
	Source      string
	Identifiers map[string]string
}

// Slug is the stable public identifier for a book, derived from its content
// hash. Unlike the integer ID (an autoincrement rowid that changes if the DB is
// rebuilt), the slug is deterministic: the same file always yields the same
// slug, so a book's URL survives DB rebuilds, library moves, and re-imports.
// 16 hex chars (64 bits) is collision-safe for a personal library.
func (b *Book) Slug() string {
	if len(b.FileHash) >= 16 {
		return b.FileHash[:16]
	}
	return b.FileHash
}

// Catalog owns the DB handle and the books root directory.
type Catalog struct {
	db        *sql.DB
	libraryRoot string
}

// Open opens (and migrates) the SQLite catalog at dbPath. libraryRoot is the
// directory under which EPUB files live; book.Path is relative to it.
func Open(dbPath, libraryRoot string) (*Catalog, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	c := &Catalog{db: db, libraryRoot: libraryRoot}
	if err := c.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Catalog) Close() error { return c.db.Close() }

func (c *Catalog) migrate() error {
	_, err := c.db.Exec(schema)
	return err
}

// --- Ingest ---------------------------------------------------------------

// Index reads metadata from the EPUB at absPath and upserts a catalog row.
// source records how the file arrived ("scan", "acsm", "epub-import").
// It is idempotent on file path: re-indexing an unchanged file is a no-op.
func (c *Catalog) Index(ctx context.Context, absPath, source string) (int64, error) {
	rel, err := filepath.Rel(c.libraryRoot, absPath)
	if err != nil {
		return 0, fmt.Errorf("path %q not under books root: %w", absPath, err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return 0, err
	}
	hash, err := hashFile(absPath)
	if err != nil {
		return 0, err
	}

	// Skip if an identical file at this path is already indexed.
	var existingID int64
	var existingHash string
	err = c.db.QueryRowContext(ctx, `SELECT id, file_hash FROM books WHERE path = ?`, rel).Scan(&existingID, &existingHash)
	if err == nil && existingHash == hash {
		return existingID, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	meta, err := epub.Read(absPath)
	if err != nil {
		return 0, fmt.Errorf("read epub metadata: %w", err)
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	id, err := upsertBook(ctx, tx, rel, hash, info.Size(), source, meta, existingID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func upsertBook(ctx context.Context, tx *sql.Tx, rel, hash string, size int64, source string, meta *epub.Metadata, existingID int64) (int64, error) {
	sortTitle := sortKey(meta.Title)
	now := time.Now().Unix()

	var id int64
	if existingID != 0 {
		_, err := tx.ExecContext(ctx, `
			UPDATE books SET title=?, sort_title=?, language=?, publisher=?,
			    description=?, published=?, file_size=?, file_hash=?,
			    has_cover=?, source=? WHERE id=?`,
			meta.Title, sortTitle, meta.Language, meta.Publisher,
			meta.Description, meta.Published, size, hash,
			meta.HasCover, source, existingID)
		if err != nil {
			return 0, err
		}
		id = existingID
		// Clear join rows; we re-insert below.
		for _, t := range []string{"book_authors", "book_series", "book_tags", "identifiers"} {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE book_id=?", t), id); err != nil {
				return 0, err
			}
		}
	} else {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO books (title, sort_title, path, file_size, file_hash,
			    language, publisher, description, published, has_cover, added_at, source)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			meta.Title, sortTitle, rel, size, hash,
			meta.Language, meta.Publisher, meta.Description, meta.Published,
			meta.HasCover, now, source)
		if err != nil {
			return 0, err
		}
		id, _ = res.LastInsertId()
	}

	for _, name := range meta.Authors {
		aid, err := upsertNamed(ctx, tx, "authors", name)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO book_authors (book_id, author_id) VALUES (?,?)`, id, aid); err != nil {
			return 0, err
		}
	}
	if meta.Series != "" {
		sid, err := upsertNamed(ctx, tx, "series", meta.Series)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO book_series (book_id, series_id, idx) VALUES (?,?,?)`, id, sid, meta.SeriesIndex); err != nil {
			return 0, err
		}
	}
	for scheme, val := range meta.Identifiers {
		if _, err := tx.ExecContext(ctx, `INSERT INTO identifiers (book_id, scheme, value) VALUES (?,?,?)`, id, scheme, val); err != nil {
			return 0, err
		}
	}

	// Refresh the FTS row.
	tx.ExecContext(ctx, `DELETE FROM books_fts WHERE rowid=?`, id)
	if _, err := tx.ExecContext(ctx, `INSERT INTO books_fts (rowid, title, authors, description) VALUES (?,?,?,?)`,
		id, meta.Title, strings.Join(meta.Authors, " "), meta.Description); err != nil {
		return 0, err
	}
	return id, nil
}

func upsertNamed(ctx context.Context, tx *sql.Tx, table, name string) (int64, error) {
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("INSERT OR IGNORE INTO %s (name) VALUES (?)", table), name); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT id FROM %s WHERE name=?", table), name).Scan(&id)
	return id, err
}

// Scan walks the books root and indexes every .epub, returning how many were
// newly indexed or updated. A missing books root is treated as an empty library
// (not an error), but pruning still runs so deletions are reflected.
func (c *Catalog) Scan(ctx context.Context) (int, error) {
	n := 0
	if _, err := os.Stat(c.libraryRoot); os.IsNotExist(err) {
		pruned, _ := c.Prune(ctx)
		_ = pruned
		return 0, nil
	}
	err := filepath.WalkDir(c.libraryRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".epub") {
			return nil
		}
		if _, err := c.Index(ctx, p, "scan"); err != nil {
			// Log and continue; one bad file shouldn't abort the scan.
			fmt.Fprintf(os.Stderr, "scan: skip %s: %v\n", p, err)
			return nil
		}
		n++
		return nil
	})
	if err != nil {
		return n, err
	}
	// Prune rows whose file no longer exists on disk, so deleting a book from
	// books/ removes it from the catalog instead of leaving a dangling row that
	// 404s in the UI.
	if pruned, perr := c.Prune(ctx); perr != nil {
		fmt.Fprintf(os.Stderr, "scan: prune failed: %v\n", perr)
	} else if pruned > 0 {
		fmt.Fprintf(os.Stderr, "scan: pruned %d missing book(s)\n", pruned)
	}
	return n, err
}

// Prune deletes catalog rows whose underlying file is gone from disk. Returns
// the number removed. Join-table rows are removed by ON DELETE CASCADE.
func (c *Catalog) Prune(ctx context.Context) (int, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT id, path FROM books`)
	if err != nil {
		return 0, err
	}
	var missing []int64
	for rows.Next() {
		var id int64
		var rel string
		if err := rows.Scan(&id, &rel); err != nil {
			rows.Close()
			return 0, err
		}
		if _, err := os.Stat(filepath.Join(c.libraryRoot, rel)); os.IsNotExist(err) {
			missing = append(missing, id)
		}
	}
	rows.Close()

	for _, id := range missing {
		if _, err := c.db.ExecContext(ctx, `DELETE FROM books WHERE id=?`, id); err != nil {
			return 0, err
		}
		c.db.ExecContext(ctx, `DELETE FROM books_fts WHERE rowid=?`, id)
	}
	return len(missing), nil
}

// Reorganize moves every book file to its canonical Author/Title.epub location
// under the books root and updates the stored path. Files already in place are
// left alone. Returns the number of files moved. Empty author directories left
// behind by moves are removed.
func (c *Catalog) Reorganize(ctx context.Context) (int, error) {
	books, err := c.List(ctx, ListOptions{Limit: 1 << 30})
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, b := range books {
		want := fileutil.LibraryRelPath(b.Authors, b.Title)
		if want == b.Path {
			continue // already organized
		}
		srcAbs := filepath.Join(c.libraryRoot, b.Path)
		dstAbs := filepath.Join(c.libraryRoot, want)
		if _, err := os.Stat(srcAbs); err != nil {
			continue // file missing; Prune handles it
		}
		if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
			return moved, err
		}
		// Avoid clobbering an existing file at the destination.
		dstAbs = uniqueAbs(dstAbs)
		want, _ = filepath.Rel(c.libraryRoot, dstAbs)
		if err := os.Rename(srcAbs, dstAbs); err != nil {
			return moved, fmt.Errorf("move %q -> %q: %w", b.Path, want, err)
		}
		if _, err := c.db.ExecContext(ctx, `UPDATE books SET path=? WHERE id=?`, want, b.ID); err != nil {
			return moved, err
		}
		// Tidy a now-empty source directory (e.g. the old flat root won't be
		// empty, but per-author folders may be).
		if dir := filepath.Dir(srcAbs); dir != c.libraryRoot {
			os.Remove(dir) // no-op if non-empty
		}
		moved++
	}
	return moved, nil
}

// uniqueAbs returns p, or p with a " (2)" suffix before the extension if it
// already exists, so a move never overwrites an existing library file.
func uniqueAbs(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

// --- Queries --------------------------------------------------------------

// SortOrder selects how List orders results.
type SortOrder int

const (
	// SortAuthorTitle orders by author, then title (the default library view).
	SortAuthorTitle SortOrder = iota
	// SortRecent orders by most-recently-added first (the "Recently Added" feed).
	SortRecent
)

// ListOptions filters and paginates List.
type ListOptions struct {
	Query  string // FTS query; empty = all
	Author string
	Series string
	Sort   SortOrder
	Limit  int
	Offset int
}

// List returns books matching opts. Default ordering is by author, then title;
// set opts.Sort = SortRecent for newest-first.
func (c *Catalog) List(ctx context.Context, opts ListOptions) ([]*Book, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	var (
		where []string
		args  []any
		from  = "books b"
	)
	if opts.Query != "" {
		from += " JOIN books_fts f ON f.rowid = b.id"
		where = append(where, "books_fts MATCH ?")
		args = append(args, ftsQuery(opts.Query))
	}
	if opts.Author != "" {
		from += " JOIN book_authors ba ON ba.book_id=b.id JOIN authors a ON a.id=ba.author_id"
		where = append(where, "a.name = ?")
		args = append(args, opts.Author)
	}
	if opts.Series != "" {
		from += " JOIN book_series bs ON bs.book_id=b.id JOIN series s ON s.id=bs.series_id"
		where = append(where, "s.name = ?")
		args = append(args, opts.Series)
	}
	q := "SELECT b.id FROM " + from
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY " + orderClause(opts.Sort) + " LIMIT ? OFFSET ?"
	args = append(args, opts.Limit, opts.Offset)

	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return c.loadBooks(ctx, ids)
}

// Get returns a single book by id.
func (c *Catalog) Get(ctx context.Context, id int64) (*Book, error) {
	books, err := c.loadBooks(ctx, []int64{id})
	if err != nil {
		return nil, err
	}
	if len(books) == 0 {
		return nil, sql.ErrNoRows
	}
	return books[0], nil
}

// GetBySlug returns a book by its stable slug (a file-hash prefix). This is the
// lookup behind public URLs, so identity survives DB rebuilds.
func (c *Catalog) GetBySlug(ctx context.Context, slug string) (*Book, error) {
	if slug == "" {
		return nil, sql.ErrNoRows
	}
	var id int64
	// Match on the hash prefix; the slug is the first 16 hex chars of file_hash.
	err := c.db.QueryRowContext(ctx,
		`SELECT id FROM books WHERE file_hash LIKE ? || '%' LIMIT 1`, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	return c.Get(ctx, id)
}

func (c *Catalog) loadBooks(ctx context.Context, ids []int64) ([]*Book, error) {
	byID := map[int64]*Book{}
	order := make([]int64, 0, len(ids))
	for _, id := range ids {
		var b Book
		var added int64
		err := c.db.QueryRowContext(ctx, `
			SELECT id, title, sort_title, language, publisher, description,
			       published, path, file_size, file_hash, has_cover, added_at, source
			FROM books WHERE id=?`, id).Scan(
			&b.ID, &b.Title, &b.SortTitle, &b.Language, &b.Publisher, &b.Description,
			&b.Published, &b.Path, &b.FileSize, &b.FileHash, &b.HasCover, &added, &b.Source)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		b.AddedAt = time.Unix(added, 0)
		b.Identifiers = map[string]string{}
		byID[id] = &b
		order = append(order, id)
	}
	if len(order) == 0 {
		return nil, nil
	}

	// Hydrate joins in bulk-ish (per-book queries are fine at this scale).
	for id, b := range byID {
		rows, _ := c.db.QueryContext(ctx, `SELECT a.name FROM authors a JOIN book_authors ba ON ba.author_id=a.id WHERE ba.book_id=? ORDER BY a.name`, id)
		for rows.Next() {
			var n string
			rows.Scan(&n)
			b.Authors = append(b.Authors, n)
		}
		rows.Close()

		c.db.QueryRowContext(ctx, `SELECT s.name, bs.idx FROM series s JOIN book_series bs ON bs.series_id=s.id WHERE bs.book_id=?`, id).Scan(&b.Series, &b.SeriesIndex)

		rows, _ = c.db.QueryContext(ctx, `SELECT scheme, value FROM identifiers WHERE book_id=?`, id)
		for rows.Next() {
			var s, v string
			rows.Scan(&s, &v)
			b.Identifiers[s] = v
		}
		rows.Close()
	}

	out := make([]*Book, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}

// AbsPath returns the on-disk path for a book file.
func (c *Catalog) AbsPath(b *Book) string {
	return filepath.Join(c.libraryRoot, b.Path)
}

// SaveReadState upserts the browser reading position for a book.
func (c *Catalog) SaveReadState(ctx context.Context, bookID int64, percent float64, cfi string) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO read_state (book_id, percent, cfi, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(book_id) DO UPDATE SET percent=excluded.percent, cfi=excluded.cfi, updated_at=excluded.updated_at`,
		bookID, percent, cfi, time.Now().Unix())
	return err
}

// --- helpers --------------------------------------------------------------

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// orderClause returns the SQL ORDER BY body (no "ORDER BY" prefix) for a sort
// order. The author key is the book's alphabetically-first author name, via a
// correlated subquery so the active joins don't multiply rows; books with no
// author sort last. Comparison is case-insensitive.
func orderClause(s SortOrder) string {
	const authorExpr = `(SELECT MIN(a2.name) FROM authors a2
		JOIN book_authors ba2 ON ba2.author_id=a2.id WHERE ba2.book_id=b.id)`
	switch s {
	case SortRecent:
		return "b.added_at DESC, b.sort_title"
	default: // SortAuthorTitle
		return authorExpr + " IS NULL, LOWER(" + authorExpr + "), b.sort_title"
	}
}

// sortKey strips a leading article so "The Hobbit" sorts under H.
func sortKey(title string) string {
	t := strings.TrimSpace(title)
	for _, art := range []string{"The ", "A ", "An "} {
		if strings.HasPrefix(t, art) {
			return strings.ToLower(t[len(art):])
		}
	}
	return strings.ToLower(t)
}

// ftsQuery makes a user string safe-ish for FTS5 by quoting each term as a
// prefix match.
func ftsQuery(q string) string {
	fields := strings.Fields(q)
	sort.SliceStable(fields, func(i, j int) bool { return false })
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.ReplaceAll(f, `"`, "")
		if f == "" {
			continue
		}
		parts = append(parts, `"`+f+`"*`)
	}
	return strings.Join(parts, " ")
}
