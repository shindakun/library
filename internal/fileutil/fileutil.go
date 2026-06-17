// Package fileutil holds small filesystem helpers shared across packages.
package fileutil

import "path/filepath"

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

// SafeFilename strips characters that are illegal or troublesome in filenames
// across platforms, so a book title can be used directly as a filename. Returns
// "book" if nothing usable remains.
func SafeFilename(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "book"
	}
	return string(out)
}
