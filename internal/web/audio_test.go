package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steve/library/internal/catalog"
)

// indexM4B builds a tiny chaptered .m4b with ffmpeg, drops it in the server's
// library dir, and indexes it. Returns the book's slug. Skips if ffmpeg absent.
func indexM4B(t *testing.T, s *Server) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping audio player tests")
	}
	libDir := libraryDir(t, s)
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(t.TempDir(), "ff.txt")
	const ffmeta = ";FFMETADATA1\ntitle=Player Book\nartist=Player Author\nalbum_artist=The Narrator\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=3000\ntitle=One\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=3000\nEND=6000\ntitle=Two\n"
	if err := os.WriteFile(meta, []byte(ffmeta), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(libDir, "player.m4b")
	cmd := exec.Command("ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-t", "6", "-i", "anullsrc=r=44100:cl=mono",
		"-i", meta, "-map", "0:a", "-map_metadata", "1", "-map_chapters", "1",
		"-c:a", "aac", "-b:a", "32k", "-movflags", "+faststart", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg build failed: %v\n%s", err, b)
	}
	if _, err := s.Cat.Index(context.Background(), out, "scan"); err != nil {
		t.Fatal(err)
	}
	books, _ := s.Cat.List(context.Background(), catalog.ListOptions{})
	if len(books) != 1 {
		t.Fatalf("want 1 indexed book, got %d", len(books))
	}
	return books[0].Slug()
}

func TestAudioChapters(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	slug := indexM4B(t, s)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/book/"+slug+"/chapters", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("chapters status = %d", w.Code)
	}
	var got struct {
		Duration float64 `json:"duration"`
		Chapters []struct {
			Title string  `json:"title"`
			Start float64 `json:"start"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
	}
	if len(got.Chapters) != 2 {
		t.Fatalf("got %d chapters, want 2: %+v", len(got.Chapters), got.Chapters)
	}
	if got.Chapters[0].Title != "One" || got.Chapters[1].Start != 3 {
		t.Errorf("chapters = %+v, want One@0 Two@3", got.Chapters)
	}
	if got.Duration < 5.5 || got.Duration > 6.5 {
		t.Errorf("duration = %v, want ~6", got.Duration)
	}
}

func TestAudioStreamSupportsRange(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	slug := indexM4B(t, s)

	// A range request must yield 206 + the requested slice (seeking depends on
	// this). ServeFile advertises Accept-Ranges and honors Range.
	req := httptest.NewRequest(http.MethodGet, "/book/"+slug+"/audio", nil)
	req.Header.Set("Range", "bytes=0-99")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", w.Code)
	}
	if w.Body.Len() != 100 {
		t.Errorf("range body = %d bytes, want 100", w.Body.Len())
	}
	if ct := w.Header().Get("Content-Type"); ct != "audio/mp4" {
		t.Errorf("content-type = %q, want audio/mp4", ct)
	}
	// Must NOT be an attachment (that would force a download, not play inline).
	if cd := w.Header().Get("Content-Disposition"); strings.Contains(cd, "attachment") {
		t.Errorf("audio stream set Content-Disposition: %q (should be inline)", cd)
	}
}

func TestAudioChaptersRejectsNonAudio(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	// An epub indexed; /chapters must 404.
	p := writeLibraryEPUB(t, libraryDir(t, s), "An Epub.epub", "An Epub", "Author")
	if _, err := s.Cat.Index(context.Background(), p, "scan"); err != nil {
		t.Fatal(err)
	}
	books, _ := s.Cat.List(context.Background(), catalog.ListOptions{})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/book/"+books[0].Slug()+"/chapters", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("epub /chapters status = %d, want 404", w.Code)
	}
}

// TestReaderRendersAudioPlayer confirms /read/{slug} for an audiobook renders
// the audio player template (native <audio>, the chapters list, skip buttons),
// not the 501 placeholder or the epub reader.
func TestReaderRendersAudioPlayer(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	slug := indexM4B(t, s)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/read/"+slug, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("reader status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`<audio id="player"`,             // native player
		`src="/book/` + slug + `/audio"`, // inline stream source
		`id="chapters"`,                  // chapter list container
		`audioSkip(-30)`,                 // skip back
		`audioSkip(30)`,                  // skip forward
		`/static/js/audio.js`,            // the player script
		"The Narrator",                   // narrator surfaced
	} {
		if !strings.Contains(body, want) {
			t.Errorf("audio player HTML missing %q", want)
		}
	}
}
