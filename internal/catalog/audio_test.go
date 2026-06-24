package catalog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// makeM4B writes a tiny tagged, chaptered M4B into dir/name using ffmpeg, the
// shape a clean audiobook has after the sidecar decrypts a .aax. ffmpeg is a
// TEST-only dependency; the catalog parses the file in pure Go. Skips if ffmpeg
// is unavailable so the suite stays green on hosts without it.
func makeM4B(t *testing.T, dir, name string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping m4b catalog test")
	}
	meta := filepath.Join(t.TempDir(), "ff.txt")
	const ffmeta = `;FFMETADATA1
title=The Audio Book
artist=Audio Author
album_artist=The Narrator
date=2021
[CHAPTER]
TIMEBASE=1/1000
START=0
END=3000
title=Intro
`
	if err := os.WriteFile(meta, []byte(ffmeta), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-t", "3", "-i", "anullsrc=r=44100:cl=mono",
		"-i", meta, "-map", "0:a", "-map_metadata", "1", "-map_chapters", "1",
		"-c:a", "aac", "-b:a", "32k", "-movflags", "+faststart", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg m4b fixture failed: %v\n%s", err, b)
	}
	return out
}

// TestIndexAudiobook indexes a clean .m4b and checks it lands as format="audio"
// with the narrator and duration read from the file, plus the shared
// title/author fields.
func TestIndexAudiobook(t *testing.T) {
	dir := t.TempDir()
	library := filepath.Join(dir, "library")
	if err := os.MkdirAll(library, 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := Open(filepath.Join(dir, "catalog.db"), library, filepath.Join(dir, "covers"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	p := makeM4B(t, library, "book.m4b")
	if _, err := c.Index(context.Background(), p, "scan"); err != nil {
		t.Fatal(err)
	}

	books, err := c.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 {
		t.Fatalf("want 1 book, got %d", len(books))
	}
	b := books[0]
	if b.Format != "audio" {
		t.Errorf("Format = %q, want audio", b.Format)
	}
	if b.Title != "The Audio Book" {
		t.Errorf("Title = %q, want The Audio Book", b.Title)
	}
	if len(b.Authors) != 1 || b.Authors[0] != "Audio Author" {
		t.Errorf("Authors = %v, want [Audio Author]", b.Authors)
	}
	if b.Narrator != "The Narrator" {
		t.Errorf("Narrator = %q, want The Narrator", b.Narrator)
	}
	if b.Duration < 2.5 || b.Duration > 3.5 {
		t.Errorf("Duration = %v, want ~3", b.Duration)
	}
}

// TestAudiobookEmbedRefused confirms a metadata edit on an audiobook is NOT
// written back into the file (internal/audio has no writer): EmbedMetadata must
// refuse cleanly rather than fall through to the epub writer and corrupt the
// .m4b.
func TestAudiobookEmbedRefused(t *testing.T) {
	dir := t.TempDir()
	library := filepath.Join(dir, "library")
	if err := os.MkdirAll(library, 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := Open(filepath.Join(dir, "catalog.db"), library, filepath.Join(dir, "covers"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	p := makeM4B(t, library, "book.m4b")
	if _, err := c.Index(context.Background(), p, "scan"); err != nil {
		t.Fatal(err)
	}
	books, _ := c.List(context.Background(), ListOptions{})
	if len(books) != 1 {
		t.Fatalf("want 1 book, got %d", len(books))
	}
	slug := books[0].Slug()

	res, err := c.EmbedMetadata(context.Background(), slug, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Embedded {
		t.Error("audiobook metadata was embedded into the file; it must be refused")
	}
	if res.Reason == "" {
		t.Error("expected a reason explaining why the audiobook edit was not embedded")
	}
}
