package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImportable(t *testing.T) {
	yes := []string{"book.acsm", "book.epub", "BOOK.EPUB", "x.AcSm", "a.b.epub"}
	no := []string{"book.pdf", "book.txt", "book", "cover.jpg", ".epubx", "epub"}
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
