package audio

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// writeTinyPNG writes a 2x2 red PNG to path using only the stdlib.
func writeTinyPNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for x := 0; x < 2; x++ {
		for y := 0; y < 2; y++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// buildFixtureM4B writes a tiny chaptered M4B to dir using ffmpeg and returns
// its path. ffmpeg is a TEST-ONLY dependency here (it generates the fixture);
// the package under test never shells out. If ffmpeg is unavailable the caller
// skips, so the suite stays green on hosts without it.
func buildFixtureM4B(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping m4b fixture tests")
	}
	dir := t.TempDir()
	meta := filepath.Join(dir, "chapters.txt")
	const ffmeta = `;FFMETADATA1
title=Test Audiobook
artist=Test Author
album_artist=Test Narrator
date=2024-03-01T00:00:00Z
[CHAPTER]
TIMEBASE=1/1000
START=0
END=2000
title=Chapter One
[CHAPTER]
TIMEBASE=1/1000
START=2000
END=4000
title=Chapter Two
[CHAPTER]
TIMEBASE=1/1000
START=4000
END=6000
title=Chapter Three
`
	if err := os.WriteFile(meta, []byte(ffmeta), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "book.m4b")
	// 6 seconds of silence, AAC, with the tags + chapters above. -movflags
	// +faststart puts moov first, exactly like the sidecar's decrypt output.
	cmd := exec.Command("ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-t", "6", "-i", "anullsrc=r=44100:cl=mono",
		"-i", meta, "-map", "0:a", "-map_metadata", "1", "-map_chapters", "1",
		"-c:a", "aac", "-b:a", "32k", "-movflags", "+faststart", out)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg fixture build failed: %v\n%s", err, outBytes)
	}
	return out
}

func TestReadTagsAndDuration(t *testing.T) {
	m4b := buildFixtureM4B(t)
	m, err := Read(m4b)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Test Audiobook" {
		t.Errorf("Title = %q, want Test Audiobook", m.Title)
	}
	if len(m.Authors) != 1 || m.Authors[0] != "Test Author" {
		t.Errorf("Authors = %v, want [Test Author]", m.Authors)
	}
	if m.Narrator != "Test Narrator" {
		t.Errorf("Narrator = %q, want Test Narrator", m.Narrator)
	}
	if m.Published != "2024" {
		t.Errorf("Published = %q, want 2024", m.Published)
	}
	// 6s of audio; AAC framing can make the container a hair over/under, so
	// allow a small tolerance.
	if m.Duration < 5.5 || m.Duration > 6.5 {
		t.Errorf("Duration = %v, want ~6", m.Duration)
	}
}

func TestReadChapters(t *testing.T) {
	m4b := buildFixtureM4B(t)
	m, err := Read(m4b)
	if err != nil {
		t.Fatal(err)
	}
	want := []Chapter{
		{Title: "Chapter One", Start: 0},
		{Title: "Chapter Two", Start: 2},
		{Title: "Chapter Three", Start: 4},
	}
	if len(m.Chapters) != len(want) {
		t.Fatalf("got %d chapters, want %d: %+v", len(m.Chapters), len(want), m.Chapters)
	}
	for i, w := range want {
		g := m.Chapters[i]
		if g.Title != w.Title {
			t.Errorf("chapter %d title = %q, want %q", i, g.Title, w.Title)
		}
		// chpl start times are exact (we set them); allow 1ms slack.
		if d := g.Start - w.Start; d < -0.001 || d > 0.001 {
			t.Errorf("chapter %d start = %v, want %v", i, g.Start, w.Start)
		}
	}
}

// TestChaptersFromTextTrack exercises the QuickTime chapter-text-track path,
// the form REAL Audible files use (they have no chpl box). The ffmpeg fixture
// carries both a chpl and a text track, and Read prefers chpl; renaming the
// chpl box to "free" forces the text-track fallback, which must yield the same
// chapters. (Validated against a real .aax during development; that file can't
// live in the repo, so this stand-in keeps the path covered in CI.)
func TestChaptersFromTextTrack(t *testing.T) {
	m4b := buildFixtureM4B(t)
	data, err := os.ReadFile(m4b)
	if err != nil {
		t.Fatal(err)
	}
	i := indexOf(data, "chpl")
	if i < 0 {
		t.Skip("fixture has no chpl box to strip")
	}
	// Rename the box type in place; a 'free' box is skipped by the walker, so
	// Read falls through to the text track.
	copy(data[i:i+4], []byte("free"))
	out := filepath.Join(filepath.Dir(m4b), "no-chpl.m4b")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := Read(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Chapters) != 3 {
		t.Fatalf("got %d chapters via text track, want 3: %+v", len(m.Chapters), m.Chapters)
	}
	if m.Chapters[0].Title != "Chapter One" {
		t.Errorf("chapter 0 title = %q, want Chapter One", m.Chapters[0].Title)
	}
	if d := m.Chapters[1].Start - 2; d < -0.05 || d > 0.05 {
		t.Errorf("chapter 1 start = %v, want ~2", m.Chapters[1].Start)
	}
}

