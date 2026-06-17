package comic

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
)

// comicInfoOut is the ComicInfo.xml we WRITE. It mirrors the read struct but
// uses omitempty so absent fields don't emit empty elements, and carries the
// root element name. Values are emitted via encoding/xml, which XML-escapes
// text, so a hostile field value cannot break out of its element.
type comicInfoOut struct {
	XMLName     xml.Name `xml:"ComicInfo"`
	Title       string   `xml:"Title,omitempty"`
	Series      string   `xml:"Series,omitempty"`
	Number      string   `xml:"Number,omitempty"`
	Writer      string   `xml:"Writer,omitempty"`
	Year        string   `xml:"Year,omitempty"`
	Summary     string   `xml:"Summary,omitempty"`
	LanguageISO string   `xml:"LanguageISO,omitempty"`
	PageCount   int      `xml:"PageCount,omitempty"`
}

// WriteComicInfo writes srcPath's CBZ to dstPath with its ComicInfo.xml set from
// m: the existing ComicInfo.xml (if any) is replaced, otherwise one is added.
// Every page image is copied through unchanged (same bytes, stored). This is how
// a metadata edit is embedded into a comic; for the many comics that ship with no
// ComicInfo.xml, it ADDS one.
//
// dstPath must differ from srcPath (write to a temp file, then the caller
// verifies and swaps). On any error dstPath is removed so no partial file remains.
func WriteComicInfo(srcPath, dstPath string, m Metadata) (err error) {
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("open cbz: %w", err)
	}
	defer func() { _ = zr.Close() }()

	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = out.Close()
			_ = os.Remove(dstPath)
		}
	}()
	zw := zip.NewWriter(out)

	// Copy every entry except an existing ComicInfo.xml, preserving each entry's
	// original compression method and metadata (CreateHeader copies the header).
	for _, f := range zr.File {
		if strings.EqualFold(path.Base(f.Name), "ComicInfo.xml") {
			continue // drop the old one; we write a fresh one below
		}
		hdr := f.FileHeader // copy
		w, werr := zw.CreateHeader(&hdr)
		if werr != nil {
			return werr
		}
		if f.FileInfo().IsDir() {
			continue
		}
		rc, oerr := f.Open()
		if oerr != nil {
			return oerr
		}
		_, cerr := io.Copy(w, rc)
		_ = rc.Close()
		if cerr != nil {
			return fmt.Errorf("copy %q: %w", f.Name, cerr)
		}
	}

	// Write the fresh ComicInfo.xml at the archive root.
	ci := comicInfoOut{
		Title:       m.Title,
		Series:      m.Series,
		Writer:      strings.Join(m.Authors, ", "),
		Summary:     m.Description,
		LanguageISO: m.Language,
		Year:        m.Published,
		PageCount:   m.PageCount,
	}
	if m.SeriesIndex != 0 {
		ci.Number = formatNumber(m.SeriesIndex)
	}
	body, merr := xml.MarshalIndent(ci, "", "  ")
	if merr != nil {
		return merr
	}
	body = append([]byte(xml.Header), body...)

	w, werr := zw.Create("ComicInfo.xml")
	if werr != nil {
		return werr
	}
	if _, werr := w.Write(body); werr != nil {
		return werr
	}

	if err = zw.Close(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	return nil
}

// formatNumber renders an issue number, dropping a trailing ".0" for whole
// numbers (issue "3", not "3.0") but keeping real fractions (e.g. "1.5").
func formatNumber(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
