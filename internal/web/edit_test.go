package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steve/library/internal/catalog"
	"github.com/steve/library/internal/epub"
)

// writeLibraryEPUB writes a minimal epub into the catalog's library dir and
// returns its absolute path.
func writeLibraryEPUB(t *testing.T, libraryDir, rel, title, author string) string {
	t.Helper()
	p := filepath.Join(libraryDir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	mw, _ := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	_, _ = mw.Write([]byte("application/epub+zip"))
	add := func(n, c string) { w, _ := zw.Create(n); _, _ = w.Write([]byte(c)) }
	add("META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
<rootfiles><rootfile full-path="content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`)
	add("content.opf", `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
<dc:title>`+title+`</dc:title><dc:creator>`+author+`</dc:creator><dc:language>en</dc:language></metadata>
<manifest><item id="c" href="c.xhtml" media-type="application/xhtml+xml"/></manifest></package>`)
	add("c.xhtml", "<html><body>hi</body></html>")
	_ = zw.Close()
	return p
}

// libraryDir returns the catalog's library root for a test server.
func libraryDir(t *testing.T, s *Server) string {
	t.Helper()
	return s.Cat.LibraryRoot()
}

// waitEmbedDone polls the import-job registry until the given embed job reaches a
// terminal state (the embed runs in the background after the PUT returns).
func waitEmbedDone(t *testing.T, s *Server, jobID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, j := range s.Jobs.Snapshot() {
			if j.ID == jobID && !j.EndedAt.IsZero() {
				if j.State == "failed" {
					t.Fatalf("embed job failed: %s", j.Err)
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("embed job %s did not finish within 5s", jobID)
}

// TestEditUpdatesDBAndEmbedsFile is the step-4 end-to-end: a PUT edits the
// catalog AND embeds into the epub file, and the response reports embedded:true.
func TestEditUpdatesDBAndEmbedsFile(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	lib := libraryDir(t, s)
	writeLibraryEPUB(t, lib, "Old Author/Old Title.epub", "Old Title", "Old Author")
	if _, err := s.Cat.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	books, _ := s.Cat.List(context.Background(), catalog.ListOptions{})
	if len(books) != 1 {
		t.Fatalf("want 1 book, got %d", len(books))
	}
	slug := books[0].Slug()

	body := `{"title":"New Title","authors":"New Author"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/books/"+slug, strings.NewReader(body))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Book  struct{ Title string }
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if resp.Book.Title != "New Title" {
		t.Errorf("response title = %q, want New Title", resp.Book.Title)
	}
	if resp.JobID == "" {
		t.Fatal("expected a jobId for the background embed")
	}
	// The embed runs in the background; wait for its job to finish.
	waitEmbedDone(t, s, resp.JobID)

	// DB reflects the edit.
	got, _ := s.Cat.GetBySlug(context.Background(), slug)
	if got.Title != "New Title" || len(got.Authors) != 1 || got.Authors[0] != "New Author" {
		t.Errorf("catalog not updated: %+v", got)
	}
	// The file on disk was rewritten AND relocated to the new Author/Title path.
	abs := s.Cat.AbsPath(got)
	if !strings.HasSuffix(abs, filepath.Join("New Author", "New Title.epub")) {
		t.Errorf("file not relocated to new path: %s", abs)
	}
	m, err := epub.Read(abs)
	if err != nil {
		t.Fatalf("embedded epub does not parse: %v", err)
	}
	if m.Title != "New Title" || m.Authors[0] != "New Author" {
		t.Errorf("file metadata not embedded: %+v", m)
	}
}

// TestEditAudiobookFormAndNarrator covers the tailored audiobook edit path: the
// edit form renders a Narrator field (and not Series #), and a narrator edit
// round-trips through the handler with NO embed job (audio is catalog-only).
func TestEditAudiobookFormAndNarrator(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	slug := indexM4B(t, s) // narrator "The Narrator"

	// The edit FORM is audio-tailored.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/book/"+slug+"/edit", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/edit status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="narrator"`) {
		t.Error("audio edit form should have a Narrator field")
	}
	if strings.Contains(body, `name="seriesIndex"`) {
		t.Error("audio edit form should NOT have a Series # field")
	}

	// A narrator edit round-trips with no embed job.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/books/"+slug, strings.NewReader(`{"narrator":"New Voice"}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		JobID  string `json:"jobId"`
		Embeds bool   `json:"embeds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Embeds {
		t.Error("audiobook edit should report embeds=false")
	}
	if resp.JobID != "" {
		t.Error("audiobook edit should not start an embed job")
	}
	got, _ := s.Cat.GetBySlug(context.Background(), slug)
	if got.Narrator != "New Voice" {
		t.Errorf("narrator not updated: %q, want New Voice", got.Narrator)
	}
}

// TestEditMissingBookIs404 confirms an unknown slug is a clean 404.
func TestEditMissingBookIs404(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/books/deadbeefdeadbeef", strings.NewReader(`{"title":"x"}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown slug: status = %d, want 404", rec.Code)
	}
}

// TestEditPageServes verifies the edit form renders with the fields + edit.js,
// and that edit.js serves 200 (static frontend verification, per convention).
func TestEditPageServes(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	lib := libraryDir(t, s)
	writeLibraryEPUB(t, lib, "A/B.epub", "B", "A")
	_, _ = s.Cat.Scan(context.Background())
	books, _ := s.Cat.List(context.Background(), catalog.ListOptions{})
	slug := books[0].Slug()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/book/"+slug+"/edit", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/edit status = %d, want 200", rec.Code)
	}
	for _, want := range []string{`id="edit-form"`, `name="title"`, `/static/js/edit.js`, `action="/book/` + slug + `/edit"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("edit page missing %q", want)
		}
	}
	ar := httptest.NewRecorder()
	mux.ServeHTTP(ar, httptest.NewRequest(http.MethodGet, "/static/js/edit.js", nil))
	if ar.Code != http.StatusOK {
		t.Errorf("edit.js status = %d, want 200", ar.Code)
	}
}

// tinyPNGBytes is a real 1x1 PNG, used as a valid cover upload.
var tinyPNGBytes = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
	0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

// TestCoverOverrideWinsAndRejectsNonImage covers the cover-override path: a valid
// image upload becomes the served cover (winning over the extracted cache), and a
// non-image blob is rejected.
func TestCoverOverrideWinsAndRejectsNonImage(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	lib := libraryDir(t, s)
	writeLibraryEPUB(t, lib, "A/B.epub", "B", "A")
	_, _ = s.Cat.Scan(context.Background())
	books, _ := s.Cat.List(context.Background(), catalog.ListOptions{})
	slug := books[0].Slug()

	// A non-image upload is rejected (415), nothing stored.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/book/"+slug+"/cover", strings.NewReader("not an image")))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("non-image cover: status = %d, want 415", rec.Code)
	}

	// A valid PNG is accepted and becomes the served cover.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/book/"+slug+"/cover", bytes.NewReader(tinyPNGBytes)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cover upload: status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}

	// The override lives in overrides/, isolated from the cache, and GET /cover
	// serves exactly the override bytes.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/book/"+slug+"/cover", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET cover: status = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), tinyPNGBytes) {
		t.Error("served cover is not the uploaded override")
	}

	// The override survives a rescan (it is not the cache; the extractor must not
	// touch overrides/).
	_, _ = s.Cat.Scan(context.Background())
	b, _ := s.Cat.GetBySlug(context.Background(), slug)
	if s.Cat.CoverOverridePath(b) == "" {
		t.Error("override was wiped/overwritten by a rescan")
	}
}

// TestDeleteBookRemovesRowFileAndCover is the delete end-to-end: DELETE removes
// the catalog row, the file on disk, and any cover override, and the book is gone
// from listings (and stays gone after a rescan).
func TestDeleteBookRemovesRowFileAndCover(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	lib := libraryDir(t, s)
	abs := writeLibraryEPUB(t, lib, "Auth/Book.epub", "Book", "Auth")
	_, _ = s.Cat.Scan(context.Background())
	books, _ := s.Cat.List(context.Background(), catalog.ListOptions{})
	if len(books) != 1 {
		t.Fatalf("want 1 book, got %d", len(books))
	}
	slug := books[0].Slug()

	// Give it a cover override so we can confirm that's cleaned up too.
	if _, err := os.Stat(abs); err != nil {
		t.Fatal(err)
	}
	if err := s.Cat.SetCoverOverride(context.Background(), books[0], tinyPNGBytes, "image/png"); err != nil {
		t.Fatal(err)
	}
	if s.Cat.CoverOverridePath(books[0]) == "" {
		t.Fatal("override not set up")
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/books/"+slug, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}

	// Row gone.
	if _, err := s.Cat.GetBySlug(context.Background(), slug); err == nil {
		t.Error("catalog row still present after delete")
	}
	// File gone.
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Errorf("book file still on disk after delete: %v", err)
	}
	// Override gone.
	if s.Cat.CoverOverridePath(books[0]) != "" {
		t.Error("cover override not cleaned up")
	}
	// Stays gone after a rescan (file removed, so nothing to re-import).
	_, _ = s.Cat.Scan(context.Background())
	if got, _ := s.Cat.List(context.Background(), catalog.ListOptions{}); len(got) != 0 {
		t.Errorf("book reappeared after rescan: %d", len(got))
	}
}

// TestDeleteMissingBookIs404 confirms an unknown slug deletes to a clean 404.
func TestDeleteMissingBookIs404(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/books/deadbeefdeadbeef", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown slug: status = %d, want 404", rec.Code)
	}
}

// TestIndexRendersBookMenu verifies the grid renders the three-dot actions menu
// (Edit + Delete) for each book, not a bare edit link.
func TestIndexRendersBookMenu(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	writeLibraryEPUB(t, libraryDir(t, s), "A/B.epub", "B", "A")
	_, _ = s.Cat.Scan(context.Background())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("index status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`class="book-menu"`, `class="menu-delete"`, `/edit"`} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
}
