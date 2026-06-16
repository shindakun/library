package epub

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

// writeEPUB builds a minimal but valid EPUB on disk and returns its path.
// files maps zip-internal paths to contents; the caller supplies container.xml,
// the OPF, and any META-INF DRM files it wants to test.
func writeEPUB(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "book.epub")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	// The spec wants "mimetype" first and stored; we don't rely on that for
	// parsing, but include it so the fixture is realistic.
	mw, _ := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	mw.Write([]byte("application/epub+zip"))
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

const containerXML = `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`

// opf builds an OPF package document with the given title/author and extra
// metadata lines spliced in.
func opf(title, author, extraMeta string) string {
	return `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>` + title + `</dc:title>
    <dc:creator>` + author + `</dc:creator>
    <dc:language>en</dc:language>
    ` + extraMeta + `
  </metadata>
  <manifest><item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/></manifest>
</package>`
}

func TestRead_BasicMetadata(t *testing.T) {
	p := writeEPUB(t, map[string]string{
		"META-INF/container.xml": containerXML,
		"OEBPS/content.opf": opf("The Hobbit", "J.R.R. Tolkien",
			`<dc:publisher>Allen &amp; Unwin</dc:publisher>
			 <dc:identifier opf:scheme="ISBN" xmlns:opf="http://www.idpf.org/2007/opf">9780261103283</dc:identifier>
			 <meta name="calibre:series" content="Middle-earth"/>
			 <meta name="calibre:series_index" content="1"/>`),
		"OEBPS/ch1.xhtml": "<html><body>x</body></html>",
	})

	m, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "The Hobbit" {
		t.Errorf("title = %q, want The Hobbit", m.Title)
	}
	if len(m.Authors) != 1 || m.Authors[0] != "J.R.R. Tolkien" {
		t.Errorf("authors = %v, want [J.R.R. Tolkien]", m.Authors)
	}
	if m.Language != "en" {
		t.Errorf("language = %q, want en", m.Language)
	}
	if m.Publisher != "Allen & Unwin" {
		t.Errorf("publisher = %q, want 'Allen & Unwin' (entity-decoded)", m.Publisher)
	}
	if m.Series != "Middle-earth" || m.SeriesIndex != 1 {
		t.Errorf("series = %q idx %v, want Middle-earth 1", m.Series, m.SeriesIndex)
	}
	if got := m.Identifiers["isbn"]; got != "9780261103283" {
		t.Errorf("isbn identifier = %q, want 9780261103283", got)
	}
}

func TestRead_MissingTitleFallsBack(t *testing.T) {
	p := writeEPUB(t, map[string]string{
		"META-INF/container.xml": containerXML,
		"OEBPS/content.opf":      opf("", "", ""),
	})
	m, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Untitled" {
		t.Errorf("empty title should fall back to Untitled, got %q", m.Title)
	}
}

func TestRead_NotAnEPUB(t *testing.T) {
	// A zip with no container.xml must error, not panic.
	p := writeEPUB(t, map[string]string{"random.txt": "hi"})
	if _, err := Read(p); err == nil {
		t.Fatal("expected error for zip without container.xml")
	}
	// A non-zip file must error too.
	bad := filepath.Join(t.TempDir(), "notzip.epub")
	os.WriteFile(bad, []byte("not a zip"), 0o644)
	if _, err := Read(bad); err == nil {
		t.Fatal("expected error for non-zip file")
	}
}

func TestIsADEPTEncrypted_Synthetic(t *testing.T) {
	adeptRights := `<?xml version="1.0"?><rights xmlns="http://ns.adobe.com/adept"><licenseToken/></rights>`
	adeptEnc := `<?xml version="1.0"?><encryption xmlns:enc="http://www.w3.org/2001/04/xmlenc#">
		<enc:EncryptedData><KeyInfo xmlns="http://ns.adobe.com/adept"/></enc:EncryptedData></encryption>`
	// Font obfuscation uses the IDPF namespace, NOT Adobe — must read as clean.
	idpfFontEnc := `<?xml version="1.0"?><encryption xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
		<EncryptedData><EncryptionMethod Algorithm="http://www.idpf.org/2008/embedding"/></EncryptedData></encryption>`

	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"adept rights.xml", map[string]string{
			"META-INF/container.xml": containerXML,
			"OEBPS/content.opf":      opf("X", "Y", ""),
			"META-INF/rights.xml":    adeptRights,
		}, true},
		{"adept encryption.xml only", map[string]string{
			"META-INF/container.xml":  containerXML,
			"OEBPS/content.opf":       opf("X", "Y", ""),
			"META-INF/encryption.xml": adeptEnc,
		}, true},
		{"clean (no drm files)", map[string]string{
			"META-INF/container.xml": containerXML,
			"OEBPS/content.opf":      opf("X", "Y", ""),
		}, false},
		{"font obfuscation only (not adept)", map[string]string{
			"META-INF/container.xml":  containerXML,
			"OEBPS/content.opf":       opf("X", "Y", ""),
			"META-INF/encryption.xml": idpfFontEnc,
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeEPUB(t, c.files)
			got, err := IsADEPTEncrypted(p)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("IsADEPTEncrypted = %v, want %v", got, c.want)
			}
		})
	}
}

func TestGuessIdentifierScheme(t *testing.T) {
	cases := []struct{ in, want string }{
		{"urn:isbn:9780261103283", "isbn"},
		{"9780261103283", "isbn"},
		{"978-0-261-10328-3", "isbn"},
		{"urn:uuid:806f4b05-088a-475f-a112-c04571bca9e2", "uuid"},
		{"http://www.gutenberg.org/84", "unknown"},
	}
	for _, c := range cases {
		if got := guessIdentifierScheme(c.in); got != c.want {
			t.Errorf("guessIdentifierScheme(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMediaTypeForCover(t *testing.T) {
	cases := map[string]string{
		"cover.png":  "image/png",
		"cover.PNG":  "image/png",
		"cover.gif":  "image/gif",
		"cover.svg":  "image/svg+xml",
		"cover.jpg":  "image/jpeg",
		"cover.jpeg": "image/jpeg",
		"cover":      "image/jpeg",
	}
	for in, want := range cases {
		if got := mediaTypeForCover(in); got != want {
			t.Errorf("mediaTypeForCover(%q) = %q, want %q", in, got, want)
		}
	}
}
