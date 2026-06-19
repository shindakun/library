// Package comic reads CBZ comic archives: a ZIP of page images, optionally with
// a ComicInfo.xml (the de-facto ComicRack metadata schema) at the root. It is a
// sibling to internal/epub with the same shape (Read/CoverImage), so the catalog
// and cover cache treat a comic like any other book.
//
// CBZ only. CBR (a RAR) is converted to CBZ at the import boundary (see
// internal/ingest); the rest of the system, including this package, only ever
// sees ZIPs.
package comic

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Metadata is what the catalog needs from a comic. Field names mirror
// epub.Metadata so the same upsert path can store either: comics simply leave
// the epub-only fields (Publisher, Description, Identifiers) empty.
type Metadata struct {
	Title       string
	Authors     []string // writer(s) from ComicInfo.xml
	Series      string
	SeriesIndex float64 // issue number, if numeric
	Language    string
	Publisher   string
	Description string
	Published   string            // year, if known
	Identifiers map[string]string // always empty for comics; present for symmetry
	HasCover    bool              // true when the archive has at least one page image
	PageCount   int
}

// comicInfo is the subset of the ComicRack ComicInfo.xml schema we read.
type comicInfo struct {
	Title       string `xml:"Title"`
	Series      string `xml:"Series"`
	Number      string `xml:"Number"`
	Writer      string `xml:"Writer"`
	Penciller   string `xml:"Penciller"`
	Year        string `xml:"Year"`
	Summary     string `xml:"Summary"`
	LanguageISO string `xml:"LanguageISO"`
	PageCount   int    `xml:"PageCount"`
}

// imageExts are the page-image extensions we recognize inside a CBZ.
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true,
}

// Read opens a .cbz (a ZIP), counts image pages, and pulls metadata from
// ComicInfo.xml if present, else derives Title from the filename.
func Read(cbzPath string) (*Metadata, error) {
	zr, err := zip.OpenReader(cbzPath)
	if err != nil {
		return nil, fmt.Errorf("open cbz: %w", err)
	}
	defer func() { _ = zr.Close() }()

	pages := pageList(&zr.Reader)
	m := &Metadata{
		PageCount:   len(pages),
		HasCover:    len(pages) > 0,
		Identifiers: map[string]string{},
	}

	if ci := readComicInfo(&zr.Reader); ci != nil {
		applyComicInfo(m, ci)
	}
	// Title always falls back to the filename (a comic with ComicInfo.xml but no
	// Title still wants a sensible name).
	if m.Title == "" {
		m.Title = titleFromFilename(cbzPath)
	}
	return m, nil
}

// Pages returns the archive's image entry names in reading order. Used by the
// reader to page through the comic.
func Pages(cbzPath string) ([]string, error) {
	zr, err := zip.OpenReader(cbzPath)
	if err != nil {
		return nil, fmt.Errorf("open cbz: %w", err)
	}
	defer func() { _ = zr.Close() }()
	return pageList(&zr.Reader), nil
}

// PageImage returns one page's bytes and mime type by index (0-based, in reading
// order). The reader fetches these one at a time.
func PageImage(cbzPath string, index int) ([]byte, string, error) {
	zr, err := zip.OpenReader(cbzPath)
	if err != nil {
		return nil, "", fmt.Errorf("open cbz: %w", err)
	}
	defer func() { _ = zr.Close() }()

	pages := pageList(&zr.Reader)
	if index < 0 || index >= len(pages) {
		return nil, "", fmt.Errorf("page %d out of range (have %d)", index, len(pages))
	}
	return readEntry(&zr.Reader, pages[index])
}

// CoverImage returns the first page (reading order) as the cover, parallel to
// epub.CoverImage, so the existing cover cache works unchanged.
func CoverImage(cbzPath string) ([]byte, string, error) {
	zr, err := zip.OpenReader(cbzPath)
	if err != nil {
		return nil, "", fmt.Errorf("open cbz: %w", err)
	}
	defer func() { _ = zr.Close() }()

	pages := pageList(&zr.Reader)
	if len(pages) == 0 {
		return nil, "", fmt.Errorf("no page images in cbz")
	}
	return readEntry(&zr.Reader, pages[0])
}

// pageList returns the image entries sorted into reading order, skipping
// directories, ComicInfo.xml, and any non-image file.
func pageList(zr *zip.Reader) []string {
	var names []string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := path.Base(f.Name)
		if strings.EqualFold(base, "ComicInfo.xml") {
			continue
		}
		if !imageExts[strings.ToLower(path.Ext(f.Name))] {
			continue
		}
		names = append(names, f.Name)
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	return names
}

