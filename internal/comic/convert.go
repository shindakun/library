package comic

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/nwaples/rardecode/v2"
)

// ConvertCBR converts a CBR (a RAR of page images) into a CBZ (a ZIP) at
// dstPath, normalizing it to the pure-ZIP form the rest of the system handles.
// It reports progress via onProgress (pagesDone/total) if non-nil. The page
// images are re-zipped in natural reading order (matching pageList), so the
// resulting CBZ reads correctly regardless of the RAR's internal entry order.
//
// Errors (and encrypted/unsupported archives) leave no partial CBZ behind: a
// failure removes dstPath, so a failed import never yields a half-written file.
func ConvertCBR(srcPath, dstPath string, onProgress func(done, total int)) (err error) {
	// First pass: enumerate image entries (header-only, no decompression) to get
	// a total for progress and to fail fast on an encrypted/unreadable archive.
	names, err := cbrImageEntries(srcPath)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("cbr has no page images")
	}
	want := make(map[string]int, len(names)) // entry name -> reading-order index
	for i, n := range names {
		want[n] = i
	}

	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	// On any error, close and remove the partial output so nothing half-written
	// is left for the importer to pick up.
	defer func() {
		if err != nil {
			_ = out.Close()
			_ = os.Remove(dstPath)
		}
	}()
	zw := zip.NewWriter(out)

	// Second pass: extract each image and STREAM it straight into the CBZ, one
	// page at a time (no buffering all pages in memory: a large comic is hundreds
	// of MB decompressed). Each page is named by its precomputed reading-order
	// index (zero-padded), so the output sorts into reading order on read even
	// though we write entries in the archive's order. Progress is reported per
	// page as it is fully written, so the bar tracks the real work (read+write),
	// not just the read.
	width := len(fmt.Sprintf("%d", len(names)))
	rc, err := rardecode.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("open cbr: %w", err)
	}
	defer func() { _ = rc.Close() }()

	// The page images store essentially no better under deflate (JPEG/PNG are
	// already compressed), so store them uncompressed: far faster and avoids
	// burning CPU re-compressing incompressible data on a big volume.
	done := 0
	for {
		hdr, nerr := rc.Next()
		if nerr == io.EOF {
			break
		}
		if nerr != nil {
			return fmt.Errorf("read cbr entry: %w", nerr)
		}
		if hdr.Encrypted || hdr.HeaderEncrypted {
			return fmt.Errorf("cbr is encrypted (unsupported)")
		}
		idx, ok := want[hdr.Name]
		if !ok {
			continue // non-image entry (or a dir); skip, matching pageList
		}
		name := fmt.Sprintf("%0*d%s", width, idx+1, strings.ToLower(path.Ext(hdr.Name)))
		w, werr := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if werr != nil {
			return werr
		}
		if _, werr := io.Copy(w, rc); werr != nil {
			return fmt.Errorf("extract %q: %w", hdr.Name, werr)
		}
		done++
		if onProgress != nil {
			onProgress(done, len(names))
		}
	}

	if err = zw.Close(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	return nil
}

// cbrImageEntries returns the image entry names inside a CBR in natural reading
// order (header pass only). It also surfaces an encrypted archive as an error.
func cbrImageEntries(srcPath string) ([]string, error) {
	rc, err := rardecode.OpenReader(srcPath)
	if err != nil {
		return nil, fmt.Errorf("open cbr: %w", err)
	}
	defer func() { _ = rc.Close() }()

	var names []string
	for {
		hdr, nerr := rc.Next()
		if nerr == io.EOF {
			break
		}
		if nerr != nil {
			return nil, fmt.Errorf("read cbr header: %w", nerr)
		}
		if hdr.HeaderEncrypted {
			return nil, fmt.Errorf("cbr is encrypted (unsupported)")
		}
		if hdr.IsDir {
			continue
		}
		base := path.Base(hdr.Name)
		if strings.EqualFold(base, "ComicInfo.xml") {
			continue
		}
		if !imageExts[strings.ToLower(path.Ext(hdr.Name))] {
			continue
		}
		names = append(names, hdr.Name)
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	return names, nil
}
