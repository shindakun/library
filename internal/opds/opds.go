// Package opds emits an OPDS 1.2 (Atom) catalog for the Xteink X4 / Crosspoint
// OPDS client.
//
// HARD REQUIREMENT, PAGING: acquisition feeds are ALWAYS paged at PageSize.
// The X4 runs on an ESP32C3 with very little RAM; handing it one unbounded
// acquisition feed (every book in the library in a single XML document) can
// make the firmware hang or OOM while parsing. So:
//
//   - The root feed (/opds) is a small *navigation* feed: a handful of links to
//     bounded subsections (New, All, by Author, by Series, Search). It never
//     lists books directly.
//   - Every *acquisition* feed (/opds/all, /opds/new, search results, per-author,
//     per-series) is capped at PageSize entries and carries rel="next"/"prev"
//     links so the client pulls one bounded page at a time.
//
// Do not "optimize" by removing the cap. The cap is the contract with the device.
package opds

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/steve/library/internal/catalog"
)

// PageSize bounds every acquisition feed so a memory-constrained OPDS client
// never receives an unbounded feed. Kept deliberately small for the X4's limited
// memory (ESP32C3); tune only after testing on the real device.
const PageSize = 30

// Handler serves the OPDS endpoints. baseURL is how the device reaches us
// (e.g. "http://192.168.1.20:8080"); it is used to build absolute links, which
// some constrained clients handle more reliably than relative ones.
type Handler struct {
	Cat     *catalog.Catalog
	BaseURL string
}

// Register wires the OPDS routes onto mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /opds", h.root)
	mux.HandleFunc("GET /opds/all", h.all)
	mux.HandleFunc("GET /opds/new", h.newest)
	mux.HandleFunc("GET /opds/search", h.search)
	mux.HandleFunc("GET /opds/opensearch.xml", h.openSearchDesc)
}

// --- Atom/OPDS document model --------------------------------------------

const (
	nsAtom       = "http://www.w3.org/2005/Atom"
	typeNavFeed  = "application/atom+xml;profile=opds-catalog;kind=navigation"
	typeAcqFeed  = "application/atom+xml;profile=opds-catalog;kind=acquisition"
	typeEpub     = "application/epub+zip"
	relSelf      = "self"
	relStart     = "start"
	relNext      = "next"
	relPrev      = "previous"
	relSearch    = "search"
	relAcq       = "http://opds-spec.org/acquisition"
	relImage     = "http://opds-spec.org/image"
	relThumb     = "http://opds-spec.org/image/thumbnail"
	typeOpenSrch = "application/opensearchdescription+xml"
)

type feed struct {
	XMLName xml.Name `xml:"feed"`
	XMLNS   string   `xml:"xmlns,attr"`
	ID      string   `xml:"id"`
	Title   string   `xml:"title"`
	Updated string   `xml:"updated"`
	Links   []link   `xml:"link"`
	Entries []entry  `xml:"entry"`
}

type link struct {
	Rel   string `xml:"rel,attr"`
	Href  string `xml:"href,attr"`
	Type  string `xml:"type,attr"`
	Title string `xml:"title,attr,omitempty"`
}

type entry struct {
	ID      string   `xml:"id"`
	Title   string   `xml:"title"`
	Updated string   `xml:"updated"`
	Authors []author `xml:"author"`
	Content *content `xml:"content,omitempty"`
	Links   []link   `xml:"link"`
}

type author struct {
	Name string `xml:"name"`
}

