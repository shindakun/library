package fileutil

import (
	"path/filepath"
	"testing"
)

func TestLibraryRelPath(t *testing.T) {
	cases := []struct {
		authors []string
		title   string
		want    string
	}{
		{[]string{"Megan E. O'Keefe"}, "Chaos Vector", filepath.Join("Megan E. O'Keefe", "Chaos Vector.epub")},
		{[]string{"A", "B"}, "T", filepath.Join("A", "T.epub")}, // first author only
		{nil, "Orphan", filepath.Join("Unknown Author", "Orphan.epub")},
		{[]string{""}, "Orphan", filepath.Join("Unknown Author", "Orphan.epub")},
		{[]string{"Bad/Name"}, "Bad:Title", filepath.Join("Bad_Name", "Bad_Title.epub")}, // sanitized
	}
	for _, c := range cases {
		if got := LibraryRelPath(c.authors, c.title); got != c.want {
			t.Errorf("LibraryRelPath(%v, %q) = %q, want %q", c.authors, c.title, got, c.want)
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
	}
	for in, want := range cases {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
