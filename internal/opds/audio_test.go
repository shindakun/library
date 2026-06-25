package opds

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/steve/library/internal/catalog"
)

// writeMinM4B builds a tiny tagged .m4b with ffmpeg (a test-only dependency) in
// dir/file. Skips the test if ffmpeg is unavailable.
func writeMinM4B(t *testing.T, dir, file, title string) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping audio OPDS test")
	}
	meta := filepath.Join(t.TempDir(), "ff.txt")
	if err := os.WriteFile(meta, []byte(";FFMETADATA1\ntitle="+title+"\nartist=Author\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, file)
	cmd := exec.Command("ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-t", "2", "-i", "anullsrc=r=44100:cl=mono",
		"-i", meta, "-map", "0:a", "-map_metadata", "1",
		"-c:a", "aac", "-b:a", "32k", "-movflags", "+faststart", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg m4b build failed: %v\n%s", err, b)
	}
}

// handlerWithMixed builds an OPDS handler over a catalog with one epub and one
// audiobook, so the feed-filtering can be checked.
func handlerWithMixed(t *testing.T) *Handler {
	t.Helper()
	dir := t.TempDir()
	books := filepath.Join(dir, "library")
	if err := os.MkdirAll(books, 0o755); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"), books, filepath.Join(dir, "covers"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	writeMinEPUB(t, books, "ebook.epub", "An Ebook", "Author")
	writeMinM4B(t, books, "audiobook.m4b", "An Audiobook")
	if _, err := cat.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &Handler{Cat: cat, BaseURL: "http://test.local"}
}

func TestAudioExcludedFromMainFeeds(t *testing.T) {
	h := handlerWithMixed(t)
	for _, path := range []string{"/opds/all", "/opds/new", "/opds/search?q=An"} {
		_, f := do(t, h, path)
		for _, e := range f.Entries {
			if e.Title == "An Audiobook" {
				t.Errorf("%s included the audiobook; e-reader feeds must exclude audio", path)
			}
		}
		// The ebook must still be present (we didn't over-filter).
		found := false
		for _, e := range f.Entries {
			if e.Title == "An Ebook" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s dropped the ebook", path)
		}
	}
}

func TestAudiobooksFeedListsOnlyAudio(t *testing.T) {
	h := handlerWithMixed(t)
	_, f := do(t, h, "/opds/audiobooks")
	if len(f.Entries) != 1 {
		t.Fatalf("audiobooks feed has %d entries, want 1", len(f.Entries))
	}
	e := f.Entries[0]
	if e.Title != "An Audiobook" {
		t.Errorf("audiobooks feed entry = %q, want An Audiobook", e.Title)
	}
	var acqType string
	for _, l := range e.Links {
		if l.Rel == relAcq {
			acqType = l.Type
		}
	}
	if acqType != typeAudio {
		t.Errorf("audiobook acquisition type = %q, want %q", acqType, typeAudio)
	}
}

func TestRootLinksAudiobooksFeed(t *testing.T) {
	h := handlerWithMixed(t)
	_, f := do(t, h, "/opds")
	found := false
	for _, e := range f.Entries {
		for _, l := range e.Links {
			if l.Href == "http://test.local/opds/audiobooks" {
				found = true
			}
		}
	}
	if !found {
		t.Error("nav root should link to the /opds/audiobooks feed")
	}
}

func TestAcquisitionTypeMapping(t *testing.T) {
	cases := map[string]string{
		"epub":    typeEpub,
		"cbz":     typeComic,
		"audio":   typeAudio,
		"unknown": typeEpub, // default
	}
	for format, want := range cases {
		if got := acquisitionType(format); got != want {
			t.Errorf("acquisitionType(%q) = %q, want %q", format, got, want)
		}
	}
}
