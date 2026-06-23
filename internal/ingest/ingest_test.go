package ingest

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steve/library/internal/catalog"
)

func TestImportable(t *testing.T) {
	yes := []string{"book.acsm", "book.epub", "BOOK.EPUB", "x.AcSm", "a.b.epub", "comic.cbz", "C.CBZ", "comic.cbr", "X.CBR"}
	no := []string{
		"book.pdf", "book.txt", "book", "cover.jpg", ".epubx", "epub",
		// Hidden/temp upload stages share an importable extension but must be
		// skipped so the watcher never grabs a partial file mid-write.
		".upload-331674232.cbz", ".upload-1.epub", ".upload-2.cbr",
		filepath.Join("dir", ".upload-9.cbz"),
	}
	for _, n := range yes {
		if !importable(n) {
			t.Errorf("importable(%q) = false, want true", n)
		}
	}
	for _, n := range no {
		if importable(n) {
			t.Errorf("importable(%q) = true, want false", n)
		}
	}
}

func TestSourceFor(t *testing.T) {
	if got := sourceFor("x.acsm"); got != "acsm" {
		t.Errorf("sourceFor(.acsm) = %q, want acsm", got)
	}
	if got := sourceFor("x.ACSM"); got != "acsm" {
		t.Errorf("sourceFor(.ACSM) = %q, want acsm (case-insensitive)", got)
	}
	if got := sourceFor("x.epub"); got != "epub-import" {
		t.Errorf("sourceFor(.epub) = %q, want epub-import", got)
	}
	if got := sourceFor("x.cbz"); got != "comic-import" {
		t.Errorf("sourceFor(.cbz) = %q, want comic-import", got)
	}
	if got := sourceFor("x.cbr"); got != "comic-import" {
		t.Errorf("sourceFor(.cbr) = %q, want comic-import", got)
	}
}

func TestUniquePath(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "Book.epub")

	// Nothing there yet: returns the path unchanged.
	if got := uniquePath(base); got != base {
		t.Errorf("uniquePath on free path = %q, want %q", got, base)
	}

	// Create it; next call must suffix " (2)".
	_ = os.WriteFile(base, []byte("x"), 0o644)
	want2 := filepath.Join(dir, "Book (2).epub")
	if got := uniquePath(base); got != want2 {
		t.Errorf("uniquePath with 1 collision = %q, want %q", got, want2)
	}

	// Create (2) as well; next must be " (3)".
	_ = os.WriteFile(want2, []byte("x"), 0o644)
	want3 := filepath.Join(dir, "Book (3).epub")
	if got := uniquePath(base); got != want3 {
		t.Errorf("uniquePath with 2 collisions = %q, want %q", got, want3)
	}
}

func TestWorkDir(t *testing.T) {
	im := &Importer{ImportDir: "/data/import"}
	if got := im.workDir(); got != "/data/import/work" {
		t.Errorf("workDir = %q, want /data/import/work", got)
	}
}

func TestMoveFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "sub", "dst.txt")
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	_ = os.WriteFile(src, []byte("hello"), 0o644)

	if err := moveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	// Source gone, dest has the content.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should be gone after moveFile")
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "hello" {
		t.Errorf("dst content = %q (err %v), want hello", got, err)
	}
}

// TestClaimDeduplicates covers the fix for fsnotify firing Create+Write on a
// single dropped file: the second claim must be refused until released.
func TestClaimDeduplicates(t *testing.T) {
	im := &Importer{}
	const p = "/data/import/x.acsm"
	if !im.claim(p) {
		t.Fatal("first claim should succeed")
	}
	if im.claim(p) {
		t.Fatal("second concurrent claim of the same path should be refused")
	}
	im.release(p)
	if !im.claim(p) {
		t.Fatal("after release, claim should succeed again")
	}
}

// tinyPNG is a 1x1 PNG used as a comic page in the import test.
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
	0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

