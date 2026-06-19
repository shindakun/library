// Package epub reads metadata and the cover image out of an EPUB file.
//
// An EPUB is a ZIP whose META-INF/container.xml points at an OPF package
// document; the OPF holds Dublin Core metadata and a manifest. We parse just
// enough to catalog a book and pull its cover, with no third-party dependency.
package epub

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strings"
)

// Metadata is the subset of OPF data we catalog.
type Metadata struct {
	Title       string
	Authors     []string
	Language    string
	Publisher   string
	Description string
	Published   string
	Series      string
	SeriesIndex float64
	Identifiers map[string]string // scheme -> value (e.g. "isbn" -> "978...")
	HasCover    bool
	coverHref   string // OPF-relative path to the cover image, if found
	opfDir      string // directory of the OPF within the zip, for resolving hrefs
}

// container.xml -> rootfile points at the OPF.
type container struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

type opfPackage struct {
	Metadata struct {
		Titles      []string `xml:"http://purl.org/dc/elements/1.1/ title"`
		Creators    []string `xml:"http://purl.org/dc/elements/1.1/ creator"`
		Language    string   `xml:"http://purl.org/dc/elements/1.1/ language"`
		Publisher   string   `xml:"http://purl.org/dc/elements/1.1/ publisher"`
		Description string   `xml:"http://purl.org/dc/elements/1.1/ description"`
		Date        string   `xml:"http://purl.org/dc/elements/1.1/ date"`
		Identifiers []struct {
			Scheme string `xml:"http://www.idpf.org/2007/opf scheme,attr"`
			Value  string `xml:",chardata"`
		} `xml:"http://purl.org/dc/elements/1.1/ identifier"`
		Metas []struct {
			Name     string `xml:"name,attr"`
			Content  string `xml:"content,attr"`
			Property string `xml:"property,attr"`
			Value    string `xml:",chardata"`
		} `xml:"meta"`
	} `xml:"metadata"`
	Manifest struct {
		Items []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			MediaType  string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"item"`
	} `xml:"manifest"`
}

// Read opens the EPUB at path and extracts its metadata.
func Read(epubPath string) (*Metadata, error) {
	zr, err := zip.OpenReader(epubPath)
	if err != nil {
		return nil, fmt.Errorf("open epub: %w", err)
	}
	defer func() { _ = zr.Close() }()
	return read(&zr.Reader)
}

func read(zr *zip.Reader) (*Metadata, error) {
	opfPath, err := findOPFPath(zr)
	if err != nil {
		return nil, err
	}
	pkg, err := parseOPF(zr, opfPath)
	if err != nil {
		return nil, err
	}

	m := &Metadata{
		Identifiers: map[string]string{},
		opfDir:      path.Dir(opfPath),
	}
	if len(pkg.Metadata.Titles) > 0 {
		m.Title = strings.TrimSpace(pkg.Metadata.Titles[0])
	}
	for _, c := range pkg.Metadata.Creators {
		if c = strings.TrimSpace(c); c != "" {
			m.Authors = append(m.Authors, c)
		}
	}
	m.Language = strings.TrimSpace(pkg.Metadata.Language)
	m.Publisher = strings.TrimSpace(pkg.Metadata.Publisher)
	m.Description = strings.TrimSpace(pkg.Metadata.Description)
	m.Published = strings.TrimSpace(pkg.Metadata.Date)

	for _, id := range pkg.Metadata.Identifiers {
		scheme := strings.ToLower(strings.TrimSpace(id.Scheme))
		val := strings.TrimSpace(id.Value)
		if val == "" {
			continue
		}
		if scheme == "" {
			scheme = guessIdentifierScheme(val)
		}
		m.Identifiers[scheme] = val
	}

	// Series: Calibre stores it in <meta name="calibre:series"> and
	// EPUB3 in <meta property="belongs-to-collection">.
	for _, meta := range pkg.Metadata.Metas {
		switch {
		case meta.Name == "calibre:series":
			m.Series = strings.TrimSpace(meta.Content)
		case meta.Name == "calibre:series_index":
			_, _ = fmt.Sscanf(meta.Content, "%f", &m.SeriesIndex)
		case meta.Property == "belongs-to-collection" && m.Series == "":
			m.Series = strings.TrimSpace(meta.Value)
		}
	}

	m.coverHref = findCoverHref(pkg)
	m.HasCover = m.coverHref != ""

	if m.Title == "" {
		m.Title = "Untitled"
	}
	return m, nil
}

func findOPFPath(zr *zip.Reader) (string, error) {
	f := openFile(zr, "META-INF/container.xml")
	if f == nil {
		return "", fmt.Errorf("no META-INF/container.xml (not a valid epub?)")
	}
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	var c container
	if err := xml.NewDecoder(rc).Decode(&c); err != nil {
		return "", fmt.Errorf("parse container.xml: %w", err)
	}
	if len(c.Rootfiles) == 0 || c.Rootfiles[0].FullPath == "" {
		return "", fmt.Errorf("container.xml names no rootfile")
	}
	return c.Rootfiles[0].FullPath, nil
}

func parseOPF(zr *zip.Reader, opfPath string) (*opfPackage, error) {
	f := openFile(zr, opfPath)
	if f == nil {
		return nil, fmt.Errorf("opf %q not found in archive", opfPath)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var pkg opfPackage
	if err := xml.NewDecoder(rc).Decode(&pkg); err != nil {
		return nil, fmt.Errorf("parse opf: %w", err)
	}
	return &pkg, nil
}

// findCoverHref resolves the cover image's OPF-relative href via the two common
// conventions: EPUB3 properties="cover-image", or a <meta name="cover"> that
// references a manifest item id.
func findCoverHref(pkg *opfPackage) string {
	for _, it := range pkg.Manifest.Items {
		if strings.Contains(it.Properties, "cover-image") {
			return it.Href
		}
	}
	var coverID string
	for _, meta := range pkg.Metadata.Metas {
		if meta.Name == "cover" {
			coverID = meta.Content
			break
		}
	}
	if coverID != "" {
		for _, it := range pkg.Manifest.Items {
			if it.ID == coverID {
				return it.Href
			}
		}
	}
	// Last resort: first image whose id/href looks like a cover.
	for _, it := range pkg.Manifest.Items {
		if strings.HasPrefix(it.MediaType, "image/") &&
			(strings.Contains(strings.ToLower(it.ID), "cover") ||
				strings.Contains(strings.ToLower(it.Href), "cover")) {
			return it.Href
		}
	}
	return ""
}

// IsADEPTEncrypted reports whether the EPUB carries Adobe ADEPT (Digital
// Editions) DRM and therefore needs DeDRM before it can be read. ADEPT books
// ship a META-INF/rights.xml (the license) and a META-INF/encryption.xml that
// references Adobe's ADEPT namespace. A plain DRM-free EPUB has neither (or an
// encryption.xml only for font obfuscation, which is NOT ADEPT), so this lets
// the importer skip decryption for clean files instead of failing them.
func IsADEPTEncrypted(epubPath string) (bool, error) {
	zr, err := zip.OpenReader(epubPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = zr.Close() }()

	// rights.xml is the strongest ADEPT signal.
	if f := openFile(&zr.Reader, "META-INF/rights.xml"); f != nil {
		if rc, err := f.Open(); err == nil {
			data, rerr := readCapped(rc, maxMetaBytes)
			_ = rc.Close()
			if rerr == nil && bytes.Contains(data, []byte("ns.adobe.com/adept")) {
				return true, nil
			}
		}
	}
	// Fall back to encryption.xml referencing the ADEPT namespace. Font-only
	// obfuscation uses the IDPF namespace instead, so we match Adobe specifically.
	if f := openFile(&zr.Reader, "META-INF/encryption.xml"); f != nil {
		if rc, err := f.Open(); err == nil {
			data, rerr := readCapped(rc, maxMetaBytes)
			_ = rc.Close()
			if rerr == nil && bytes.Contains(data, []byte("ns.adobe.com/adept")) {
				return true, nil
			}
		}
	}
	return false, nil
}

// CoverImage returns the raw bytes and media type of the cover, if any.
func CoverImage(epubPath string) ([]byte, string, error) {
	zr, err := zip.OpenReader(epubPath)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = zr.Close() }()

	opfPath, err := findOPFPath(&zr.Reader)
	if err != nil {
		return nil, "", err
	}
	pkg, err := parseOPF(&zr.Reader, opfPath)
	if err != nil {
		return nil, "", err
	}
	href := findCoverHref(pkg)
	if href == "" {
		return nil, "", fmt.Errorf("no cover image in %s", epubPath)
	}
	coverPath := path.Join(path.Dir(opfPath), href)
	f := openFile(&zr.Reader, coverPath)
	if f == nil {
		return nil, "", fmt.Errorf("cover %q missing from archive", coverPath)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rc.Close() }()
	data, err := readCapped(rc, maxCoverBytes)
	if err != nil {
		return nil, "", err
	}
	return data, mediaTypeForCover(coverPath), nil
}

// Read caps for untrusted zip entries: a cover image and the small DRM metadata
// files. These bound memory use against a hostile/oversized archive (a cover or
// rights.xml claiming to be gigabytes) so a scan can't be OOM'd by one bad file.
const (
	maxCoverBytes = 64 << 20 // 64 MiB: very generous for a cover image
	maxMetaBytes  = 1 << 20  // 1 MiB: rights.xml / encryption.xml are tiny
)

// readCapped reads up to max bytes, returning an error if the source exceeds it
// (so an oversized entry is rejected rather than silently truncated).
func readCapped(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("entry exceeds %d bytes", max)
	}
	return data, nil
}

// openFile finds a zip entry by name, tolerating leading "./" and case.
func openFile(zr *zip.Reader, name string) *zip.File {
	name = strings.TrimPrefix(name, "./")
	for _, f := range zr.File {
		if f.Name == name {
			return f
		}
	}
	lower := strings.ToLower(name)
	for _, f := range zr.File {
		if strings.ToLower(f.Name) == lower {
			return f
		}
	}
	return nil
}

func mediaTypeForCover(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	default:
		return "image/jpeg"
	}
}

func guessIdentifierScheme(val string) string {
	v := strings.ToLower(val)
	switch {
	case strings.HasPrefix(v, "urn:isbn:"), len(strings.ReplaceAll(v, "-", "")) == 13 && strings.HasPrefix(v, "978"):
		return "isbn"
	case strings.HasPrefix(v, "urn:uuid:"):
		return "uuid"
	default:
		return "unknown"
	}
}
