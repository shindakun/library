package ingest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/steve/library/internal/audible"
	"github.com/steve/library/internal/catalog"
)

// buildM4B writes a tiny tagged audiobook .m4b at out using ffmpeg (a test-only
// dependency; the importer never shells out). Skips if ffmpeg is absent.
func buildM4B(t *testing.T, out string) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping audiobook import test")
	}
	meta := filepath.Join(t.TempDir(), "ff.txt")
	const ffmeta = ";FFMETADATA1\ntitle=Imported Audiobook\nartist=Imported Author\nalbum_artist=The Narrator\n"
	if err := os.WriteFile(meta, []byte(ffmeta), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-t", "2", "-i", "anullsrc=r=44100:cl=mono",
		"-i", meta, "-map", "0:a", "-map_metadata", "1",
		"-c:a", "aac", "-b:a", "32k", "-movflags", "+faststart", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg m4b build failed: %v\n%s", err, b)
	}
}

// mockAudibleSidecar serves the /job decrypt contract: on a decrypt request it
// builds a real .m4b in workDir (named after the input base) and returns its
// path, exactly as the real sidecar would after ffmpeg finishes. /health reports
// configured so the importer treats it as ready.
func mockAudibleSidecar(t *testing.T, workDir string) *audible.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true,"activation":true}`))
		case "/job":
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Op    string `json:"op"`
				Input string `json:"input"`
			}
			_ = json.Unmarshal(body, &req)
			base := filepath.Base(req.Input)
			base = base[:len(base)-len(filepath.Ext(base))]
			if err := os.MkdirAll(workDir, 0o755); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(workDir, base+".m4b")
			buildM4B(t, out)
			resp := map[string]any{"ok": true, "output": out, "format": "m4b"}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return audible.New(srv.URL)
}

// TestImportAudiobookEndToEnd drops a .aax, runs the full handle() pipeline with
// a mock audiobook sidecar that produces a clean .m4b, and asserts the book
// lands in the library as Author/Title.m4b, is indexed as format "audio", and
// the dropped .aax is archived. Exercises the .aax branch in pipeline(),
// decryptAudible, verify() for .m4b, and the shared tail together.
func TestImportAudiobookEndToEnd(t *testing.T) {
	dir := t.TempDir()
	importDir := filepath.Join(dir, "import")
	libraryDir := filepath.Join(dir, "library")
	for _, d := range []string{importDir, libraryDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"), libraryDir, filepath.Join(dir, "covers"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	// The dropped .aax: its CONTENT doesn't matter here (the mock sidecar
	// produces the .m4b), only that the pipeline routes it correctly.
	src := filepath.Join(importDir, "dropped.aax")
	if err := os.WriteFile(src, []byte("not a real aax, the mock sidecar makes the m4b"), 0o644); err != nil {
		t.Fatal(err)
	}

	im := &Importer{
		Cat:         cat,
		ImportDir:   importDir,
		LibraryDir:  libraryDir,
		SidecarPath: func(p string) string { return p },
	}
	im.Audible = mockAudibleSidecar(t, im.workDir())
	im.handle(context.Background(), src)

	// In the library as Author/Title.m4b.
	wantPath := filepath.Join(libraryDir, "Imported Author", "Imported Audiobook.m4b")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("audiobook not in library at %s: %v", wantPath, err)
	}
	// Dropped .aax archived out of the import root.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("original .aax still at import root (should be archived): err=%v", err)
	}

	books, err := cat.List(context.Background(), catalog.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 {
		t.Fatalf("catalog has %d books, want 1", len(books))
	}
	b := books[0]
	if b.Format != "audio" {
		t.Errorf("format = %q, want audio", b.Format)
	}
	if b.Title != "Imported Audiobook" || len(b.Authors) != 1 || b.Authors[0] != "Imported Author" {
		t.Errorf("metadata = %q by %v, want Imported Audiobook by [Imported Author]", b.Title, b.Authors)
	}
	if b.Narrator != "The Narrator" {
		t.Errorf("narrator = %q, want The Narrator", b.Narrator)
	}

	// Job finished Done with a slug.
	var done *Job
	for _, j := range im.JobRegistry().Snapshot() {
		done = j
	}
	if done == nil || done.State != StateDone {
		t.Fatalf("job state = %v, want done", done)
	}
}

// TestImportAudiobookWithoutSidecar verifies that with the audiobook sidecar
// disabled (Audible == nil), a dropped .aax fails cleanly into import/failed/
// rather than panicking on a nil client.
func TestImportAudiobookWithoutSidecar(t *testing.T) {
	dir := t.TempDir()
	importDir := filepath.Join(dir, "import")
	libraryDir := filepath.Join(dir, "library")
	for _, d := range []string{importDir, libraryDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"), libraryDir, filepath.Join(dir, "covers"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	src := filepath.Join(importDir, "book.aax")
	if err := os.WriteFile(src, []byte("aax bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	im := &Importer{
		Cat:         cat,
		Audible:     nil, // sidecar disabled
		ImportDir:   importDir,
		LibraryDir:  libraryDir,
		SidecarPath: func(p string) string { return p },
	}
	im.handle(context.Background(), src) // must not panic

	// The .aax must have failed into import/failed/, not be in the library.
	if _, err := os.Stat(filepath.Join(importDir, "failed", "book.aax")); err != nil {
		t.Errorf("disabled-sidecar .aax not in failed/: %v", err)
	}
	books, _ := cat.List(context.Background(), catalog.ListOptions{})
	if len(books) != 0 {
		t.Errorf("catalog has %d books, want 0 (import should have failed)", len(books))
	}
}

// TestImportAaxcMovesVoucher drops an .aaxc plus its sibling .voucher, imports it
// through the mock sidecar, and asserts both the .aaxc AND the voucher are
// archived to done/ (the voucher must not be orphaned in the import root).
func TestImportAaxcMovesVoucher(t *testing.T) {
	dir := t.TempDir()
	importDir := filepath.Join(dir, "import")
	libraryDir := filepath.Join(dir, "library")
	for _, d := range []string{importDir, libraryDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"), libraryDir, filepath.Join(dir, "covers"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	src := filepath.Join(importDir, "dropped.aaxc")
	if err := os.WriteFile(src, []byte("aaxc bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	voucher := filepath.Join(importDir, "dropped.voucher")
	if err := os.WriteFile(voucher, []byte(`{"content_license":{"license_response":{"key":"k","iv":"v"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	im := &Importer{
		Cat:         cat,
		ImportDir:   importDir,
		LibraryDir:  libraryDir,
		SidecarPath: func(p string) string { return p },
	}
	im.Audible = mockAudibleSidecar(t, im.workDir())
	im.handle(context.Background(), src)

	// The audiobook landed in the library.
	if _, err := os.Stat(filepath.Join(libraryDir, "Imported Author", "Imported Audiobook.m4b")); err != nil {
		t.Fatalf("audiobook not in library: %v", err)
	}
	// Both the .aaxc and its voucher are archived to done/, neither left behind.
	if _, err := os.Stat(filepath.Join(importDir, "done", "dropped.aaxc")); err != nil {
		t.Errorf(".aaxc not archived to done/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(importDir, "done", "dropped.voucher")); err != nil {
		t.Errorf("voucher not moved to done/ (orphaned in import root?): %v", err)
	}
	if _, err := os.Stat(voucher); !os.IsNotExist(err) {
		t.Errorf("voucher still in import root (should have moved): err=%v", err)
	}
}

func TestAaxcIsImportableAndAudible(t *testing.T) {
	if !importable("book.aaxc") || !importable("BOOK.AAXC") {
		t.Error("importable should accept .aaxc")
	}
	if !isAudible("book.aaxc") {
		t.Error("isAudible should be true for .aaxc")
	}
	if sourceFor("book.aaxc") != "audible-import" {
		t.Errorf("sourceFor(.aaxc) = %q, want audible-import", sourceFor("book.aaxc"))
	}
}
