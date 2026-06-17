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
	"time"

	"github.com/steve/library/internal/catalog"
	"github.com/steve/library/internal/comic"
	"github.com/steve/library/internal/drm"
	"github.com/steve/library/internal/fileutil"
	"github.com/steve/library/internal/ingest"
)

//go:embed assets/*
var assets embed.FS

// Server holds dependencies for the web handlers.
type Server struct {
	Cat *catalog.Catalog
	// ImportDir is where browser uploads land, so the import watcher picks them
	// up and runs the same fulfill/decrypt pipeline as a manual drop.
	ImportDir string
	// DRM drives the sidecar; used for the first-run setup form (check whether
	// Adobe is configured, and run setup). May be nil (setup form disabled).
	DRM *drm.Client
	// Jobs is the import-job registry, read for the /imports page + SSE stream.
	// May be nil (the imports endpoints then report an empty list).
	Jobs *ingest.Jobs
	tpl  *template.Template
}

// New parses templates and returns a Server.
func New(cat *catalog.Catalog, importDir string, drmClient *drm.Client, jobs *ingest.Jobs) (*Server, error) {
	tpl, err := template.ParseFS(assets, "assets/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{Cat: cat, ImportDir: importDir, DRM: drmClient, Jobs: jobs, tpl: tpl}, nil
}

// Register wires the browser + API routes onto mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /", s.index)
	// Public book URLs key on the stable {slug} (a content-hash prefix), not the
	// autoincrement DB id, so a book's URL survives catalog rebuilds.
	mux.HandleFunc("GET /read/{slug}", s.reader)
	mux.HandleFunc("GET /book/{slug}/file", s.file)
	mux.HandleFunc("GET /book/{slug}/cover", s.cover)
	mux.HandleFunc("GET /book/{slug}/pages", s.comicPages)
	mux.HandleFunc("GET /book/{slug}/page/{n}", s.comicPage)
	mux.HandleFunc("GET /api/books", s.apiBooks)
	mux.HandleFunc("PUT /api/books/{slug}/read", s.apiSaveRead)
	mux.HandleFunc("POST /api/scan", s.apiScan)
	mux.HandleFunc("POST /api/upload", s.apiUpload)
	mux.HandleFunc("POST /api/setup", s.apiSetup)
	mux.HandleFunc("GET /imports", s.imports)
	mux.HandleFunc("GET /api/imports", s.apiImports)
	mux.HandleFunc("GET /api/imports/stream", s.apiImportsStream)
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
		"Books":      books,
		"Query":      r.URL.Query().Get("q"),
		"Uploaded":   r.URL.Query().Get("uploaded"),
		"NeedsSetup": s.needsSetup(r.Context()),
	})
}

func (s *Server) reader(w http.ResponseWriter, r *http.Request) {
	b, err := s.book(r.Context(), r)
	if err != nil {
		s.bookErr(w, err)
		return
	}
	if b.Format == "cbz" {
		// Restore the saved page (cfi holds the page number for comics).
		page := 0
		if _, cfi := s.Cat.ReadState(r.Context(), b.ID); cfi != "" {
			if n, perr := strconv.Atoi(cfi); perr == nil && n >= 0 {
				page = n
			}
		}
		s.render(w, "comic.html", map[string]any{"Book": b, "StartPage": page})
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
	// Content type + download filename match the format so e-readers and OPDS
	// clients get the right media type (comics download as .cbz).
	ctype, ext := "application/epub+zip", ".epub"
	if b.Format == "cbz" {
		ctype, ext = "application/vnd.comicbook+zip", ".cbz"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileutil.SafeFilename(b.Title)+ext))
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
	// the cache for next time, and serve. Format-neutral so comics work too.
	data, mime, err := catalog.CoverImageFor(s.Cat.AbsPath(b))
	if err != nil {
		http.Error(w, "no cover", http.StatusNotFound)
		return
	}
	s.Cat.CacheCoverData(b, data, mime)
	w.Header().Set("Content-Type", mime)
	_, _ = w.Write(data)
}

// comicPages returns the page count for a comic as {"count": N}. The comic
// viewer fetches this once to size its pager.
func (s *Server) comicPages(w http.ResponseWriter, r *http.Request) {
	b, err := s.book(r.Context(), r)
	if err != nil {
		s.bookErr(w, err)
		return
	}
	if b.Format != "cbz" {
		http.Error(w, "not a comic", http.StatusNotFound)
		return
	}
	pages, err := comic.Pages(s.Cat.AbsPath(b))
	if err != nil {
		http.Error(w, "cannot read comic", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]int{"count": len(pages)})
}

