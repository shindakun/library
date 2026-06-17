package catalog

import (
	"archive/zip"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newTestCatalog opens a catalog backed by a temp dir + temp SQLite DB.
func newTestCatalog(t *testing.T) (*Catalog, string) {
	t.Helper()
	dir := t.TempDir()
	books := filepath.Join(dir, "books")
	if err := os.MkdirAll(books, 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := Open(filepath.Join(dir, "catalog.db"), books, filepath.Join(dir, "covers"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, books
}

// TestMigrateAddsFormatColumnToOldDB verifies the format migration against a
// real pre-format database: an old books table with no `format` column and a
// row in it. migrate() must add the column (defaulting existing rows to "epub")
// and must be safe to run again (idempotent), since it runs on every Open.
func TestMigrateAddsFormatColumnToOldDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.db")

	// Stand up an OLD-shape DB by hand: books WITHOUT the format column, one row.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE books (
		id INTEGER PRIMARY KEY, title TEXT NOT NULL, sort_title TEXT,
		path TEXT NOT NULL UNIQUE, file_size INTEGER, file_hash TEXT,
		language TEXT, publisher TEXT, description TEXT, published TEXT,
		has_cover INTEGER NOT NULL DEFAULT 0, added_at INTEGER NOT NULL, source TEXT);`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO books (title, path, added_at) VALUES ('Old Book','a/b.epub',1)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// Open runs migrate(): the guarded ALTER must add format and default to epub.
	c, err := Open(dbPath, filepath.Join(dir, "books"), "")
	if err != nil {
		t.Fatalf("open/migrate old DB: %v", err)
	}
	defer func() { _ = c.Close() }()

	var format string
	if err := c.db.QueryRow(`SELECT format FROM books WHERE path='a/b.epub'`).Scan(&format); err != nil {
		t.Fatalf("format column not queryable after migrate: %v", err)
	}
	if format != "epub" {
		t.Errorf("backfilled format = %q, want epub", format)
	}

	// Idempotent: running migrate again (as the next startup would) must not error
	// with a duplicate-column failure.
	if err := c.migrate(); err != nil {
		t.Errorf("second migrate() failed (not idempotent): %v", err)
	}
}

// makeEPUBWithCover writes an EPUB whose manifest declares a cover image, plus
// the image bytes, so cover extraction has something to find.
func makeEPUBWithCover(t *testing.T, libraryDir, file, title string, img []byte) string {
	t.Helper()
	p := filepath.Join(libraryDir, file)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	add := func(name string, content []byte) {
		w, _ := zw.Create(name)
		_, _ = w.Write(content)
	}
	add("META-INF/container.xml", []byte(`<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
<rootfiles><rootfile full-path="content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`))
	add("content.opf", []byte(`<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
<dc:title>`+title+`</dc:title></metadata>
<manifest><item id="cover" href="cover.jpg" media-type="image/jpeg" properties="cover-image"/>
<item id="c" href="c.xhtml" media-type="application/xhtml+xml"/></manifest></package>`))
	add("cover.jpg", img)
	add("c.xhtml", []byte("<html><body>hi</body></html>"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCoverCacheOnIndex verifies indexing a cover-bearing epub writes the cover
// to the covers dir, and CoverCachePath then finds it.
func TestCoverCacheOnIndex(t *testing.T) {
	dir := t.TempDir()
	library := filepath.Join(dir, "library")
	covers := filepath.Join(dir, "covers")
	_ = os.MkdirAll(library, 0o755)
	c, err := Open(filepath.Join(dir, "catalog.db"), library, covers)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	imgBytes := []byte("\xff\xd8\xff\xe0fakejpegdata") // JPEG magic prefix
	p := makeEPUBWithCover(t, library, "a.epub", "Alpha", imgBytes)
	if _, err := c.Index(context.Background(), p, "scan"); err != nil {
		t.Fatal(err)
	}

	books, _ := c.List(context.Background(), ListOptions{})
	if len(books) != 1 {
		t.Fatalf("want 1 book, got %d", len(books))
	}
	cached := c.CoverCachePath(books[0])
	if cached == "" {
		t.Fatal("cover was not cached on index")
	}
	got, err := os.ReadFile(cached)
	if err != nil || string(got) != string(imgBytes) {
		t.Errorf("cached cover bytes = %q (err %v), want the original image", got, err)
	}
}

// TestCoverCacheDisabled: with no covers dir, CoverCachePath returns "" and
// indexing still works.
func TestCoverCacheDisabled(t *testing.T) {
	dir := t.TempDir()
	library := filepath.Join(dir, "library")
	_ = os.MkdirAll(library, 0o755)
	c, err := Open(filepath.Join(dir, "catalog.db"), library, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	p := makeEPUBWithCover(t, library, "a.epub", "Alpha", []byte("\xff\xd8\xffimg"))
	if _, err := c.Index(context.Background(), p, "scan"); err != nil {
		t.Fatal(err)
	}
	books, _ := c.List(context.Background(), ListOptions{})
	if c.CoverCachePath(books[0]) != "" {
		t.Error("cache disabled but CoverCachePath returned a path")
	}
}

// makeEPUB writes a minimal valid EPUB with the given title/author into the
// books dir and returns its absolute path.
func makeEPUB(t *testing.T, libraryDir, file, title, author string) string {
	t.Helper()
	p := filepath.Join(libraryDir, file)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	add := func(name, content string) {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(content))
	}
	add("META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
<rootfiles><rootfile full-path="content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`)
	add("content.opf", `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
<dc:title>`+title+`</dc:title><dc:creator>`+author+`</dc:creator><dc:language>en</dc:language></metadata>
<manifest><item id="c" href="c.xhtml" media-type="application/xhtml+xml"/></manifest></package>`)
	add("c.xhtml", "<html><body>hi</body></html>")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// makeCBZ writes a minimal valid comic archive (ComicInfo.xml + one image page).
func makeCBZ(t *testing.T, libraryDir, file, title string) string {
	t.Helper()
	p := filepath.Join(libraryDir, file)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	ci, _ := zw.Create("ComicInfo.xml")
	_, _ = ci.Write([]byte("<ComicInfo><Title>" + title + "</Title></ComicInfo>"))
	pg, _ := zw.Create("page01.png")
	_, _ = pg.Write([]byte("image-bytes"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestScanIndexesComics pins that a rescan picks up CBZ files, not just epubs:
// importing calls Index directly, but a clean-db rescan (or a CBZ dropped into
// the library) must still catalog comics.
func TestScanIndexesComics(t *testing.T) {
	c, books := newTestCatalog(t)
	makeEPUB(t, books, "a.epub", "Alpha", "Author One")
	makeCBZ(t, books, "comic.cbz", "A Comic")

	n, err := c.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("scan indexed %d, want 2 (epub + cbz)", n)
	}
	got, _ := c.List(context.Background(), ListOptions{})
	var foundComic bool
	for _, b := range got {
		if b.Format == "cbz" {
			foundComic = true
			if b.Title != "A Comic" {
				t.Errorf("comic title = %q, want A Comic", b.Title)
			}
		}
	}
	if !foundComic {
		t.Error("scan did not index the CBZ as a comic")
	}
}

func TestScanIndexesBooks(t *testing.T) {
	c, books := newTestCatalog(t)
	makeEPUB(t, books, "a.epub", "Alpha", "Author One")
	makeEPUB(t, books, "b.epub", "Beta", "Author Two")

	n, err := c.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("scan indexed %d, want 2", n)
	}
	got, _ := c.List(context.Background(), ListOptions{})
	if len(got) != 2 {
		t.Fatalf("list returned %d books, want 2", len(got))
	}
	// Format threads through Index -> upsert -> Scan: epubs classify as "epub".
	for _, b := range got {
		if b.Format != "epub" {
			t.Errorf("book %q format = %q, want epub", b.Title, b.Format)
		}
	}
}

func TestIndexIsIdempotent(t *testing.T) {
	c, books := newTestCatalog(t)
	p := makeEPUB(t, books, "a.epub", "Alpha", "Author One")

	id1, err := c.Index(context.Background(), p, "scan")
	if err != nil {
		t.Fatal(err)
	}
	// Re-indexing the same unchanged file must return the same id and not
	// duplicate the row.
	id2, err := c.Index(context.Background(), p, "scan")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("re-index changed id: %d -> %d", id1, id2)
	}
	got, _ := c.List(context.Background(), ListOptions{})
	if len(got) != 1 {
		t.Errorf("idempotent re-index produced %d rows, want 1", len(got))
	}
}

// TestPruneRemovesMissingFiles covers the bug fixed after deleting books left
// dangling rows: a scan must drop catalog entries whose file is gone.
func TestPruneRemovesMissingFiles(t *testing.T) {
	c, books := newTestCatalog(t)
	pa := makeEPUB(t, books, "a.epub", "Alpha", "A")
	makeEPUB(t, books, "b.epub", "Beta", "B")
	if _, err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Delete one file on disk, then rescan.
	if err := os.Remove(pa); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _ := c.List(context.Background(), ListOptions{})
	if len(got) != 1 {
		t.Fatalf("after deleting a.epub, catalog has %d books, want 1", len(got))
	}
	if got[0].Title != "Beta" {
		t.Errorf("surviving book = %q, want Beta", got[0].Title)
	}
}

func TestPruneRemovesJoinRows(t *testing.T) {
	c, books := newTestCatalog(t)
	p := makeEPUB(t, books, "a.epub", "Alpha", "Solo Author")
	if _, err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(p)
	if _, err := c.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The author join row should be gone via ON DELETE CASCADE; filtering by it
	// must return nothing.
	got, _ := c.List(context.Background(), ListOptions{Author: "Solo Author"})
	if len(got) != 0 {
		t.Errorf("pruned book still reachable by author filter: %d rows", len(got))
	}
}

func TestSearchByTitleAndAuthor(t *testing.T) {
	c, books := newTestCatalog(t)
	makeEPUB(t, books, "a.epub", "The Hobbit", "Tolkien")
	makeEPUB(t, books, "b.epub", "Dune", "Herbert")
	if _, err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	hits, _ := c.List(context.Background(), ListOptions{Query: "hobbit"})
	if len(hits) != 1 || hits[0].Title != "The Hobbit" {
		t.Errorf("title search 'hobbit' = %v, want [The Hobbit]", titles(hits))
	}
	hits, _ = c.List(context.Background(), ListOptions{Query: "herbert"})
	if len(hits) != 1 || hits[0].Title != "Dune" {
		t.Errorf("author search 'herbert' = %v, want [Dune]", titles(hits))
	}
	// Prefix match: "hobb" should still find The Hobbit.
	hits, _ = c.List(context.Background(), ListOptions{Query: "hobb"})
	if len(hits) != 1 {
		t.Errorf("prefix search 'hobb' returned %d, want 1", len(hits))
	}
}

func TestListSortsByAuthorThenTitle(t *testing.T) {
	c, books := newTestCatalog(t)
	// Deliberately insert out of order. Expected author>title order:
	//   Adams / Hitchhiker, Asimov / Caves, Asimov / Foundation, (no author) / Zzz
	makeEPUB(t, books, "1.epub", "Foundation", "Isaac Asimov")
	makeEPUB(t, books, "2.epub", "The Hitchhiker's Guide", "Douglas Adams")
	makeEPUB(t, books, "3.epub", "The Caves of Steel", "Isaac Asimov")
	makeEPUB(t, books, "4.epub", "Zzz Orphan", "") // no author -> sorts last
	if _, err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := c.List(context.Background(), ListOptions{}) // default = author,title
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"The Hitchhiker's Guide", "The Caves of Steel", "Foundation", "Zzz Orphan"}
	if g := titles(got); !equal(g, want) {
		t.Errorf("author>title order = %v, want %v", g, want)
	}
}

func TestListSortRecent(t *testing.T) {
	c, books := newTestCatalog(t)
	// Index one at a time so added_at differs and order is deterministic.
	p1 := makeEPUB(t, books, "1.epub", "First", "A")
	if _, err := c.Index(context.Background(), p1, "scan"); err != nil {
		t.Fatal(err)
	}
	p2 := makeEPUB(t, books, "2.epub", "Second", "B")
	if _, err := c.Index(context.Background(), p2, "scan"); err != nil {
		t.Fatal(err)
	}
	got, _ := c.List(context.Background(), ListOptions{Sort: SortRecent})
	// added_at is whole-second; both may share a timestamp, so just assert the
	// set and that recent-sort doesn't error or drop rows. The ordering tie is
	// broken by sort_title (First, Second).
	if len(got) != 2 {
		t.Fatalf("SortRecent returned %d, want 2", len(got))
	}
}

func TestSlugIsStableAcrossRebuild(t *testing.T) {
	dir := t.TempDir()
	libraryDir := filepath.Join(dir, "library")
	_ = os.MkdirAll(libraryDir, 0o755)
	makeEPUB(t, libraryDir, "a.epub", "Alpha", "Author")

	// First DB: index and capture the slug.
	c1, err := Open(filepath.Join(dir, "cat1.db"), libraryDir, filepath.Join(dir, "covers1"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c1.Scan(context.Background())
	b1, _ := c1.List(context.Background(), ListOptions{})
	slug := b1[0].Slug()
	id1 := b1[0].ID
	_ = c1.Close()

	if slug == "" || len(slug) != 16 {
		t.Fatalf("slug = %q, want 16 hex chars", slug)
	}

	// Fresh DB over the same files (simulating a rebuild). The integer id may
	// differ, but the slug MUST be identical and resolve to the same book.
	c2, err := Open(filepath.Join(dir, "cat2.db"), libraryDir, filepath.Join(dir, "covers2"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	_, _ = c2.Scan(context.Background())

	got, err := c2.GetBySlug(context.Background(), slug)
	if err != nil {
		t.Fatalf("GetBySlug(%q) after rebuild: %v", slug, err)
	}
	if got.Title != "Alpha" {
		t.Errorf("slug resolved to %q, want Alpha", got.Title)
	}
	if got.Slug() != slug {
		t.Errorf("slug changed across rebuild: %q -> %q", slug, got.Slug())
	}
	_ = id1 // the integer id is allowed to change; the slug is not
}

func TestHasHash(t *testing.T) {
	c, library := newTestCatalog(t)
	p := makeEPUB(t, library, "a.epub", "Alpha", "Author")
	if _, err := c.Index(context.Background(), p, "scan"); err != nil {
		t.Fatal(err)
	}
	hash, err := FileHash(p)
	if err != nil {
		t.Fatal(err)
	}
	has, err := c.HasHash(context.Background(), hash)
	if err != nil || !has {
		t.Errorf("HasHash(indexed file) = %v (err %v), want true", has, err)
	}
	if has, _ := c.HasHash(context.Background(), "0000000000000000"); has {
		t.Error("HasHash(unknown) = true, want false")
	}
	if has, _ := c.HasHash(context.Background(), ""); has {
		t.Error("HasHash(empty) = true, want false")
	}
}

func TestGetBySlugMissing(t *testing.T) {
	c, _ := newTestCatalog(t)
	_, err := c.GetBySlug(context.Background(), "deadbeefdeadbeef")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetBySlug(unknown) = %v, want sql.ErrNoRows", err)
	}
	_, err = c.GetBySlug(context.Background(), "")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetBySlug(empty) = %v, want sql.ErrNoRows", err)
	}
}

func TestGetMissingReturnsNoRows(t *testing.T) {
	c, _ := newTestCatalog(t)
	_, err := c.Get(context.Background(), 999)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get(999) error = %v, want sql.ErrNoRows", err)
	}
}

func TestReadStateRoundTrips(t *testing.T) {
	c, books := newTestCatalog(t)
	p := makeEPUB(t, books, "a.epub", "Alpha", "A")
	id, _ := c.Index(context.Background(), p, "scan")

	if err := c.SaveReadState(context.Background(), id, 0.42, "epubcfi(/6/4!/4)"); err != nil {
		t.Fatal(err)
	}
	// Overwrite (upsert) with a new position.
	if err := c.SaveReadState(context.Background(), id, 0.91, "epubcfi(/6/8!/2)"); err != nil {
		t.Fatal(err)
	}
	// We don't expose a read-state getter, so verify via the raw DB.
	var pct float64
	var cfi string
	err := c.db.QueryRow(`SELECT percent, cfi FROM read_state WHERE book_id=?`, id).Scan(&pct, &cfi)
	if err != nil {
		t.Fatal(err)
	}
	if pct != 0.91 || cfi != "epubcfi(/6/8!/2)" {
		t.Errorf("read_state = %v %q, want 0.91 epubcfi(/6/8!/2)", pct, cfi)
	}
}

func TestSortKey(t *testing.T) {
	cases := map[string]string{
		"The Hobbit":            "hobbit",
		"A Wizard of Earthsea":  "wizard of earthsea",
		"An Inconvenient Truth": "inconvenient truth",
		"Dune":                  "dune",
		"  The Stand ":          "stand", // trimmed first, then the "The " article is stripped
	}
	for in, want := range cases {
		if got := sortKey(in); got != want {
			t.Errorf("sortKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFTSQueryQuotesTerms(t *testing.T) {
	// A user query with FTS-special characters must not blow up the MATCH; each
	// term becomes a quoted prefix.
	got := ftsQuery(`foo "bar baz`)
	want := `"foo"* "bar"* "baz"*`
	if got != want {
		t.Errorf("ftsQuery = %q, want %q", got, want)
	}
	if ftsQuery("   ") != "" {
		t.Errorf("ftsQuery of whitespace should be empty")
	}
}

func titles(bs []*Book) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Title
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
