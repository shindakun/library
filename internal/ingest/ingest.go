// Package ingest watches the import directory and runs the DRM pipeline on new
// files (fulfill .acsm -> decrypt ADEPT -> index), driving the sidecar via the
// drm client. It is independent of the HTTP layer: the only seam with web is
// that the upload handler writes a file into the watched import dir.
package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/steve/library/internal/catalog"
	"github.com/steve/library/internal/comic"
	"github.com/steve/library/internal/drm"
	"github.com/steve/library/internal/epub"
	"github.com/steve/library/internal/fileutil"
)

// Importer watches the import directory and runs the DRM pipeline on new files,
// then hands the clean EPUB to the catalog. It is the only thing that drives
// the sidecar.
//
// Layout under importDir:
//
//	import/        <- drop *.acsm or *.epub here (watched)
//	import/work/   <- sidecar scratch: fulfilled + clean epubs (NOT watched)
//	import/done/   <- originals move here on success
//	import/failed/ <- originals move here on failure (with a .log sibling)
type Importer struct {
	Cat        *catalog.Catalog
	DRM        *drm.Client
	ImportDir  string // host path watched for new files
	LibraryDir string // where clean epubs land
	// SidecarPath maps a host import path to the path the sidecar sees for the
	// same file (shared volume mounted at a possibly-different path). For the
	// common case where both mount the same volume at the same path, set it to
	// the identity function.
	SidecarPath func(hostPath string) string

	mu         sync.Mutex
	inFlight   map[string]bool // paths currently being processed (dedupe Create+Write)
	pipelineMu sync.Mutex      // serializes imports so work-dir cleanup is unambiguous

	jobsOnce sync.Once
	jobs     *Jobs
}

// JobRegistry returns the importer's import-job registry (lazily created),
// which the web layer reads for the /imports page and SSE stream.
func (im *Importer) JobRegistry() *Jobs {
	im.jobsOnce.Do(func() { im.jobs = newJobs() })
	return im.jobs
}

// cleanWorkDir empties the sidecar scratch dir. Safe because imports are
// serialized by im.pipelineMu (see handle): only one job uses work/ at a time.
func (im *Importer) cleanWorkDir() {
	entries, err := os.ReadDir(im.workDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(im.workDir(), e.Name()))
	}
}

// claim returns false if path is already being processed, otherwise marks it
// in-flight. Create and Write events both fire for a single dropped file; this
// keeps the pipeline from running twice on it.
func (im *Importer) claim(path string) bool {
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.inFlight == nil {
		im.inFlight = map[string]bool{}
	}
	if im.inFlight[path] {
		return false
	}
	im.inFlight[path] = true
	return true
}

func (im *Importer) release(path string) {
	im.mu.Lock()
	delete(im.inFlight, path)
	im.mu.Unlock()
}

// workDir is the sidecar scratch subdir. Intermediates (the fulfilled encrypted
// epub) and the clean output land here, OUTSIDE the watched set, so the watcher
// never re-triggers on its own pipeline's byproducts.
func (im *Importer) workDir() string { return filepath.Join(im.ImportDir, "work") }

// Run blocks watching ImportDir until ctx is cancelled. It also processes any
// files already present at startup.
func (im *Importer) Run(ctx context.Context) error {
	for _, sub := range []string{"work", "done", "failed"} {
		if err := os.MkdirAll(filepath.Join(im.ImportDir, sub), 0o755); err != nil {
			return fmt.Errorf("create import subdir %s: %w", sub, err)
		}
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()
	if err := w.Add(im.ImportDir); err != nil {
		return err
	}

	// Sweep anything already present at startup.
	im.sweep(ctx)

	// Periodic sweep as a fallback. fsnotify/inotify events do NOT cross some
	// bind-mount boundaries (notably macOS -> Podman/Docker VM via virtiofs/9p),
	// so on those hosts the event path below never fires. Polling makes the
	// importer work everywhere; the dedupe in claim() keeps it from racing the
	// event path on hosts where both deliver.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			im.sweep(ctx)
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write) != 0 && importable(ev.Name) {
				path := ev.Name
				// Debounce: let the writer finish before we touch it.
				go func() {
					time.Sleep(750 * time.Millisecond)
					im.handle(ctx, path)
				}()
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintf(os.Stderr, "watcher error: %v\n", err)
		}
	}
}

