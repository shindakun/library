package catalog

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func strptr(s string) *string { return &s }

// TestUpdateMetadataPersistsAndIndexes edits several fields and confirms they
// land in the DB and the FTS index, and that edited_fields/edited_at are set.
func TestUpdateMetadataPersistsAndIndexes(t *testing.T) {
	c, books := newTestCatalog(t)
	p := makeEPUB(t, books, "a.epub", "Original Title", "Orig Author")
	id, err := c.Index(context.Background(), p, "scan")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := c.Get(context.Background(), id)

	authors := []string{"New Author"}
	updated, err := c.UpdateMetadata(context.Background(), b.Slug(), Edits{
		Title:       strptr("Edited Title"),
		Authors:     &authors,
		Description: strptr("a new description"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "Edited Title" || len(updated.Authors) != 1 || updated.Authors[0] != "New Author" {
		t.Errorf("returned book not updated: %+v", updated)
	}
	if updated.EditedFields == "" || updated.EditedAt.IsZero() {
		t.Errorf("edited_fields/edited_at not set: %q / %v", updated.EditedFields, updated.EditedAt)
	}

	// Re-read from DB and check persistence.
	got, _ := c.Get(context.Background(), id)
	if got.Title != "Edited Title" || got.Authors[0] != "New Author" {
		t.Errorf("edit did not persist: %+v", got)
	}
	// FTS reflects the edit: new title/author searchable, old title not.
	if hits, _ := c.List(context.Background(), ListOptions{Query: "Edited"}); len(hits) != 1 {
		t.Errorf("FTS missing edited title: %d hits", len(hits))
	}
	if hits, _ := c.List(context.Background(), ListOptions{Query: "Original"}); len(hits) != 0 {
		t.Errorf("FTS still indexes the old title: %d hits", len(hits))
	}
}

// TestEditSurvivesRescanOfChangedFile is the load-bearing test: a rescan where
// the file CONTENT changed (new hash + different file metadata) must NOT clobber
// the user's edited fields, but must still refresh non-edited fields from the
// file.
func TestEditSurvivesRescanOfChangedFile(t *testing.T) {
	c, books := newTestCatalog(t)
	p := makeEPUB(t, books, "a.epub", "File Title v1", "File Author v1")
	id, err := c.Index(context.Background(), p, "scan")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := c.Get(context.Background(), id)

	// User edits the title (and only the title).
	if _, err := c.UpdateMetadata(context.Background(), b.Slug(), Edits{Title: strptr("User Title")}); err != nil {
		t.Fatal(err)
	}

	// The file is replaced with different content AND different metadata (new
	// title + author + description). Re-index the same path.
	makeEPUBFull(t, books, "a.epub", "File Title v2", "File Author v2", "file desc v2", "extra page to change the hash")
	if _, err := c.Index(context.Background(), p, "scan"); err != nil {
		t.Fatal(err)
	}

	got, _ := c.Get(context.Background(), id)
	// Edited title is preserved.
	if got.Title != "User Title" {
		t.Errorf("rescan clobbered edited title: got %q, want User Title", got.Title)
	}
	// Non-edited fields refreshed from the new file.
	if got.Description != "file desc v2" {
		t.Errorf("non-edited description not refreshed: got %q, want 'file desc v2'", got.Description)
	}
	if len(got.Authors) != 1 || got.Authors[0] != "File Author v2" {
		t.Errorf("non-edited authors not refreshed from file: %v", got.Authors)
	}
	// FTS still finds the edited title, not the file's.
	if hits, _ := c.List(context.Background(), ListOptions{Query: "User"}); len(hits) != 1 {
		t.Errorf("FTS lost the edited title after rescan: %d hits", len(hits))
	}
}

// TestEditedAuthorsSurviveRescan covers the join-clobber gap: edited authors must
// survive a changed-file rescan (joins are DELETEd+reinserted from the file
// otherwise).
func TestEditedAuthorsSurviveRescan(t *testing.T) {
	c, books := newTestCatalog(t)
	p := makeEPUB(t, books, "a.epub", "T", "File Author")
	id, _ := c.Index(context.Background(), p, "scan")
	b, _ := c.Get(context.Background(), id)

	custom := []string{"Custom Author A", "Custom Author B"}
	if _, err := c.UpdateMetadata(context.Background(), b.Slug(), Edits{Authors: &custom}); err != nil {
		t.Fatal(err)
	}

	makeEPUBFull(t, books, "a.epub", "T", "Different File Author", "d", "hash-changing tail")
	if _, err := c.Index(context.Background(), p, "scan"); err != nil {
		t.Fatal(err)
	}

	got, _ := c.Get(context.Background(), id)
	if len(got.Authors) != 2 || got.Authors[0] != "Custom Author A" {
		t.Errorf("edited authors clobbered by rescan: %v", got.Authors)
	}
}

// TestUpdateMetadataSanitizes confirms hostile input is stripped: control chars
// removed, length bounded, blanks dropped from lists.
func TestUpdateMetadataSanitizes(t *testing.T) {
	c, books := newTestCatalog(t)
	p := makeEPUB(t, books, "a.epub", "T", "A")
	id, _ := c.Index(context.Background(), p, "scan")
	b, _ := c.Get(context.Background(), id)

	dirtyTitle := "Clean\x00Title\n\twith\x1bctrl"
	longDesc := strings.Repeat("x", maxFieldLen+500)
	authors := []string{"  Spaced  ", "", "Good\x07Bell"}
	got, err := c.UpdateMetadata(context.Background(), b.Slug(), Edits{
		Title:       &dirtyTitle,
		Description: &longDesc,
		Authors:     &authors,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got.Title, "\x00\n\t\x1b") {
		t.Errorf("title not sanitized: %q", got.Title)
	}
	if got.Title != "CleanTitlewithctrl" {
		t.Errorf("title = %q, want CleanTitlewithctrl", got.Title)
	}
	if len(got.Description) > maxFieldLen {
		t.Errorf("description not length-bounded: %d > %d", len(got.Description), maxFieldLen)
	}
	if len(got.Authors) != 2 { // blank dropped
		t.Errorf("authors = %v, want 2 (blank dropped)", got.Authors)
	}
	for _, a := range got.Authors {
		if strings.ContainsRune(a, '\x07') {
			t.Errorf("author not sanitized: %q", a)
		}
	}
}

// makeEPUBFull writes an EPUB with title, author, description, and a filler file
// whose content varies the archive bytes (so the content hash changes between
// otherwise-similar builds). Used to simulate "the file changed" on rescan.
func makeEPUBFull(t *testing.T, libraryDir, file, title, author, desc, filler string) string {
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
<dc:title>`+title+`</dc:title><dc:creator>`+author+`</dc:creator><dc:language>en</dc:language>
<dc:description>`+desc+`</dc:description></metadata>
<manifest><item id="c" href="c.xhtml" media-type="application/xhtml+xml"/></manifest></package>`)
	add("c.xhtml", "<html><body>hi</body></html>")
	add("filler.txt", filler) // varies the archive bytes -> different content hash
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}