// TestImportComicEndToEnd drops a real CBZ into the import dir, runs the full
// handle() pipeline (no DRM sidecar), and asserts the comic lands in the library
// as a .cbz, is indexed with format "cbz", gets a cached cover, and the job
// finishes Done with a slug. This exercises the comic branches in pipeline(),
// verify(), the destination extension, and catalog.Index together.
func TestImportComicEndToEnd(t *testing.T) {
	dir := t.TempDir()
	importDir := filepath.Join(dir, "import")
	libraryDir := filepath.Join(dir, "library")
	coversDir := filepath.Join(dir, "covers")
	for _, d := range []string{importDir, libraryDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"), libraryDir, coversDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	// A CBZ with ComicInfo.xml so the title/author are deterministic.
	src := filepath.Join(importDir, "dropped.cbz")
	writeCBZ(t, src, map[string][]byte{
		"ComicInfo.xml": []byte(`<ComicInfo><Title>Test Comic</Title><Writer>Test Author</Writer></ComicInfo>`),
		"page01.png":    tinyPNG,
		"page02.png":    tinyPNG,
	})

	im := &Importer{
		Cat:         cat,
		DRM:         nil, // comics never touch the sidecar
		ImportDir:   importDir,
		LibraryDir:  libraryDir,
		SidecarPath: func(p string) string { return p },
	}
	im.handle(context.Background(), src)

	// The comic must be in the library as Author/Title.cbz.
	wantPath := filepath.Join(libraryDir, "Test Author", "Test Comic.cbz")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("comic not in library at %s: %v", wantPath, err)
	}
	// The dropped original must be archived out of the top-level import dir.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("original CBZ still at import root (should be archived): err=%v", err)
	}

	// It must be indexed as a comic.
	books, err := cat.List(context.Background(), catalog.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 {
		t.Fatalf("catalog has %d books, want 1", len(books))
	}
	b := books[0]
	if b.Format != "cbz" {
		t.Errorf("format = %q, want cbz", b.Format)
	}
	if b.Title != "Test Comic" || len(b.Authors) != 1 || b.Authors[0] != "Test Author" {
		t.Errorf("metadata = %q by %v, want Test Comic by [Test Author]", b.Title, b.Authors)
	}
	if !b.HasCover {
		t.Error("comic should have a cover (first page)")
	}
	if p := cat.CoverCachePath(b); p == "" {
		t.Error("cover was not cached to disk")
	}

	// The import job must have finished Done with a slug.
	var done *Job
	for _, j := range im.JobRegistry().Snapshot() {
		done = j
	}
	if done == nil || done.State != StateDone {
		t.Fatalf("job state = %v, want done", done)
	}
	if done.BookSlug != b.Slug() {
		t.Errorf("job slug = %q, want %q", done.BookSlug, b.Slug())
	}
}

func writeCBZ(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestImportDRMFileWithoutSidecar verifies that with DRM disabled (DRM == nil),
// a dropped .acsm fails cleanly into import/failed/ instead of panicking on a nil
// sidecar client. Comics/DRM-free epubs are covered by TestImportComicEndToEnd.
func TestImportDRMFileWithoutSidecar(t *testing.T) {
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

	src := filepath.Join(importDir, "loan.acsm")
	if err := os.WriteFile(src, []byte("<fulfillmentToken/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	im := &Importer{
		Cat:         cat,
		DRM:         nil, // sidecar disabled
		ImportDir:   importDir,
		LibraryDir:  libraryDir,
		SidecarPath: func(p string) string { return p },
	}
	im.handle(context.Background(), src) // must not panic

	// The .acsm must have failed into import/failed/, not be in the library.
	if _, err := os.Stat(filepath.Join(importDir, "failed", "loan.acsm")); err != nil {
		t.Errorf("DRM file should land in import/failed/ when sidecar disabled: %v", err)
	}
	if books, _ := cat.List(context.Background(), catalog.ListOptions{}); len(books) != 0 {
		t.Errorf("a DRM file must not be cataloged with no sidecar; got %d books", len(books))
	}
	// The job should be marked failed with a clear reason.
	var failed *Job
	for _, j := range im.JobRegistry().Snapshot() {
		failed = j
	}
	if failed == nil || failed.State != StateFailed {
		t.Fatalf("job state = %v, want failed", failed)
	}
	if !strings.Contains(failed.Err, "DRM sidecar disabled") {
		t.Errorf("failure reason = %q, want it to mention DRM sidecar disabled", failed.Err)
	}
}
