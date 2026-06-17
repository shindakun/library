package web

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tinyPNG is a 1x1 PNG used as comic page content.
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
	0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

// indexComic writes a CBZ into the server's library dir and indexes it, returning
// the book's public slug.
func indexComic(t *testing.T, s *Server, name string, pages int) string {
	t.Helper()
	root := s.Cat.LibraryRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("ComicInfo.xml")
	_, _ = w.Write([]byte(`<ComicInfo><Title>Test Comic</Title></ComicInfo>`))
	for i := 1; i <= pages; i++ {
		pw, _ := zw.Create(fmt.Sprintf("page%02d.png", i))
		_, _ = pw.Write(tinyPNG)
	}
	_ = zw.Close()
	_ = f.Close()

	id, err := s.Cat.Index(context.Background(), p, "comic-import")
	if err != nil {
		t.Fatalf("index comic: %v", err)
	}
	b, err := s.Cat.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Slug()
}

func TestComicReaderRendersComicTemplate(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	slug := indexComic(t, s, "Test Comic.cbz", 3)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/read/"+slug, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/read status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`id="comic-page"`, `/static/js/comic.js`, `window.START_PAGE`} {
		if !strings.Contains(body, want) {
			t.Errorf("comic reader page missing %q", want)
		}
	}
	// Must NOT be the epub reader.
	if strings.Contains(body, "epub.min.js") {
		t.Error("comic served the epub reader template")
	}
}

func TestComicPagesAndPageEndpoints(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	slug := indexComic(t, s, "Test Comic.cbz", 3)

	// Page count.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/book/"+slug+"/pages", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/pages status = %d", rec.Code)
	}
	var cnt struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cnt); err != nil {
		t.Fatalf("decode pages: %v; body=%s", err, rec.Body.String())
	}
	if cnt.Count != 3 {
		t.Errorf("page count = %d, want 3", cnt.Count)
	}

	// A real page image.
	pr := httptest.NewRecorder()
	mux.ServeHTTP(pr, httptest.NewRequest(http.MethodGet, "/book/"+slug+"/page/0", nil))
	if pr.Code != http.StatusOK {
		t.Fatalf("/page/0 status = %d", pr.Code)
	}
	if ct := pr.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("page content-type = %q, want image/png", ct)
	}
	if pr.Body.Len() == 0 {
		t.Error("page 0 returned no bytes")
	}

	// Out-of-range page -> 404.
	or := httptest.NewRecorder()
	mux.ServeHTTP(or, httptest.NewRequest(http.MethodGet, "/book/"+slug+"/page/99", nil))
	if or.Code != http.StatusNotFound {
		t.Errorf("/page/99 status = %d, want 404", or.Code)
	}
}

func TestComicFileDownloadMediaType(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	slug := indexComic(t, s, "Test Comic.cbz", 2)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/book/"+slug+"/file", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/file status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.comicbook+zip" {
		t.Errorf("comic download content-type = %q, want application/vnd.comicbook+zip", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, ".cbz") {
		t.Errorf("comic download filename not .cbz: %q", cd)
	}
}
