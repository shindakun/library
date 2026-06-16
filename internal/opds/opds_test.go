package opds

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steve/library/internal/catalog"
)

// parsedFeed is a minimal Atom/OPDS feed shape for asserting on responses.
type parsedFeed struct {
	Links []struct {
		Rel  string `xml:"rel,attr"`
		Href string `xml:"href,attr"`
		Type string `xml:"type,attr"`
	} `xml:"link"`
	Entries []struct {
		Title string `xml:"title"`
		Links []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
			Type string `xml:"type,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

func (f parsedFeed) linkRel(rel string) (string, bool) {
	for _, l := range f.Links {
		if l.Rel == rel {
			return l.Href, true
		}
	}
	return "", false
}

// newHandler builds an OPDS handler over a real catalog seeded with n books.
func newHandler(t *testing.T, n int) *Handler {
	t.Helper()
	dir := t.TempDir()
	books := filepath.Join(dir, "library")
	_ = os.MkdirAll(books, 0o755)
	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"), books, filepath.Join(dir, "covers"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	for i := 0; i < n; i++ {
		writeMinEPUB(t, books, fmt.Sprintf("b%03d.epub", i), fmt.Sprintf("Book %03d", i), "Author")
	}
	if _, err := cat.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &Handler{Cat: cat, BaseURL: "http://test.local"}
}

func writeMinEPUB(t *testing.T, dir, file, title, author string) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	add := func(n, c string) { w, _ := zw.Create(n); _, _ = w.Write([]byte(c)) }
	add("META-INF/container.xml", `<?xml version="1.0"?><container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`)
	add("content.opf", `<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" version="3.0"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>`+title+`</dc:title><dc:creator>`+author+`</dc:creator></metadata><manifest><item id="c" href="c.xhtml" media-type="application/xhtml+xml"/></manifest></package>`)
	add("c.xhtml", "<html/>")
	_ = zw.Close()
}

func do(t *testing.T, h *Handler, path string) (*http.Response, parsedFeed) {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	res := rec.Result()
	var f parsedFeed
	if strings.Contains(res.Header.Get("Content-Type"), "xml") || strings.HasPrefix(rec.Body.String(), "<?xml") {
		if err := xml.Unmarshal(rec.Body.Bytes(), &f); err != nil {
			t.Fatalf("parse feed for %s: %v\nbody: %s", path, err, rec.Body.String())
		}
	}
	return res, f
}

// TestRootIsNavigationOnly enforces that the root feed lists NO books, only
// bounded subsection links. This is what keeps the X4 from being handed the
// whole library at the entry point.
func TestRootIsNavigationOnly(t *testing.T) {
	h := newHandler(t, 50)
	_, f := do(t, h, "/opds")
	for _, e := range f.Entries {
		for _, l := range e.Links {
			if l.Rel == relAcq {
				t.Fatalf("root feed must not contain acquisition (book) entries; found %q", e.Title)
			}
		}
	}
	if _, ok := f.linkRel(relSearch); !ok {
		t.Error("root feed should advertise a search link")
	}
}

// TestAcquisitionPaging is the core X4-safety invariant: a feed is never larger
// than PageSize, and next/prev links bound the pages correctly.
func TestAcquisitionPaging(t *testing.T) {
	const total = 70 // 2 full pages of 30 + a partial page of 10
	h := newHandler(t, total)

	// Page 0: exactly PageSize entries, a next link, no prev.
	_, p0 := do(t, h, "/opds/all")
	if len(p0.Entries) != PageSize {
		t.Errorf("page 0 has %d entries, want PageSize=%d", len(p0.Entries), PageSize)
	}
	if _, ok := p0.linkRel(relNext); !ok {
		t.Error("page 0 should have a next link")
	}
	if _, ok := p0.linkRel(relPrev); ok {
		t.Error("page 0 should NOT have a prev link")
	}

	// Page 1: full, both next and prev.
	_, p1 := do(t, h, "/opds/all?page=1")
	if len(p1.Entries) != PageSize {
		t.Errorf("page 1 has %d entries, want %d", len(p1.Entries), PageSize)
	}
	if _, ok := p1.linkRel(relNext); !ok {
		t.Error("page 1 should have a next link")
	}
	if _, ok := p1.linkRel(relPrev); !ok {
		t.Error("page 1 should have a prev link")
	}

	// Page 2: the partial last page (10 entries), prev but no next.
	_, p2 := do(t, h, "/opds/all?page=2")
	if len(p2.Entries) != total-2*PageSize {
		t.Errorf("page 2 has %d entries, want %d", len(p2.Entries), total-2*PageSize)
	}
	if _, ok := p2.linkRel(relNext); ok {
		t.Error("last page should NOT have a next link")
	}
	if _, ok := p2.linkRel(relPrev); !ok {
		t.Error("last page should have a prev link")
	}
}

func TestExactlyOnePageHasNoPaging(t *testing.T) {
	h := newHandler(t, PageSize) // exactly one full page
	_, f := do(t, h, "/opds/all")
	if len(f.Entries) != PageSize {
		t.Fatalf("got %d entries, want %d", len(f.Entries), PageSize)
	}
	if _, ok := f.linkRel(relNext); ok {
		t.Error("a single exactly-full page must not advertise a next link")
	}
}

// TestAcquisitionEntryShape verifies each book entry carries the epub
// acquisition link the X4 downloads from.
func TestAcquisitionEntryShape(t *testing.T) {
	h := newHandler(t, 1)
	_, f := do(t, h, "/opds/all")
	if len(f.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(f.Entries))
	}
	var hasAcq bool
	for _, l := range f.Entries[0].Links {
		if l.Rel == relAcq {
			hasAcq = true
			if l.Type != typeEpub {
				t.Errorf("acquisition link type = %q, want %q", l.Type, typeEpub)
			}
			if !strings.HasPrefix(l.Href, "http://test.local/book/") {
				t.Errorf("acquisition href = %q, want absolute book URL", l.Href)
			}
		}
	}
	if !hasAcq {
		t.Error("book entry missing an acquisition link")
	}
}

func TestSearchFeedIsPaged(t *testing.T) {
	h := newHandler(t, 40) // all titles contain "Book", so search matches all
	_, f := do(t, h, "/opds/search?q=Book")
	if len(f.Entries) != PageSize {
		t.Errorf("search page has %d entries, want capped at %d", len(f.Entries), PageSize)
	}
	if _, ok := f.linkRel(relNext); !ok {
		t.Error("a search matching >PageSize books must page")
	}
}
