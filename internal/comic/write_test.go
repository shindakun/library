package comic

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readZipEntry returns the bytes of a named entry in a zip file (test helper).
func readZipEntry(t *testing.T, zipPath, name string) ([]byte, bool) {
	t.Helper()
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		if f.Name == name {
			rc, _ := f.Open()
			defer func() { _ = rc.Close() }()
			b := new(bytes.Buffer)
			_, _ = b.ReadFrom(rc)
			return b.Bytes(), true
		}
	}
	return nil, false
}

// TestWriteComicInfoAddsWhenAbsent: a comic with NO ComicInfo.xml gets one added,
// and the pages are preserved.
func TestWriteComicInfoAddsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	src := makeCBZ(t, dir, "src.cbz", [][2]any{
		img("page01.png"), img("page02.png"),
	})
	// Sanity: source has no ComicInfo.xml.
	if _, ok := readZipEntry(t, src, "ComicInfo.xml"); ok {
		t.Fatal("fixture unexpectedly already has ComicInfo.xml")
	}

	dst := filepath.Join(dir, "out.cbz")
	err := WriteComicInfo(src, dst, Metadata{
		Title:     "The Heist",
		Authors:   []string{"Ada Author", "Bo Writer"},
		Series:    "Caper",
		Language:  "en",
		Published: "2021",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// ComicInfo.xml now present and parses back to the written values.
	m, err := Read(dst)
	if err != nil {
		t.Fatalf("converted cbz not readable: %v", err)
	}
	if m.Title != "The Heist" {
		t.Errorf("Title = %q, want The Heist", m.Title)
	}
	if m.Series != "Caper" || m.Language != "en" || m.Published != "2021" {
		t.Errorf("metadata not round-tripped: %+v", m)
	}
	if len(m.Authors) != 2 || m.Authors[0] != "Ada Author" {
		t.Errorf("Authors = %v, want [Ada Author, Bo Writer]", m.Authors)
	}
	// Pages preserved in order.
	pages, _ := Pages(dst)
	if len(pages) != 2 || pages[0] != "page01.png" {
		t.Errorf("pages not preserved: %v", pages)
	}
}

// TestWriteComicInfoReplacesExisting: an existing ComicInfo.xml is replaced (not
// duplicated), and the page bytes are byte-identical to the source.
func TestWriteComicInfoReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	src := makeCBZ(t, dir, "src.cbz", [][2]any{
		{"ComicInfo.xml", []byte("<ComicInfo><Title>Old Title</Title></ComicInfo>")},
		img("01.jpg"),
	})
	origPage, _ := readZipEntry(t, src, "01.jpg")

	dst := filepath.Join(dir, "out.cbz")
	if err := WriteComicInfo(src, dst, Metadata{Title: "New Title"}, nil); err != nil {
		t.Fatal(err)
	}

	// Exactly one ComicInfo.xml, with the new title.
	zr, _ := zip.OpenReader(dst)
	defer func() { _ = zr.Close() }()
	count := 0
	for _, f := range zr.File {
		if f.Name == "ComicInfo.xml" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("ComicInfo.xml count = %d, want 1", count)
	}
	m, _ := Read(dst)
	if m.Title != "New Title" {
		t.Errorf("Title = %q, want New Title", m.Title)
	}
	// Page bytes unchanged.
	newPage, ok := readZipEntry(t, dst, "01.jpg")
	if !ok || !bytes.Equal(origPage, newPage) {
		t.Error("page bytes changed during rewrite")
	}
}

// TestWriteComicInfoEscapesHostileValues: a value containing XML metacharacters
// must be escaped, not break the document (injection guard).
func TestWriteComicInfoEscapesHostileValues(t *testing.T) {
	dir := t.TempDir()
	src := makeCBZ(t, dir, "src.cbz", [][2]any{img("01.jpg")})

	dst := filepath.Join(dir, "out.cbz")
	hostile := "</Title><Injected>x</Injected><Title>"
	if err := WriteComicInfo(src, dst, Metadata{Title: hostile}, nil); err != nil {
		t.Fatal(err)
	}
	raw, ok := readZipEntry(t, dst, "ComicInfo.xml")
	if !ok {
		t.Fatal("no ComicInfo.xml written")
	}
	if strings.Contains(string(raw), "<Injected>") {
		t.Errorf("hostile value not escaped; raw XML:\n%s", raw)
	}
	// And it still parses, with the literal hostile string as the title.
	m, err := Read(dst)
	if err != nil {
		t.Fatalf("escaped doc did not parse: %v", err)
	}
	if m.Title != hostile {
		t.Errorf("round-tripped title = %q, want the literal hostile string", m.Title)
	}
}

// TestWriteComicInfoFailureLeavesNoPartial: a failed write (bad source) must not
// leave a dst file behind.
func TestWriteComicInfoFailureLeavesNoPartial(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "notazip.cbz")
	if err := os.WriteFile(bad, []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.cbz")
	if err := WriteComicInfo(bad, dst, Metadata{Title: "X"}, nil); err == nil {
		t.Error("expected error opening a non-zip source")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("failed write left a partial dst file")
	}
}
