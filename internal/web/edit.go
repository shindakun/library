package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/steve/library/internal/catalog"
)

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

// apiUpdateBook applies a metadata edit: it always writes the edit to the catalog
// first (the durable change), then attempts to embed it into the file. A failed
// embed is reported, not fatal: the response carries the updated book plus the
// embed status, so the UI can show "saved, not yet embedded in file".
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

	// Best-effort embed. EmbedMetadata returns (result, nil) for an expected
	// failure (it leaves the original file intact); a non-nil err is internal.
	embed, eerr := s.Cat.EmbedMetadata(r.Context(), slug)
	if eerr != nil {
		embed = catalog.EmbedResult{Reason: "embed error: " + eerr.Error()}
	}
	// Re-read so the response reflects any path change from a rename-on-embed.
	if fresh, gerr := s.Cat.GetBySlug(r.Context(), slug); gerr == nil {
		updated = fresh
	}

	writeJSON(w, map[string]any{
		"book": map[string]any{
			"slug":    updated.Slug(),
			"title":   updated.Title,
			"authors": updated.Authors,
		},
		"embedded":    embed.Embedded,
		"embedReason": embed.Reason,
	})
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
	if _, err := s.Cat.UpdateMetadata(r.Context(), slug, p.toEdits()); err != nil {
		s.bookErr(w, err)
		return
	}
	_, _ = s.Cat.EmbedMetadata(r.Context(), slug)
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
