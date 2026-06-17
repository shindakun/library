package epub

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sp(s string) *string { return &s }

// readEntry reads a zip entry's bytes (test helper).
func readEntry(t *testing.T, zipPath, name string) (string, bool) {
	t.Helper()
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		if f.Name == name {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			return string(b), true
		}
	}
	return "", false
}

func TestWriteMetadataReplacesExistingFields(t *testing.T) {
	src := writeEPUB(t, map[string]string{
		"META-INF/container.xml": containerXML,
		"OEBPS/content.opf":      opf("Old Title", "Old Author", `<dc:publisher>Old Pub</dc:publisher><dc:description>old desc</dc:description>`),
		"OEBPS/ch1.xhtml":        "<html><body>chapter</body></html>",
	})
	dst := filepath.Join(t.TempDir(), "out.epub")

	err := WriteMetadata(src, dst, Edits{
		Title:       sp("New Title"),
		Publisher:   sp("New Pub"),
		Description: sp("new desc"),
		Authors:     &[]string{"Author One", "Author Two"},
	})
	if err != nil {
		t.Fatal(err)
	}

	m, err := Read(dst)
	if err != nil {
		t.Fatalf("rewritten epub does not parse: %v", err)
	}
	if m.Title != "New Title" {
		t.Errorf("Title = %q, want New Title", m.Title)
	}
	if m.Publisher != "New Pub" {
		t.Errorf("Publisher = %q, want New Pub", m.Publisher)
	}
	if m.Description != "new desc" {
		t.Errorf("Description = %q, want new desc", m.Description)
	}
	if len(m.Authors) != 2 || m.Authors[0] != "Author One" || m.Authors[1] != "Author Two" {
		t.Errorf("Authors = %v, want [Author One, Author Two]", m.Authors)
	}
	// Non-targeted content (the chapter) is preserved byte-for-byte.
	if ch, ok := readEntry(t, dst, "OEBPS/ch1.xhtml"); !ok || ch != "<html><body>chapter</body></html>" {
		t.Errorf("chapter content not preserved: %q", ch)
	}
}

func TestWriteMetadataInsertsMissingField(t *testing.T) {
	// OPF has no dc:publisher; setting it must insert one.
	src := writeEPUB(t, map[string]string{
		"META-INF/container.xml": containerXML,
		"OEBPS/content.opf":      opf("T", "A", ``),
		"OEBPS/ch1.xhtml":        "x",
	})
	dst := filepath.Join(t.TempDir(), "out.epub")
	if err := WriteMetadata(src, dst, Edits{Publisher: sp("Inserted Pub")}); err != nil {
		t.Fatal(err)
	}
	m, _ := Read(dst)
	if m.Publisher != "Inserted Pub" {
		t.Errorf("inserted publisher = %q, want Inserted Pub", m.Publisher)
	}
	// Existing fields untouched.
	if m.Title != "T" || len(m.Authors) != 1 {
		t.Errorf("insert disturbed other fields: %+v", m)
	}
}

func TestWriteMetadataPreservesAttributesOnReplace(t *testing.T) {
	// A creator with file-as/role attributes; we only edit the TITLE, so the
	// creator element (with its attributes) must survive verbatim.
	customOPF := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
	<metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
		<dc:title>Old</dc:title>
		<dc:creator opf:file-as="Salvatore, R.A." opf:role="aut">R.A. Salvatore</dc:creator>
		<dc:identifier id="PrimaryID" opf:scheme="ISBN">978-0-7869-5432-2</dc:identifier>
		<dc:language>en-US</dc:language>
	</metadata>
	<manifest><item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/></manifest>
</package>`
	src := writeEPUB(t, map[string]string{
		"META-INF/container.xml": containerXML,
		"OEBPS/content.opf":      customOPF,
		"OEBPS/ch1.xhtml":        "x",
	})
	dst := filepath.Join(t.TempDir(), "out.epub")
	if err := WriteMetadata(src, dst, Edits{Title: sp("Edited")}); err != nil {
		t.Fatal(err)
	}
	raw, _ := readEntry(t, dst, "OEBPS/content.opf")
	// The creator's attributes and the identifier element must be intact.
	if !strings.Contains(raw, `opf:file-as="Salvatore, R.A." opf:role="aut"`) {
		t.Errorf("creator attributes lost:\n%s", raw)
	}
	if !strings.Contains(raw, `opf:scheme="ISBN">978-0-7869-5432-2`) {
		t.Errorf("identifier element lost:\n%s", raw)
	}
	if !strings.Contains(raw, "<dc:title>Edited</dc:title>") {
		t.Errorf("title not edited:\n%s", raw)
	}
}

func TestWriteMetadataEscapesHostileValue(t *testing.T) {
	src := writeEPUB(t, map[string]string{
		"META-INF/container.xml": containerXML,
		"OEBPS/content.opf":      opf("T", "A", ``),
		"OEBPS/ch1.xhtml":        "x",
	})
	dst := filepath.Join(t.TempDir(), "out.epub")
	hostile := "</dc:title><dc:creator>HACK</dc:creator><dc:title>"
	if err := WriteMetadata(src, dst, Edits{Title: sp(hostile)}); err != nil {
		t.Fatal(err)
	}
	m, err := Read(dst)
	if err != nil {
		t.Fatalf("escaped epub did not parse: %v", err)
	}
	// The hostile string is the literal title; no injected creator.
	if m.Title != hostile {
		t.Errorf("title = %q, want the literal hostile string", m.Title)
	}
	if len(m.Authors) != 1 || m.Authors[0] != "A" {
		t.Errorf("hostile value injected a creator: %v", m.Authors)
	}
}

func TestWriteMetadataFailsOnNoMetadataBlock(t *testing.T) {
	// A "valid enough to find the OPF" archive whose OPF has no <metadata> block:
	// the embed must fail (and leave no dst), not produce a broken book.
	src := writeEPUB(t, map[string]string{
		"META-INF/container.xml": containerXML,
		"OEBPS/content.opf":      `<?xml version="1.0"?><package><manifest/></package>`,
	})
	dst := filepath.Join(t.TempDir(), "out.epub")
	if err := WriteMetadata(src, dst, Edits{Title: sp("X")}); err == nil {
		t.Error("expected failure when OPF has no <metadata> block")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("failed embed left a partial dst")
	}
}

func TestWriteMetadataKeepsMimetypeFirstAndStored(t *testing.T) {
	src := writeEPUB(t, map[string]string{
		"META-INF/container.xml": containerXML,
		"OEBPS/content.opf":      opf("T", "A", ``),
		"OEBPS/ch1.xhtml":        "x",
	})
	dst := filepath.Join(t.TempDir(), "out.epub")
	if err := WriteMetadata(src, dst, Edits{Title: sp("New")}); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	if len(zr.File) == 0 || zr.File[0].Name != "mimetype" {
		t.Fatalf("first entry = %q, want mimetype", zr.File[0].Name)
	}
	if zr.File[0].Method != zip.Store {
		t.Errorf("mimetype not stored (method=%d)", zr.File[0].Method)
	}
}
