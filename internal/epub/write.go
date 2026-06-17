package epub

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// Edits carries the metadata values to embed into an epub's OPF. Empty-string
// fields are written as empty elements (the user cleared them); a nil Authors
// slice leaves creators untouched, an empty (non-nil) slice clears them.
//
// Only the fields this v1 writer handles are present: title, language,
// publisher, description, date, and creators (authors). Identifiers/subjects are
// intentionally left as-is in the file for now.
type Edits struct {
	Title       *string
	Language    *string
	Publisher   *string
	Description *string
	Date        *string
	Authors     *[]string // dc:creator
}

// WriteMetadata writes srcPath's epub to dstPath with the OPF's metadata edited
// in place. It edits the raw OPF bytes surgically (NOT a parse/re-marshal, which
// would drop the spine, unknown metadata, namespaces, and order and corrupt the
// book): for each single-valued field it replaces the inner text of the existing
// dc: element (keeping its tag + attributes) or inserts one before </metadata>;
// for authors it removes all dc:creator elements and inserts the edited list.
// Every other zip entry is copied through unchanged, with the stored `mimetype`
// first per the epub spec.
//
// dstPath must differ from srcPath. On any error dstPath is removed. The caller
// is expected to re-parse dstPath (epub.Read) and verify before swapping it in.
func WriteMetadata(srcPath, dstPath string, e Edits) (err error) {
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("open epub: %w", err)
	}
	defer func() { _ = zr.Close() }()

	opfPath, err := findOPFPath(&zr.Reader)
	if err != nil {
		return err
	}
	opfFile := openFile(&zr.Reader, opfPath)
	if opfFile == nil {
		return fmt.Errorf("opf %q not found in archive", opfPath)
	}
	rc, err := opfFile.Open()
	if err != nil {
		return err
	}
	orig, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return err
	}

	edited, err := editOPF(orig, e)
	if err != nil {
		return fmt.Errorf("edit opf: %w", err)
	}

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

	// The `mimetype` entry, if present, MUST be the first entry and stored
	// uncompressed per the epub OCF spec. Write it first, then everything else.
	if mt := openFile(&zr.Reader, "mimetype"); mt != nil {
		if werr := copyEntry(zw, mt, nil); werr != nil {
			return werr
		}
	}
	for _, f := range zr.File {
		if f.Name == "mimetype" {
			continue // already written first
		}
		var replacement []byte
		if f.Name == opfPath {
			replacement = edited
		}
		if werr := copyEntry(zw, f, replacement); werr != nil {
			return werr
		}
	}

	if err = zw.Close(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	return nil
}

// copyEntry writes f into zw, preserving its header (and thus compression
// method). If replacement is non-nil, that content is written instead of f's
// original bytes (the header's name/method are kept; size fields are recomputed
// by the writer).
func copyEntry(zw *zip.Writer, f *zip.File, replacement []byte) error {
	hdr := f.FileHeader // copy
	w, err := zw.CreateHeader(&hdr)
	if err != nil {
		return err
	}
	if f.FileInfo().IsDir() {
		return nil
	}
	if replacement != nil {
		_, err = w.Write(replacement)
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = io.Copy(w, rc)
	return err
}

// metadataBlockRe captures the inside of the <metadata>...</metadata> element so
// edits are confined to it (the dc: elements live only there). Non-greedy,
// case-insensitive on the tag, tolerant of attributes on <metadata>.
var metadataBlockRe = regexp.MustCompile(`(?is)(<metadata\b[^>]*>)(.*?)(</metadata>)`)

// editOPF returns the OPF bytes with the requested fields edited inside the
// <metadata> block. It fails if the metadata block cannot be located, so a
// malformed/unexpected OPF aborts the embed rather than risking corruption.
func editOPF(orig []byte, e Edits) ([]byte, error) {
	loc := metadataBlockRe.FindSubmatchIndex(orig)
	if loc == nil {
		return nil, fmt.Errorf("no <metadata> block found")
	}
	openTag := orig[loc[2]:loc[3]]
	inner := string(orig[loc[4]:loc[5]])
	closeTag := orig[loc[6]:loc[7]]

	if e.Title != nil {
		inner = setSingle(inner, "title", *e.Title)
	}
	if e.Language != nil {
		inner = setSingle(inner, "language", *e.Language)
	}
	if e.Publisher != nil {
		inner = setSingle(inner, "publisher", *e.Publisher)
	}
	if e.Description != nil {
		inner = setSingle(inner, "description", *e.Description)
	}
	if e.Date != nil {
		inner = setSingle(inner, "date", *e.Date)
	}
	if e.Authors != nil {
		inner = setCreators(inner, *e.Authors)
	}

	var b bytes.Buffer
	b.Write(orig[:loc[0]]) // everything before <metadata>
	b.Write(openTag)
	b.WriteString(inner)
	b.Write(closeTag)
	b.Write(orig[loc[1]:]) // everything after </metadata>
	return b.Bytes(), nil
}

// dcElementRe builds a regex matching a dc:<local> element (any prefix bound to
// dc by convention is "dc"; we match the literal "dc:" prefix, the near-universal
// case) with optional attributes, capturing open-tag, inner text, close-tag.
func dcElementRe(local string) *regexp.Regexp {
	return regexp.MustCompile(`(?is)(<dc:` + local + `\b[^>]*>)(.*?)(</dc:` + local + `>)`)
}

// setSingle replaces the inner text of the first dc:<local> element, preserving
// the element's attributes, or inserts a new <dc:local>value</dc:local> at the
// end of the metadata inner block if none exists.
func setSingle(inner, local, value string) string {
	esc := escapeText(value)
	re := dcElementRe(local)
	if loc := re.FindStringSubmatchIndex(inner); loc != nil {
		// Replace only group 2 (the inner text), keep groups 1 and 3 verbatim.
		return inner[:loc[4]] + esc + inner[loc[5]:]
	}
	// Insert before the trailing whitespace of the block, indented simply.
	return strings.TrimRight(inner, " \t\r\n") + "\n    <dc:" + local + ">" + esc + "</dc:" + local + ">\n  "
}

// setCreators removes every dc:creator element and inserts one plain creator per
// author. file-as/role attributes on the originals are intentionally dropped on
// an explicit author edit.
func setCreators(inner string, authors []string) string {
	re := dcElementRe("creator")
	stripped := re.ReplaceAllString(inner, "")
	// Collapse blank lines left by removal, then append the new creators.
	stripped = strings.TrimRight(stripped, " \t\r\n")
	var b strings.Builder
	b.WriteString(stripped)
	for _, a := range authors {
		b.WriteString("\n    <dc:creator>")
		b.WriteString(escapeText(a))
		b.WriteString("</dc:creator>")
	}
	b.WriteString("\n  ")
	return b.String()
}

// escapeText XML-escapes a value via encoding/xml, so a hostile value can never
// break out of its element (the sanitize-at-the-sink guarantee).
func escapeText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
