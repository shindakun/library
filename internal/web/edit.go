package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"  // register GIF decoder
	_ "image/jpeg" // register JPEG decoder
	_ "image/png"  // register PNG decoder
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/steve/library/internal/catalog"
	"github.com/steve/library/internal/ingest"
)

// maxCoverBytes bounds an uploaded cover image.
const maxCoverBytes = 8 << 20 // 8 MiB

// apiSetCover stores a user-supplied cover override for a book. The upload is
// validated to actually decode as an image (so a non-image / hostile blob is
// rejected), and the detected format selects the stored mime/extension. The
// override is keyed on the stable slug and wins over the extracted cover.
func (s *Server) apiSetCover(w http.ResponseWriter, r *http.Request) {
	b, err := s.book(r.Context(), r)
	if err != nil {
		s.bookErr(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCoverBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "upload too large or unreadable", http.StatusBadRequest)
		return
	}
	// Validate it decodes as a real image and learn its format (don't trust the
	// client's content-type).
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		http.Error(w, "not a valid image", http.StatusUnsupportedMediaType)
		return
	}
	mime := "image/jpeg"
	switch format {
	case "png":
		mime = "image/png"
	case "gif":
		mime = "image/gif"
	}
	if err := s.Cat.SetCoverOverride(r.Context(), b, data, mime); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxEditBody bounds the edit request body so a hostile client can't stream an
// unbounded payload into the handler.
const maxEditBody = 64 << 10 // 64 KiB

// editForm renders the metadata edit form for a book.
func (s *Server) editForm(w http.ResponseWriter, r *http.Request) {
	b, err := s.book(r.Context(), r)
	if err != nil {
		s.bookErr(w, err)
		return
	}
	s.render(w, "edit.html", map[string]any{
		"Book":    b,
		"Authors": strings.Join(b.Authors, ", "),
	})
}

// editPayload is the JSON shape the edit form PUTs. Pointer fields preserve
// "absent vs empty" semantics so the client can clear a field by sending "".
type editPayload struct {
	Title       *string `json:"title"`
	SortTitle   *string `json:"sortTitle"`
	Authors     *string `json:"authors"` // comma-separated
	Series      *string `json:"series"`
	SeriesIndex *string `json:"seriesIndex"` // parsed leniently; blank = clear
	Language    *string `json:"language"`
	Publisher   *string `json:"publisher"`
	Description *string `json:"description"`
	Published   *string `json:"published"`
	Tags        *string `json:"tags"` // comma-separated
}

// toEdits converts the wire payload into catalog.Edits. The catalog re-sanitizes
// every value, so this layer only has to shape the data (split lists, parse the
// numeric index); it does not need to trust or clean the input itself.
func (p editPayload) toEdits() catalog.Edits {
	var e catalog.Edits
	e.Title = p.Title
	e.SortTitle = p.SortTitle
	e.Series = p.Series
	e.Language = p.Language
	e.Publisher = p.Publisher
	e.Description = p.Description
	e.Published = p.Published
	if p.Authors != nil {
		authors := splitList(*p.Authors)
		e.Authors = &authors
	}
	if p.Tags != nil {
		tags := splitList(*p.Tags)
		e.Tags = &tags
	}
	if p.SeriesIndex != nil {
		if idx, ok := parseIndex(*p.SeriesIndex); ok {
			e.SeriesIndex = &idx
		}
	}
	return e
}

// apiUpdateBook applies a metadata edit. The catalog edit (the durable change) is
// written synchronously and fast. The file embed can be slow (a large comic
// re-zips every page), so it runs in the BACKGROUND as a tracked job: the
// response returns immediately with the new book + a jobId, and the client
// watches the existing import-job SSE stream for embed progress and completion.
func (s *Server) apiUpdateBook(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	r.Body = http.MaxBytesReader(w, r.Body, maxEditBody)
	var p editPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}

	updated, err := s.Cat.UpdateMetadata(r.Context(), slug, p.toEdits())
	if err != nil {
		s.bookErr(w, err)
		return
	}

	jobID := s.startEmbedJob(updated)

	writeJSON(w, map[string]any{
		"book": map[string]any{
			"slug":    updated.Slug(),
			"title":   updated.Title,
			"authors": updated.Authors,
		},
		"jobId": jobID,
	})
}

// startEmbedJob runs EmbedMetadata in the background, reporting into the
// import-job registry so the edit page can show live progress over the existing
// SSE stream. Returns the job id (empty if the registry is unavailable, in which
// case the embed still runs untracked). The catalog edit has already landed, so
// even a failed embed only means "not yet embedded in file".
func (s *Server) startEmbedJob(b *catalog.Book) string {
	if s.Jobs == nil {
		// No registry (e.g. in tests that don't need progress): embed inline,
		// still off the request goroutine so the handler returns promptly.
		go func() { _, _ = s.Cat.EmbedMetadata(context.Background(), b.Slug(), nil) }()
		return ""
	}
	jobID := s.Jobs.Start(b.Title, "embed")
	slug := b.Slug()
	s.Jobs.Update(jobID, func(j *ingest.Job) { j.Step = "embedding metadata" })
	go func() {
		onProgress := func(done, total int) {
			frac := 0.0
			if total > 0 {
				frac = float64(done) / float64(total)
			}
			s.Jobs.Update(jobID, func(j *ingest.Job) {
				j.Step = "embedding metadata"
				j.Progress = frac
				j.Detail = fmt.Sprintf("%d/%d", done, total)
			})
		}
		res, err := s.Cat.EmbedMetadata(context.Background(), slug, onProgress)
		switch {
		case err != nil:
			s.Jobs.Finish(jobID, ingest.StateFailed, "embed error: "+err.Error(), slug)
		case !res.Embedded:
			// Not a hard failure: the DB edit stands, the file just wasn't rewritten.
			s.Jobs.Finish(jobID, ingest.StateSkipped, res.Reason, slug)
		default:
			s.Jobs.Finish(jobID, ingest.StateDone, "", slug)
		}
	}()
	return jobID
}

// apiDeleteBook removes a book entirely (catalog row, file, cover). Irreversible;
// the UI gates it behind a confirm().
func (s *Server) apiDeleteBook(w http.ResponseWriter, r *http.Request) {
	if err := s.Cat.DeleteBook(r.Context(), r.PathValue("slug")); err != nil {
		s.bookErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// editFormPost is the no-JS fallback: a plain HTML form post. It applies the
// same edit + embed, then redirects back to the library.
func (s *Server) editFormPost(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	formField := func(name string) *string {
		if !r.Form.Has(name) {
			return nil
		}
		v := r.Form.Get(name)
		return &v
	}
	p := editPayload{
		Title:       formField("title"),
		SortTitle:   formField("sortTitle"),
		Authors:     formField("authors"),
		Series:      formField("series"),
		SeriesIndex: formField("seriesIndex"),
		Language:    formField("language"),
		Publisher:   formField("publisher"),
		Description: formField("description"),
		Published:   formField("published"),
		Tags:        formField("tags"),
	}
	updated, err := s.Cat.UpdateMetadata(r.Context(), slug, p.toEdits())
	if err != nil {
		s.bookErr(w, err)
		return
	}
	// Embed in the background so a large comic doesn't block the redirect.
	s.startEmbedJob(updated)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// splitList parses a comma-separated input into a trimmed, blank-free list. The
// catalog sanitizes each entry further.
func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseIndex parses a series index; a blank string clears it (returns 0, true).
func parseIndex(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
