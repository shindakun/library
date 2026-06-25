package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/steve/library/internal/catalog"
)

// TestEpubReaderResumesFromSavedCFI proves the epub reader round-trip: a saved
// reading position (an epub.js CFI string) is injected into the reader page as
// window.START_CFI, so reader.js can resume there. This is the bug fix, the
// reader previously called rendition.display() with no argument and always
// opened at the start.
func TestEpubReaderResumesFromSavedCFI(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	writeLibraryEPUB(t, libraryDir(t, s), "A/B.epub", "B", "A")
	if _, err := s.Cat.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	books, _ := s.Cat.List(context.Background(), catalog.ListOptions{})
	if len(books) != 1 {
		t.Fatalf("want 1 book, got %d", len(books))
	}
	b := books[0]
	slug := b.Slug()

	// Before any save, the reader injects an empty START_CFI (opens at start).
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/read/"+slug, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/read status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `window.START_CFI = "";`) {
		t.Errorf("fresh book: expected empty START_CFI, got:\n%s", scriptHead(rec.Body.String()))
	}

	// Save a position via the SAME endpoint reader.js uses.
	cfi := "epubcfi(/6/14[id4]!/4/2/2[ch1]/1:0)"
	pr := httptest.NewRequest(http.MethodPut, "/api/books/"+slug+"/read",
		strings.NewReader(`{"cfi":`+jsonQuote(cfi)+`,"percent":0.25}`))
	pr.Header.Set("Content-Type", "application/json")
	prec := httptest.NewRecorder()
	mux.ServeHTTP(prec, pr)
	if prec.Code != http.StatusNoContent {
		t.Fatalf("save status = %d, want 204; body=%s", prec.Code, prec.Body.String())
	}

	// The reader page now injects that CFI so reader.js resumes there.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/read/"+slug, nil))
	body := rec.Body.String()
	if !strings.Contains(body, `window.START_CFI`) {
		t.Fatalf("reader page has no START_CFI global:\n%s", scriptHead(body))
	}
	// The CFI value must be present (html/template JS-escapes it; the [ and ]
	// survive as-is, the value must be recoverable).
	if !strings.Contains(body, "epubcfi(") || !strings.Contains(body, "ch1") {
		t.Errorf("START_CFI does not carry the saved CFI:\n%s", scriptHead(body))
	}
}

// jsonQuote returns a JSON-quoted string literal for embedding in a request body.
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// scriptHead returns the inline <script> block for error messages.
func scriptHead(html string) string {
	i := strings.Index(html, "window.BOOK_SLUG")
	if i < 0 {
		return html[:min(len(html), 400)]
	}
	end := i + 300
	if end > len(html) {
		end = len(html)
	}
	return html[i:end]
}