// comicPage serves the nth page image (0-based) of a comic, straight from the
// archive with the page's own image mime type.
func (s *Server) comicPage(w http.ResponseWriter, r *http.Request) {
	b, err := s.book(r.Context(), r)
	if err != nil {
		s.bookErr(w, err)
		return
	}
	if b.Format != "cbz" {
		http.Error(w, "not a comic", http.StatusNotFound)
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 {
		http.Error(w, "bad page number", http.StatusBadRequest)
		return
	}
	data, mime, err := comic.PageImage(s.Cat.AbsPath(b), n)
	if err != nil {
		http.Error(w, "no such page", http.StatusNotFound)
		return
	}
	// Pages inside a comic are immutable for the life of the slug (content hash),
	// so let the browser cache them aggressively while paging back and forth.
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=86400")
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

// uploadableExt reports whether an uploaded file's extension is one the import
// pipeline accepts. Kept in sync with ingest.importable: .acsm/.epub go through
// the DRM pipeline, .cbz/.cbr are comics (a .cbr is converted to .cbz at import).
func uploadableExt(ext string) bool {
	switch ext {
	case ".acsm", ".epub", ".cbz", ".cbr":
		return true
	default:
		return false
	}
}

// apiUpload accepts an .acsm, .epub, or .cbz via multipart form and drops it
// into the import dir, where the watcher runs the same pipeline as a manual file
// drop. Written atomically (temp + rename) so the watcher never sees a partial
// file.
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
	if !uploadableExt(ext) {
		http.Error(w, "only .acsm, .epub, .cbz, or .cbr accepted", http.StatusUnsupportedMediaType)
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

// apiSetup runs one-time Adobe authorization from the first-run form. It
// forwards the AdobeID/password/version to the sidecar, which writes /secrets.
// Refuses once already configured (the sidecar enforces this too).
func (s *Server) apiSetup(w http.ResponseWriter, r *http.Request) {
	if s.DRM == nil {
		http.Error(w, "DRM sidecar not configured", http.StatusServiceUnavailable)
		return
	}
	mail := r.FormValue("mail")
	password := r.FormValue("password")
	ver, _ := strconv.Atoi(r.FormValue("ade_version"))
	if ver != 1 && ver != 2 {
		ver = 1
	}
	if mail == "" || password == "" {
		http.Error(w, "AdobeID email and password are required", http.StatusBadRequest)
		return
	}
	if err := s.DRM.Setup(r.Context(), mail, password, ver); err != nil {
		http.Error(w, "setup failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, map[string]bool{"configured": true})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// needsSetup reports whether the first-run setup form should be shown: a DRM
// sidecar exists, is reachable, and is not yet configured. Any uncertainty
// (no sidecar, unreachable) returns false so the normal library still renders.
func (s *Server) needsSetup(ctx context.Context) bool {
	if s.DRM == nil {
		return false
	}
	configured, err := s.DRM.Configured(ctx)
	if err != nil {
		return false // sidecar unreachable; don't block the library on it
	}
	return !configured
}

func (s *Server) apiScan(w http.ResponseWriter, r *http.Request) {
	n, err := s.Cat.Scan(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]int{"indexed": n})
}

// imports renders the dedicated import page (upload control + live job list).
func (s *Server) imports(w http.ResponseWriter, r *http.Request) {
	s.render(w, "imports.html", nil)
}

// apiImports returns a snapshot of current/recent import jobs. Used for the
// initial page load and as a reconnect fallback for the SSE stream.
func (s *Server) apiImports(w http.ResponseWriter, r *http.Request) {
	if s.Jobs == nil {
		writeJSON(w, []*ingest.Job{})
		return
	}
	writeJSON(w, s.Jobs.Snapshot())
}

// apiImportsStream is a Server-Sent Events stream of import-job updates. It
// sends the current snapshot first (so a late-joining client is immediately
// correct), then one event per job change until the client disconnects.
func (s *Server) apiImportsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok || s.Jobs == nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disable proxy buffering so events flush through (no-op without such a proxy).
	w.Header().Set("X-Accel-Buffering", "no")

	// Subscribe BEFORE snapshotting so no update is missed in the gap; a
	// duplicate event for a job already in the snapshot is harmless (the client
	// keys rows by job id and overwrites).
	updates, unsub := s.Jobs.Subscribe()
	defer unsub()

	for _, job := range s.Jobs.Snapshot() {
		if !writeSSE(w, job) {
			return
		}
	}
	flusher.Flush()

	ctx := r.Context()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-updates:
			if !ok {
				return
			}
			if !writeSSE(w, job) {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			// Keep idle connections (and proxies) alive, and reconcile any
			// in-flight job: the live stream coalesces/drops intermediate
			// progress frames for a slow client, so re-emit the current state of
			// non-terminal jobs here. The client upserts by id, so this only
			// corrects a stale progress bar; it never re-adds a finished row.
			sent := false
			for _, job := range s.Jobs.Snapshot() {
				if job.EndedAt.IsZero() {
					if !writeSSE(w, job) {
						return
					}
					sent = true
				}
			}
			if !sent {
				if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
					return
				}
			}
			flusher.Flush()
		}
	}
}

// writeSSE marshals a job as a single SSE "data:" event. Returns false if the
// write failed (client gone), so the caller can stop.
func writeSSE(w http.ResponseWriter, job *ingest.Job) bool {
	b, err := json.Marshal(job)
	if err != nil {
		return true // skip a bad job, keep the stream alive
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return false
	}
	return true
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