// indexOf returns the first index of sub in b, or -1.
func indexOf(b []byte, sub string) int {
	s := []byte(sub)
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == sub {
			return i
		}
	}
	return -1
}

func TestReadTitleFallsBackToFilename(t *testing.T) {
	// A non-m4b path with no moov must error, not silently return a title.
	dir := t.TempDir()
	bad := filepath.Join(dir, "not-audio.m4b")
	if err := os.WriteFile(bad, []byte("this is not an mp4 container at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(bad); err == nil {
		t.Error("expected an error reading a non-mp4 file, got nil")
	}
}

func TestCoverImageAbsent(t *testing.T) {
	// The fixture has no covr atom, so CoverImage must report that cleanly.
	m4b := buildFixtureM4B(t)
	if _, _, err := CoverImage(m4b); err == nil {
		t.Error("expected an error for a file with no cover, got nil")
	}
}

// buildFixtureM4BWithCover writes a chaptered M4B that also carries an embedded
// PNG cover (as an attached_pic, the form ffmpeg emits and Audible files use).
func buildFixtureM4BWithCover(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping cover test")
	}
	dir := t.TempDir()
	meta := filepath.Join(dir, "chapters.txt")
	if err := os.WriteFile(meta, []byte(";FFMETADATA1\ntitle=Covered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A minimal valid PNG (2x2 red), written with the stdlib so the test needs
	// no external image.
	png := filepath.Join(dir, "cover.png")
	writeTinyPNG(t, png)
	out := filepath.Join(dir, "covered.m4b")
	cmd := exec.Command("ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-t", "2", "-i", "anullsrc=r=44100:cl=mono",
		"-i", meta, "-i", png,
		"-map", "0:a", "-map", "2:v", "-map_metadata", "1",
		"-c:a", "aac", "-b:a", "32k", "-c:v", "png", "-disposition:v", "attached_pic",
		"-movflags", "+faststart", out)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg cover fixture failed: %v\n%s", err, outBytes)
	}
	return out
}

func TestCoverImagePresent(t *testing.T) {
	m4b := buildFixtureM4BWithCover(t)
	data, mime, err := CoverImage(m4b)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("cover bytes are empty")
	}
	// We embedded a PNG; CoverImage should report it as such (covr type flag 14
	// or the \x89PNG sniff).
	if mime != "image/png" {
		t.Errorf("mime = %q, want image/png", mime)
	}
	if string(data[:4]) != "\x89PNG" {
		t.Errorf("cover does not start with the PNG magic: %x", data[:4])
	}
	// And HasCover must be set on Read.
	m, err := Read(m4b)
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasCover {
		t.Error("HasCover = false, want true for a file with a covr atom")
	}
}

func TestParseChplUnit(t *testing.T) {
	// A hand-built chpl payload (the bytes inside the box, after size+type):
	// version(1) flags(3) reserved(4) count(1)=2, then two chapters.
	chpl := []byte{
		0x01, 0x00, 0x00, 0x00, // version + flags
		0x00, 0x00, 0x00, 0x00, // reserved
		0x02, // count
	}
	// chapter 1: start=0 (8 bytes), len=2, "Hi"
	chpl = append(chpl, 0, 0, 0, 0, 0, 0, 0, 0, 0x02, 'H', 'i')
	// chapter 2: start = 1.5s = 15,000,000 in 100-ns units, len=3, "Two"
	start := uint64(15_000_000)
	chpl = append(chpl,
		byte(start>>56), byte(start>>48), byte(start>>40), byte(start>>32),
		byte(start>>24), byte(start>>16), byte(start>>8), byte(start),
		0x03, 'T', 'w', 'o')

	got := parseChpl(chpl)
	if len(got) != 2 {
		t.Fatalf("got %d chapters, want 2: %+v", len(got), got)
	}
	if got[0].Title != "Hi" || got[0].Start != 0 {
		t.Errorf("chapter 0 = %+v, want {Hi 0}", got[0])
	}
	if got[1].Title != "Two" || got[1].Start != 1.5 {
		t.Errorf("chapter 1 = %+v, want {Two 1.5}", got[1])
	}
}

func TestParseChplTruncated(t *testing.T) {
	// A count that claims more chapters than the bytes provide must not panic;
	// it returns what it can.
	chpl := []byte{0x01, 0, 0, 0, 0, 0, 0, 0, 0x05} // claims 5, no chapter data
	if got := parseChpl(chpl); len(got) != 0 {
		t.Errorf("got %d chapters from truncated chpl, want 0", len(got))
	}
}