type content struct {
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

// --- Handlers -------------------------------------------------------------

// root: navigation feed only. No book entries here, just bounded entry points.
func (h *Handler) root(w http.ResponseWriter, r *http.Request) {
	f := feed{
		XMLNS:   nsAtom,
		ID:      h.BaseURL + "/opds",
		Title:   "Library",
		Updated: now(),
		Links: []link{
			{Rel: relSelf, Href: h.abs("/opds"), Type: typeNavFeed},
			{Rel: relStart, Href: h.abs("/opds"), Type: typeNavFeed},
			{Rel: relSearch, Href: h.abs("/opds/opensearch.xml"), Type: typeOpenSrch},
		},
		Entries: []entry{
			navEntry("new", "Recently Added", "Newest books first", h.abs("/opds/new")),
			navEntry("all", "All Books", "Every book, paged", h.abs("/opds/all")),
		},
	}
	writeFeed(w, &f)
}

func (h *Handler) all(w http.ResponseWriter, r *http.Request) {
	h.acquisitionPage(w, r, "all", "All Books", catalog.ListOptions{})
}

func (h *Handler) newest(w http.ResponseWriter, r *http.Request) {
	// "Recently Added" sorts newest-first; the default (all/library) sort is by
	// author then title.
	h.acquisitionPage(w, r, "new", "Recently Added", catalog.ListOptions{Sort: catalog.SortRecent})
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	h.acquisitionPage(w, r, "search", "Search: "+q, catalog.ListOptions{Query: q})
}

// acquisitionPage renders ONE bounded page of an acquisition feed and attaches
// next/prev links. This is the single chokepoint that enforces paging for the
// device; every book-listing feed funnels through here.
func (h *Handler) acquisitionPage(w http.ResponseWriter, r *http.Request, slug, title string, base catalog.ListOptions) {
	page := pageParam(r)
	base.Limit = PageSize + 1 // fetch one extra to detect a next page
	base.Offset = page * PageSize

	books, err := h.Cat.List(r.Context(), base)
	if err != nil {
		http.Error(w, "catalog error", http.StatusInternalServerError)
		return
	}
	hasNext := len(books) > PageSize
	if hasNext {
		books = books[:PageSize]
	}

	selfHref := h.pagedHref(r, slug, page)
	f := feed{
		XMLNS:   nsAtom,
		ID:      selfHref,
		Title:   title,
		Updated: now(),
		Links: []link{
			{Rel: relSelf, Href: selfHref, Type: typeAcqFeed},
			{Rel: relStart, Href: h.abs("/opds"), Type: typeNavFeed},
		},
	}
	if page > 0 {
		f.Links = append(f.Links, link{Rel: relPrev, Href: h.pagedHref(r, slug, page-1), Type: typeAcqFeed})
	}
	if hasNext {
		f.Links = append(f.Links, link{Rel: relNext, Href: h.pagedHref(r, slug, page+1), Type: typeAcqFeed})
	}
	for _, b := range books {
		f.Entries = append(f.Entries, h.bookEntry(b))
	}
	writeFeed(w, &f)
}

func (h *Handler) bookEntry(b *catalog.Book) entry {
	slug := b.Slug()
	e := entry{
		// Stable entry ID + links keyed on the content-hash slug, so a book's
		// OPDS identity survives catalog rebuilds.
		ID:      "urn:library:book:" + slug,
		Title:   b.Title,
		Updated: now(),
		Links: []link{
			// The acquisition link: this is what the OPDS client downloads.
			{Rel: relAcq, Href: h.abs("/book/" + slug + "/file"), Type: typeEpub},
		},
	}
	for _, a := range b.Authors {
		e.Authors = append(e.Authors, author{Name: a})
	}
	if b.Description != "" {
		e.Content = &content{Type: "text", Text: b.Description}
	}
	if b.HasCover {
		e.Links = append(e.Links,
			link{Rel: relImage, Href: h.abs("/book/" + slug + "/cover"), Type: "image/jpeg"},
			link{Rel: relThumb, Href: h.abs("/book/" + slug + "/cover"), Type: "image/jpeg"},
		)
	}
	return e
}

// openSearchDesc advertises the search endpoint per the OpenSearch spec, which
// Crosspoint uses to offer in-catalog search.
func (h *Handler) openSearchDesc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", typeOpenSrch)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
  <ShortName>Library</ShortName>
  <Description>Search the library</Description>
  <Url type="%s" template="%s/opds/search?q={searchTerms}"/>
</OpenSearchDescription>`, typeAcqFeed, h.BaseURL)
}

// --- helpers --------------------------------------------------------------

func navEntry(id, title, summary, href string) entry {
	return entry{
		ID:      "urn:library:nav:" + id,
		Title:   title,
		Updated: now(),
		Content: &content{Type: "text", Text: summary},
		Links:   []link{{Rel: "subsection", Href: href, Type: typeAcqFeed}},
	}
}

func (h *Handler) abs(path string) string { return h.BaseURL + path }

func (h *Handler) pagedHref(r *http.Request, slug string, page int) string {
	u := url.Values{}
	if page > 0 {
		u.Set("page", strconv.Itoa(page))
	}
	if q := r.URL.Query().Get("q"); q != "" {
		u.Set("q", q)
	}
	href := h.abs("/opds/" + slug)
	if enc := u.Encode(); enc != "" {
		href += "?" + enc
	}
	return href
}

func pageParam(r *http.Request) int {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if p < 0 {
		p = 0
	}
	return p
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func writeFeed(w http.ResponseWriter, f *feed) {
	w.Header().Set("Content-Type", typeAcqFeed)
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(f)
}

// Ensure ctx import is used even if handlers are trimmed during edits.
var _ = context.Background
