package comic

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// makeRAR builds a real .rar at dstRar from the files in srcDir using the `rar`
// CLI. It skips the test if rar is unavailable (e.g. CI without the tool), since
// there is no pure-Go RAR writer to fall back on. extraArgs lets callers request
// encryption etc.
func makeRAR(t *testing.T, dstRar, srcDir string, extraArgs ...string) {
	t.Helper()
	rar, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("rar CLI not available; skipping real-CBR conversion test")
	}
	// rar a [opts] <archive> <files...>; run inside srcDir so entry names are
	// relative (no absolute path prefix in the archive).
	args := append([]string{"a", "-ep1"}, extraArgs...)
	args = append(args, dstRar, ".")
	cmd := exec.Command(rar, args...)
	cmd.Dir = srcDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rar a failed: %v\n%s", err, out)
	}
}

func writePage(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), tinyPNG, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestConvertCBRRealArchive builds a genuine RAR of scrambled-name image pages,
// converts it to CBZ, and verifies the CBZ is valid and reads in natural order.
func TestConvertCBRRealArchive(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pages")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// Unpadded, out-of-order names so natural sort matters: 1,2,3,10.
	for _, n := range []string{"page10.png", "page2.png", "page1.png", "page3.png"} {
		writePage(t, src, n)
	}
	cbr := filepath.Join(dir, "comic.cbr")
	makeRAR(t, cbr, src) // skips if rar missing

	var lastDone, lastTotal int
	dst := filepath.Join(dir, "comic.cbz")
	if err := ConvertCBR(cbr, dst, func(done, total int) { lastDone, lastTotal = done, total }); err != nil {
		t.Fatalf("ConvertCBR: %v", err)
	}
	if lastTotal != 4 || lastDone != 4 {
		t.Errorf("final progress = %d/%d, want 4/4", lastDone, lastTotal)
	}

	// The output must be a valid CBZ with four pages in reading order.
	m, err := Read(dst)
	if err != nil {
		t.Fatalf("converted CBZ not readable: %v", err)
	}
	if m.PageCount != 4 {
		t.Errorf("PageCount = %d, want 4", m.PageCount)
	}
	pages, err := Pages(dst)
	if err != nil {
		t.Fatal(err)
	}
	// ConvertCBR re-names pages to a zero-padded sequence in reading order.
	want := []string{"1.png", "2.png", "3.png", "4.png"}
	for i := range want {
		if i >= len(pages) || pages[i] != want[i] {
			t.Fatalf("converted page order = %v, want %v", pages, want)
		}
	}
}

// TestConvertCBRNoImages errors cleanly (and leaves no output) when the archive
// has no page images.
func TestConvertCBRNoImages(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "stuff")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	cbr := filepath.Join(dir, "empty.cbr")
	makeRAR(t, cbr, src)

	dst := filepath.Join(dir, "empty.cbz")
	if err := ConvertCBR(cbr, dst, nil); err == nil {
		t.Error("ConvertCBR should error on an archive with no images")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("failed conversion must not leave a partial CBZ")
	}
}