// sweep processes every importable file currently sitting at the top level of
// the import dir. Used both at startup and as the periodic polling fallback.
func (im *Importer) sweep(ctx context.Context) {
	entries, err := os.ReadDir(im.ImportDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !importable(e.Name()) {
			continue
		}
		// Skip files still being written: if mtime is very recent, let the next
		// sweep catch it once the writer has settled.
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) < time.Second {
			continue
		}
		go im.handle(ctx, filepath.Join(im.ImportDir, e.Name()))
	}
}

func (im *Importer) handle(ctx context.Context, hostPath string) {
	if !im.claim(hostPath) {
		return // already being processed (Create+Write both fired)
	}
	defer im.release(hostPath)
	if _, err := os.Stat(hostPath); err != nil {
		return // already moved
	}

	// Register the job BEFORE taking the pipeline lock, so a job waiting its turn
	// shows as "queued" in the UI (imports are serialized; only one runs at once).
	name := filepath.Base(hostPath)
	jobs := im.JobRegistry()
	jobID := jobs.Start(name, sourceFor(hostPath))

	// Serialize imports: the sidecar is single-tenant and the work dir is shared
	// scratch, so one job at a time keeps cleanup unambiguous.
	im.pipelineMu.Lock()
	defer im.pipelineMu.Unlock()
	// Always clear the sidecar scratch when this job ends, so the encrypted
	// intermediate (and any clean file left behind on a mid-pipeline error)
	// never accumulates. The work dir is pure transient scratch.
	defer im.cleanWorkDir()

	jobs.Update(jobID, func(j *Job) { j.Step = "processing" })
	fmt.Printf("import: processing %s\n", name)

	// failJob logs, moves the original to failed/, and marks the job failed.
	failJob := func(cause error) {
		im.fail(hostPath, cause)
		jobs.Finish(jobID, StateFailed, cause.Error(), "")
	}

	onProgress := func(step string, frac float64, detail string) {
		jobs.Update(jobID, func(j *Job) { j.Step = step; j.Progress = frac; j.Detail = detail })
	}

	cleanHostPath, err := im.pipeline(ctx, hostPath, onProgress)
	if err != nil {
		failJob(err)
		return
	}

	// Verify it's a real, parseable book BEFORE committing it to the library:
	// the metadata read doubles as a "is this actually a valid file" check, and
	// the title/authors give us a clean library filename. Branch on format: an
	// epub is parsed by epub.Read, a comic by comic.Read.
	jobs.Update(jobID, func(j *Job) { j.Step = "verifying" })
	authors, title, err := verify(cleanHostPath)
	if err != nil {
		failJob(fmt.Errorf("verify clean file: %w", err))
		return
	}

	// Skip byte-identical duplicates: if a book with this exact content hash is
	// already in the library, importing it again would make a second copy on disk
	// and a second catalog row sharing the same content-hash slug (ambiguous URL).
	if hash, herr := catalog.FileHash(cleanHostPath); herr == nil {
		if dup, _ := im.Cat.HasHash(ctx, hash); dup {
			fmt.Printf("import: %s is already in the library (duplicate content), skipping\n", name)
			im.archive(hostPath, "done")
			jobs.Finish(jobID, StateSkipped, "already in the library (duplicate content)", "")
			return
		}
	}

	// Organize on disk as Author/Title.<ext>, preserving the source extension so
	// comics land as .cbz and epubs as .epub.
	jobs.Update(jobID, func(j *Job) { j.Step = "indexing" })
	dest := filepath.Join(im.LibraryDir, fileutil.LibraryRelPath(authors, title, filepath.Ext(cleanHostPath)))
	dest = uniquePath(dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		failJob(fmt.Errorf("create author dir: %w", err))
		return
	}
	if err := moveFile(cleanHostPath, dest); err != nil {
		failJob(fmt.Errorf("move clean epub: %w", err))
		return
	}
	id, err := im.Cat.Index(ctx, dest, sourceFor(hostPath))
	if err != nil {
		failJob(fmt.Errorf("index: %w", err))
		return
	}

	// Disposition of the dropped original. In the DRM path the clean epub is a
	// separate file, so the original (.acsm or encrypted .epub) is archived to
	// done/. In the direct-import path the original WAS the clean file and has
	// already been moved into the library, so there's nothing left to archive.
	if cleanHostPath != hostPath {
		im.archive(hostPath, "done")
	}
	fmt.Printf("import: done %s\n", name)

	slug := ""
	if b, gerr := im.Cat.Get(ctx, id); gerr == nil {
		slug = b.Slug()
	}
	jobs.Finish(jobID, StateDone, "", slug)
}

