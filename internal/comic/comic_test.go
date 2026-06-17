package comic

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

// tinyPNG is a 1x1 PNG; enough that mime/extension handling has real bytes.
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
	0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

// makeCBZ writes a CBZ with the given entries (name -> bytes) and returns its
// path. Entries are written in the given order so tests can scramble on-disk
// order and prove the reader re-sorts.
func makeCBZ(t *testing.T, dir, file string, entries [][2]any) string {
	t.Helper()
	p := filepath.Join(dir, file)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	for _, e := range entries {
		name := e[0].(string)
		body, _ := e[1].([]byte)
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func img(name string) [2]any { return [2]any{name, tinyPNG} }

// TestPageOrdering is the load-bearing test: pages must come back in natural
// reading order regardless of on-disk order or zero-padding.
func TestPageOrdering(t *testing.T) {
	dir := t.TempDir()
	// Deliberately scrambled, unpadded, with a non-image and ComicInfo.xml mixed
	// in. Expected reading order is 1,2,3,10,11 then nested.
	p := makeCBZ(t, dir, "c.cbz", [][2]any{
		img("page10.jpg"),
		img("page2.jpg"),
		{"ComicInfo.xml", []byte("<ComicInfo/>")},
		img("page1.jpg"),
		img("page11.jpg"),
		{"notes.txt", []byte("ignore me")},
		img("page3.jpg"),
	})
	pages, err := Pages(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"page1.jpg", "page2.jpg", "page3.jpg", "page10.jpg", "page11.jpg"}
	if len(pages) != len(want) {
		t.Fatalf("got %d pages %v, want %d %v", len(pages), pages, len(want), want)
	}
	for i := range want {
		if pages[i] != want[i] {
			t.Errorf("page[%d] = %q, want %q (full: %v)", i, pages[i], want[i], pages)
		}
	}
}

// TestPageOrderingZeroPaddedAndNested checks zero-padded names and nested
// chapter folders sort sensibly together.
func TestPageOrderingZeroPaddedAndNested(t *testing.T) {
	dir := t.TempDir()
	p := makeCBZ(t, dir, "c.cbz", [][2]any{
		img("ch02/002.png"),
		img("ch01/010.png"),
		img("ch01/002.png"),
		img("ch10/001.png"),
		img("ch02/001.png"),
		img("ch01/001.png"),
	})
	pages, err := Pages(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ch01/001.png", "ch01/002.png", "ch01/010.png",
		"ch02/001.png", "ch02/002.png",
		"ch10/001.png",
	}
	for i := range want {
		if i >= len(pages) || pages[i] != want[i] {
			t.Fatalf("page order = %v, want %v", pages, want)
		}
	}
}

func TestReadComicInfoMetadata(t *testing.T) {
	dir := t.TempDir()
	ci := `<?xml version="1.0"?>
<ComicInfo>
  <Title>The Heist</Title>
  <Series>Caper</Series>
  <Number>3</Number>
  <Writer>Ada Author, Bo Writer</Writer>
  <Year>2021</Year>
  <Summary>A summary.</Summary>
  <LanguageISO>en</LanguageISO>
</ComicInfo>`
	p := makeCBZ(t, dir, "Whatever.cbz", [][2]any{
		{"ComicInfo.xml", []byte(ci)},
		img("01.jpg"),
		img("02.jpg"),
	})
	m, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "The Heist" {
		t.Errorf("Title = %q, want The Heist", m.Title)
	}
	if m.Series != "Caper" || m.SeriesIndex != 3 {
		t.Errorf("Series/Index = %q/%v, want Caper/3", m.Series, m.SeriesIndex)
	}
	if len(m.Authors) != 2 || m.Authors[0] != "Ada Author" || m.Authors[1] != "Bo Writer" {
		t.Errorf("Authors = %v, want [Ada Author, Bo Writer]", m.Authors)
	}
	if m.Language != "en" || m.Published != "2021" {
		t.Errorf("Language/Published = %q/%q", m.Language, m.Published)
	}
	if m.PageCount != 2 || !m.HasCover {
		t.Errorf("PageCount/HasCover = %d/%v, want 2/true", m.PageCount, m.HasCover)
	}
}

func TestReadFilenameFallback(t *testing.T) {
	dir := t.TempDir()
	// No ComicInfo.xml: Title comes from the filename, separators -> spaces.
	p := makeCBZ(t, dir, "Some_Comic.Issue.cbz", [][2]any{
		img("a.jpg"), img("b.jpg"),
	})
	m, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Some Comic Issue" {
		t.Errorf("Title = %q, want 'Some Comic Issue'", m.Title)
	}
	if len(m.Authors) != 0 {
		t.Errorf("Authors = %v, want empty for filename-only comic", m.Authors)
	}
}

func TestCoverIsFirstPage(t *testing.T) {
	dir := t.TempDir()
	p := makeCBZ(t, dir, "c.cbz", [][2]any{
		img("page02.png"), img("page01.png"),
	})
	data, mime, err := CoverImage(p)
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/png" {
		t.Errorf("cover mime = %q, want image/png", mime)
	}
	if len(data) == 0 {
		t.Error("cover bytes empty")
	}
}

func TestPageImageByIndexAndOutOfRange(t *testing.T) {
	dir := t.TempDir()
	p := makeCBZ(t, dir, "c.cbz", [][2]any{img("01.jpg"), img("02.jpg")})
	if _, _, err := PageImage(p, 1); err != nil {
		t.Errorf("PageImage(1): %v", err)
	}
	if _, _, err := PageImage(p, 5); err == nil {
		t.Error("PageImage(5) should be out of range")
	}
	if _, _, err := PageImage(p, -1); err == nil {
		t.Error("PageImage(-1) should be out of range")
	}
}

func TestReadEmptyOrNonImageArchive(t *testing.T) {
	dir := t.TempDir()
	// An archive with only a ComicInfo.xml and a text file: no pages.
	p := makeCBZ(t, dir, "empty.cbz", [][2]any{
		{"ComicInfo.xml", []byte("<ComicInfo><Title>X</Title></ComicInfo>")},
		{"readme.txt", []byte("nope")},
	})
	m, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.HasCover || m.PageCount != 0 {
		t.Errorf("HasCover/PageCount = %v/%d, want false/0", m.HasCover, m.PageCount)
	}
	if _, _, err := CoverImage(p); err == nil {
		t.Error("CoverImage on a pageless archive should error")
	}
}