// naturalLess orders archive entry names the way a human reads pages: directory
// segments first (so nested chapters group), then a natural/numeric comparison
// within each segment so "page2.jpg" sorts before "page10.jpg" regardless of
// zero-padding. Comparison is case-insensitive.
func naturalLess(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	for len(la) > 0 && len(lb) > 0 {
		// Compare a run of non-digits, then a run of digits, alternating.
		ai, bi := isDigit(la[0]), isDigit(lb[0])
		if ai != bi {
			// A digit run sorts before a non-digit run at the same position so
			// "1" < "a"; this is arbitrary but stable and rarely hit in practice.
			return ai
		}
		if !ai {
			// Non-digit run: compare a single rune-ish byte at a time.
			if la[0] != lb[0] {
				return la[0] < lb[0]
			}
			la, lb = la[1:], lb[1:]
			continue
		}
		// Digit run on both sides: compare by numeric value, ignoring leading
		// zeros, falling back to length then lexical for equal values.
		na, ra := leadingNumber(la)
		nb, rb := leadingNumber(lb)
		if na != nb {
			return na < nb
		}
		la, lb = ra, rb
	}
	return len(la) < len(lb)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// leadingNumber consumes the leading digit run and returns its numeric value
// plus the remainder. Values wider than int64 saturate, which is fine for page
// numbers.
func leadingNumber(s string) (uint64, string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	n, err := strconv.ParseUint(s[:i], 10, 64)
	if err != nil {
		n = ^uint64(0) // overflow: treat as very large, keeps ordering stable
	}
	return n, s[i:]
}

func readComicInfo(zr *zip.Reader) *comicInfo {
	for _, f := range zr.File {
		if strings.EqualFold(path.Base(f.Name), "ComicInfo.xml") {
			data, _, err := readEntry(zr, f.Name)
			if err != nil {
				return nil
			}
			var ci comicInfo
			if xml.Unmarshal(data, &ci) != nil {
				return nil
			}
			return &ci
		}
	}
	return nil
}

func applyComicInfo(m *Metadata, ci *comicInfo) {
	m.Title = strings.TrimSpace(ci.Title)
	m.Series = strings.TrimSpace(ci.Series)
	m.Description = strings.TrimSpace(ci.Summary)
	m.Language = strings.TrimSpace(ci.LanguageISO)
	m.Published = strings.TrimSpace(ci.Year)
	if ci.PageCount > 0 {
		m.PageCount = ci.PageCount
	}
	// Writer is a comma-separated list in ComicRack; fall back to Penciller.
	m.Authors = append(m.Authors, splitCreators(ci.Writer)...)
	if len(m.Authors) == 0 {
		m.Authors = splitCreators(ci.Penciller)
	}
	if n := strings.TrimSpace(ci.Number); n != "" {
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			m.SeriesIndex = f
		}
	}
	// A comic with a Series but no Title reads better as "Series #N".
	if m.Title == "" && m.Series != "" {
		if num := strings.TrimSpace(ci.Number); num != "" {
			m.Title = m.Series + " #" + num
		} else {
			m.Title = m.Series
		}
	}
}

func splitCreators(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// titleFromFilename derives a readable title from the CBZ filename: strip the
// extension and turn separators into spaces.
func titleFromFilename(p string) string {
	base := path.Base(p)
	base = strings.TrimSuffix(base, path.Ext(base))
	base = strings.NewReplacer("_", " ", ".", " ").Replace(base)
	return strings.TrimSpace(base)
}

// maxEntryBytes caps a single comic entry (a page image or ComicInfo.xml) so a
// hostile/oversized archive can't OOM a scan. 128 MiB is far above any real
// comic page.
const maxEntryBytes = 128 << 20

func readEntry(zr *zip.Reader, name string) ([]byte, string, error) {
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, "", err
			}
			defer func() { _ = rc.Close() }()
			var buf bytes.Buffer
			n, err := io.Copy(&buf, io.LimitReader(rc, maxEntryBytes+1))
			if err != nil {
				return nil, "", err
			}
			if n > maxEntryBytes {
				return nil, "", fmt.Errorf("entry %q exceeds %d bytes", name, maxEntryBytes)
			}
			return buf.Bytes(), mediaType(name), nil
		}
	}
	return nil, "", fmt.Errorf("entry %q not found", name)
}

func mediaType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	default:
		return "image/jpeg"
	}
}
