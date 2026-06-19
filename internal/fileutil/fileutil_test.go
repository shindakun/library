package fileutil

import (
	"path/filepath"
	"testing"
)

func TestLibraryRelPath(t *testing.T) {
	cases := []struct {
		authors []string
		title   string
		ext     string
		want    string
	}{
		{[]string{"Auth One"}, "Sample Title", ".epub", filepath.Join("Auth One", "Sample Title.epub")},
		{[]string{"A", "B"}, "T", ".epub", filepath.Join("A", "T.epub")}, // first author only
		{nil, "Orphan", ".epub", filepath.Join("Unknown Author", "Orphan.epub")},
		{[]string{""}, "Orphan", ".epub", filepath.Join("Unknown Author", "Orphan.epub")},
		{[]string{"Bad/Name"}, "Bad:Title", ".epub", filepath.Join("Bad_Name", "Bad_Title.epub")}, // sanitized
		{[]string{"Auth One"}, "Issue 1", ".cbz", filepath.Join("Auth One", "Issue 1.cbz")},       // comic
		{[]string{"Auth One"}, "No Dot", "cbz", filepath.Join("Auth One", "No Dot.cbz")},          // ext normalized
		{[]string{"Auth One"}, "Default", "", filepath.Join("Auth One", "Default.epub")},          // empty -> epub
	}
	for _, c := range cases {
		if got := LibraryRelPath(c.authors, c.title, c.ext); got != c.want {
			t.Errorf("LibraryRelPath(%v, %q, %q) = %q, want %q", c.authors, c.title, c.ext, got, c.want)
		}
	}
}

func TestSafeFilename(t *testing.T) {
	cases := map[string]string{
		"Chaos Vector":              "Chaos Vector",
		"Frankenstein; or, The Foo": "Frankenstein; or, The Foo",
		"A/B: C*D?":                 "A_B_ C_D_",
		`weird"<>|name`:             "weird____name", // 4 illegal chars: " < > |
		"":                          "book",          // empty -> fallback
		"////":                      "____",          // all-illegal stays non-empty
		// Path-traversal guard: an all-dots segment must NOT survive as "."/"..".
		"..":           "book",
		".":            "book",
		"...":          "book",
		"../etc":       ".._etc",   // the slash is neutralized; not all-dots, kept
		"name\x00with": "namewith", // control chars dropped
		"trailing... ": "trailing", // trailing dots/spaces trimmed
	}
	for in, want := range cases {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
