// Package web serves the browser catalog UI, the epub.js reader, raw file and
// cover downloads, and the small JSON API.
package web

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steve/library/internal/catalog"
	"github.com/steve/library/internal/epub"
	"github.com/steve/library/internal/fileutil"
)

//go:embed assets/*
var assets embed.FS

// Server holds dependencies for the web handlers.
type Server struct {
	Cat *catalog.Catalog
	// ImportDir is where browser uploads land, so the import watcher picks them
	// up and runs the same fulfill/decrypt pipeline as a manual drop.
	ImportDir string
	tpl       *template.Template
}

// New parses templates and returns a Server.
func New(cat *catalog.Catalog, importDir string) (*Server, error) {
	tpl, err := template.ParseFS(assets, "assets/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{Cat: cat, ImportDir: importDir, tpl: tpl}, nil
}

// Register wires the browser + API routes onto mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /", s.index)
	// Public book URLs key on the stable {slug} (a content-hash prefix), not the
	// autoincrement DB id, so a book's URL survives catalog rebuilds.
	mux.HandleFunc("GET /read/{slug}", s.reader)
	mux.HandleFunc("GET /book/{slug}/file", s.file)
	mux.HandleFunc("GET /book/{slug}/cover", s.cover)
	mux.HandleFunc("GET /api/books", s.apiBooks)
	mux.HandleFunc("PUT /api/books/{slug}/read", s.apiSaveRead)
	mux.HandleFunc("POST /api/scan", s.apiScan)
	mux.HandleFunc("POST /api/upload", s.apiUpload)
	// Static JS/CSS (epub.js lives here once vendored).
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(mustSub(assets, "assets"))))
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	books, err := s.Cat.List(r.Context(), catalog.ListOptions{
		Query: r.URL.Query().Get("q"),
		Limit: 200,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.render(w, "index.html", map[string]any{
		"Books":    books,
		"Query":    r.URL.Query().Get("q"),
		"Uploaded": r.URL.Query().Get("uploaded"),
	})
}

func (s *Server) reader(w http.ResponseWriter, r *http.Request) {
	b, err := s.book(r.Context(), r)
	if err != nil {
		s.bookErr(w, err)
		return
	}
	s.render(w, "reader.html", map[string]any{"Book": b})
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	b, err := s.book(r.Context(), r)
	if err != nil {
		s.bookErr(w, err)
		return
	}
	// Content-Disposition gives the X4 (and browsers) a sane filename.
	w.Header().Set("Content-Type", "application/epub+zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileutil.SafeFilename(b.Title)+".epub"))
	http.ServeFile(w, r, s.Cat.AbsPath(b))
}

func (s *Server) cover(w http.ResponseWriter, r *http.Request) {
	b, err := s.book(r.Context(), r)
	if err != nil {
		s.bookErr(w, err)
		return
	}
	// Fast path: serve the pre-extracted cover from the cache if present.
	if p := s.Cat.CoverCachePath(b); p != "" {
		http.ServeFile(w, r, p)
		return
	}
	// Miss (e.g. a book indexed before the cache existed): extract live, populate
	// the cache for next time, and serve.
	data, mime, err := epub.CoverImage(s.Cat.AbsPath(b))
	if err != nil {
		http.Error(w, "no cover", http.StatusNotFound)
		return
	}
	s.Cat.CacheCoverData(b, data, mime)
	w.Header().Set("Content-Type", mime)
	_, _ = w.Write(data)
}

func (s *Server) apiBooks(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	books, err := s.Cat.List(r.Context(), catalog.ListOptions{
		Query:  r.URL.Query().Get("q"),
		Author: r.URL.Query().Get("author"),
		Series: r.URL.Query().Get("series"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, books)
}

func (s *Server) apiSaveRead(w http.ResponseWriter, r *http.Request) {
	b, err := s.book(r.Context(), r)
	if err != nil {
		s.bookErr(w, err)
		return
	}
	var body struct {
		Percent float64 `json:"percent"`
		CFI     string  `json:"cfi"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", 400)
		return
	}
	if err := s.Cat.SaveReadState(r.Context(), b.ID, body.Percent, body.CFI); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiUpload accepts an .acsm or .epub via multipart form and drops it into the
// import dir, where the watcher runs the same fulfill/decrypt pipeline as a
// manual file drop. Written atomically (temp + rename) so the watcher never
// sees a partial file.
func (s *Server) apiUpload(w http.ResponseWriter, r *http.Request) {
	if s.ImportDir == "" {
		http.Error(w, "uploads not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil { // 64 MiB in memory, rest to disk
		http.Error(w, "bad upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file field", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if ext != ".acsm" && ext != ".epub" {
		http.Error(w, "only .acsm or .epub accepted", http.StatusUnsupportedMediaType)
		return
	}
	name := fileutil.SafeFilename(strings.TrimSuffix(filepath.Base(hdr.Filename), ext)) + ext

	tmp, err := os.CreateTemp(s.ImportDir, ".upload-*"+ext)
	if err != nil {
		http.Error(w, "cannot stage upload: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, file); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		http.Error(w, "write failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = tmp.Close()

	dest := filepath.Join(s.ImportDir, name)
	if err := os.Rename(tmpPath, dest); err != nil {
		_ = os.Remove(tmpPath)
		http.Error(w, "finalize failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// HTML form posts get redirected back to the library; API clients get JSON.
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, map[string]string{"queued": name})
		return
	}
	http.Redirect(w, r, "/?uploaded="+name, http.StatusSeeOther)
}

func (s *Server) apiScan(w http.ResponseWriter, r *http.Request) {
	n, err := s.Cat.Scan(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]int{"indexed": n})
}

// --- helpers --------------------------------------------------------------

func (s *Server) book(ctx context.Context, r *http.Request) (*catalog.Book, error) {
	return s.Cat.GetBySlug(ctx, r.PathValue("slug"))
}

func (s *Server) bookErr(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "book not found", http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusBadRequest)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func mustSub(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
