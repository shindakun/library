// Package fileutil holds small filesystem helpers shared across packages.
package fileutil

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// ReadCapped reads up to max bytes from r and returns them, or an error if the
// source exceeds max (so an oversized/hostile input is rejected, never silently
// truncated). It reads at most max+1 bytes, so the over-limit case is detected
// without buffering the whole oversized source. Shared by the epub and comic
// readers, which both pull untrusted entries out of a zip and must bound memory.
func ReadCapped(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("input exceeds %d bytes", max)
	}
	return data, nil
}

// LibraryRelPath returns the library-relative path for a book, organized on
// disk as "Author/Title<ext>" (e.g. ".epub" or ".cbz"). The first author is used
// as the folder; books with no author go under "Unknown Author". Both segments
// are sanitized. ext may be given with or without a leading dot; an empty ext
// defaults to ".epub".
func LibraryRelPath(authors []string, title, ext string) string {
	author := "Unknown Author"
	if len(authors) > 0 && authors[0] != "" {
		author = authors[0]
	}
	if ext == "" {
		ext = ".epub"
	} else if ext[0] != '.' {
		ext = "." + ext
	}
	return filepath.Join(SafeFilename(author), SafeFilename(title)+ext)
}

// SafeFilename turns an arbitrary string (a book title or author, possibly
// user-edited) into a single safe path segment. It replaces path separators and
// characters that are illegal or troublesome across platforms, strips control
// characters, and trims trailing dots/spaces (Windows-hostile). Crucially, a
// result consisting only of dots (".", "..", ...) is neutralized so it can never
// act as a directory-traversal segment when joined into a path. Returns "book"
// if nothing usable remains.
func SafeFilename(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|':
			out = append(out, '_')
		case r < 0x20 || r == 0x7f:
			// Drop ASCII control characters (incl. NUL, newlines).
		default:
			out = append(out, r)
		}
	}
	// Trim trailing dots and spaces (illegal/awkward on Windows; "foo." -> "foo").
	name := strings.TrimRight(string(out), " .")
	// A segment that is all dots (or now empty) must not become a path component
	// like "." or ".." which would escape or alias a directory.
	if name == "" || strings.Trim(name, ".") == "" {
		return "book"
	}
	return name
}