// pipeline turns a dropped file into a clean, DRM-free epub and returns its
// HOST path. Three cases:
//
//	.acsm            -> fulfill (Adobe) -> ADEPT epub -> decrypt -> clean epub
//	.epub (ADEPT)    -> decrypt -> clean epub
//	.epub (no DRM)   -> used as-is (direct import; the sidecar is never touched)
//
// Sidecar outputs land in the shared work dir, so we resolve the sidecar's
// returned path to the host view by basename. The no-DRM case returns the
// original import path unchanged.
// progressFunc reports a pipeline step transition (and an optional 0..1 fraction
// + detail) to the import-job tracker. nil-safe via the noopProgress default.
type progressFunc func(step string, frac float64, detail string)

func (im *Importer) pipeline(ctx context.Context, hostPath string, onProgress progressFunc) (string, error) {
	if onProgress == nil {
		onProgress = func(string, float64, string) {}
	}
	// Comics carry no DRM and are not epubs: skip the sidecar and the epub
	// inspection entirely (epub.IsADEPTEncrypted would error on a CBZ).
	if isComic(hostPath) {
		// A CBZ imports as-is. A CBR is converted to a CBZ in the work dir first,
		// so the rest of the system only ever sees ZIPs; the converted file flows
		// through the shared tail exactly like a native CBZ.
		if isCBR(hostPath) {
			return im.convertCBR(hostPath, onProgress)
		}
		return hostPath, nil
	}

	isACSM := strings.EqualFold(filepath.Ext(hostPath), ".acsm")

	// Fast path: a plain DRM-free epub needs no fulfillment or decryption.
	if !isACSM {
		encrypted, err := epub.IsADEPTEncrypted(hostPath)
		if err != nil {
			return "", fmt.Errorf("inspect epub: %w", err)
		}
		if !encrypted {
			fmt.Printf("import: %s has no Adobe DRM, importing directly\n", filepath.Base(hostPath))
			return hostPath, nil
		}
	}

	// DRM path: fulfill (.acsm only) then decrypt via the sidecar.
	in := im.SidecarPath(hostPath)
	if isACSM {
		onProgress("fulfilling", 0, "")
		out, err := im.DRM.Fulfill(ctx, in)
		if err != nil {
			return "", fmt.Errorf("fulfill: %w", err)
		}
		in = out
	}
	onProgress("decrypting", 0, "")
	cleanSidecarPath, err := im.DRM.Decrypt(ctx, in)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	// Sidecar wrote into the shared work dir; map back to the host view.
	return filepath.Join(im.workDir(), filepath.Base(cleanSidecarPath)), nil
}

// convertCBR turns a dropped .cbr into a .cbz in the work dir and returns the
// CBZ path. Conversion reports page-by-page progress into the import job, so the
// /imports bar fills during what can be a multi-minute extract on a large comic.
func (im *Importer) convertCBR(hostPath string, onProgress progressFunc) (string, error) {
	work := im.workDir()
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", fmt.Errorf("create work dir: %w", err)
	}
	base := strings.TrimSuffix(filepath.Base(hostPath), filepath.Ext(hostPath))
	dst := filepath.Join(work, base+".cbz")

	onProgress("converting", 0, "")
	err := comic.ConvertCBR(hostPath, dst, func(done, total int) {
		frac := 0.0
		if total > 0 {
			frac = float64(done) / float64(total)
		}
		onProgress("converting", frac, fmt.Sprintf("page %d/%d", done, total))
	})
	if err != nil {
		return "", fmt.Errorf("convert cbr: %w", err)
	}
	return dst, nil
}

func (im *Importer) fail(hostPath string, cause error) {
	fmt.Fprintf(os.Stderr, "import: FAILED %s: %v\n", filepath.Base(hostPath), cause)
	dst := filepath.Join(im.ImportDir, "failed", filepath.Base(hostPath))
	if err := moveFile(hostPath, dst); err != nil {
		fmt.Fprintf(os.Stderr, "import: could not move %s to failed/: %v\n", filepath.Base(hostPath), err)
		return
	}
	if err := os.WriteFile(dst+".log", []byte(cause.Error()+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "import: could not write %s.log: %v\n", filepath.Base(dst), err)
	}
}

// archive moves a processed original into the given import subdir (done/failed),
// logging on failure. A stuck original would otherwise be reprocessed.
func (im *Importer) archive(hostPath, sub string) {
	dst := filepath.Join(im.ImportDir, sub, filepath.Base(hostPath))
	if err := moveFile(hostPath, dst); err != nil {
		fmt.Fprintf(os.Stderr, "import: could not archive %s to %s/: %v\n", filepath.Base(hostPath), sub, err)
	}
}

// uniquePath returns p, or p with a " (2)", " (3)", … suffix before the
// extension if a file already exists, so two books with the same title don't
// clobber each other in the library.
func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

func importable(name string) bool {
	// Skip hidden/temp files: the upload handler stages files as ".upload-*.cbz"
	// in this same directory before atomically renaming them into place. Those
	// carry an importable extension, so without this guard the watcher (or the
	// sweep) would grab a partial temp file mid-write and fail to parse it.
	if strings.HasPrefix(filepath.Base(name), ".") {
		return false
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".acsm", ".epub", ".cbz", ".cbr":
		return true
	default:
		return false
	}
}

func sourceFor(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".acsm":
		return "acsm"
	case ".cbz", ".cbr":
		return "comic-import"
	default:
		return "epub-import"
	}
}

// isComic reports whether a dropped file is a comic archive (imported without
// the DRM sidecar): a CBZ, or a CBR that will be converted to CBZ at import.
func isComic(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	return ext == ".cbz" || ext == ".cbr"
}

// isCBR reports whether a file is a RAR-based comic needing conversion to CBZ.
func isCBR(p string) bool {
	return strings.EqualFold(filepath.Ext(p), ".cbr")
}

// verify parses the finished file to confirm it is a real, readable book and to
// pull the authors + title used for the on-disk library name. It branches on
// format: epub via epub.Read, comic via comic.Read. A parse failure here means
// the file is corrupt/unsupported and the import fails (lands in failed/).
func verify(path string) (authors []string, title string, err error) {
	if isComic(path) {
		m, e := comic.Read(path)
		if e != nil {
			return nil, "", e
		}
		return m.Authors, m.Title, nil
	}
	m, e := epub.Read(path)
	if e != nil {
		return nil, "", e
	}
	return m.Authors, m.Title, nil
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// Cross-device fallback: copy then remove.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}
