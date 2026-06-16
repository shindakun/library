// Command library is the ebook library server: browser reader + OPDS feed for
// the Xteink X4, with an import watcher that drives the DRM sidecar.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/steve/library/internal/catalog"
	"github.com/steve/library/internal/drm"
	"github.com/steve/library/internal/ingest"
	"github.com/steve/library/internal/opds"
	"github.com/steve/library/internal/web"
)

func main() {
	var (
		addr       = flag.String("addr", env("LIBRARY_ADDR", ":8080"), "listen address")
		dataDir    = flag.String("data", env("LIBRARY_DATA", "./data"), "data directory (books, import, catalog.db)")
		baseURL    = flag.String("base-url", env("LIBRARY_BASE_URL", "http://localhost:8080"), "absolute base URL the X4 uses to reach this server")
		sidecarURL = flag.String("sidecar", env("DRM_SIDECAR_URL", "http://localhost:7000"), "DRM sidecar worker URL")
		scanOnBoot = flag.Bool("scan", true, "scan the books dir for new files on startup")
		reorganize = flag.Bool("reorganize", false, "move existing books into Author/Title.epub layout on startup, then continue")
	)
	flag.Parse()

	libraryDir := filepath.Join(*dataDir, "library")
	importDir := filepath.Join(*dataDir, "import")
	dbPath := filepath.Join(*dataDir, "catalog.db")
	for _, d := range []string{libraryDir, importDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("mkdir %s: %v", d, err)
		}
	}

	cat, err := catalog.Open(dbPath, libraryDir)
	if err != nil {
		log.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *scanOnBoot {
		n, err := cat.Scan(ctx)
		if err != nil {
			log.Printf("startup scan: %v", err)
		} else {
			log.Printf("startup scan: indexed %d book(s)", n)
		}
	}

	if *reorganize {
		n, err := cat.Reorganize(ctx)
		if err != nil {
			log.Printf("reorganize: %v", err)
		} else {
			log.Printf("reorganize: moved %d book(s) into Author/Title.epub layout", n)
		}
	}

	mux := http.NewServeMux()

	websrv, err := web.New(cat, importDir)
	if err != nil {
		log.Fatalf("web: %v", err)
	}
	websrv.Register(mux)

	(&opds.Handler{Cat: cat, BaseURL: *baseURL}).Register(mux)

	// DRM import watcher. The sidecar may not be up; that's fine, imports just
	// fail until it is. We probe it once for a friendly startup log.
	drmClient := drm.New(*sidecarURL)
	go func() {
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := drmClient.Health(hctx); err != nil {
			log.Printf("drm sidecar not ready (%v) — imports will fail until it is", err)
		} else {
			log.Printf("drm sidecar healthy at %s", *sidecarURL)
		}
	}()
	importer := &ingest.Importer{
		Cat:       cat,
		DRM:       drmClient,
		ImportDir: importDir,
		LibraryDir:  libraryDir,
		// The sidecar sees the shared volume at SIDECAR_DATA (default /data); the
		// Go service sees it at *dataDir. In compose both are /data, so this is a
		// no-op. When the Go service runs on the HOST (dev) against a containerized
		// worker, the host path must be rewritten to the container path.
		SidecarPath: pathMapper(*dataDir, env("SIDECAR_DATA", "/data")),
	}
	go func() {
		if err := importer.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("importer stopped: %v", err)
		}
	}()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	log.Printf("library listening on %s (base URL %s)", *addr, *baseURL)
	log.Printf("  browser:  %s/", *baseURL)
	log.Printf("  OPDS:     %s/opds  <- point the X4 here", *baseURL)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}

// pathMapper rewrites a host path under hostRoot to the equivalent path under
// sidecarRoot, so the Go service and the containerized worker name the same file
// identically. When the two roots are equal (the compose case) it is a no-op.
func pathMapper(hostRoot, sidecarRoot string) func(string) string {
	hostAbs, err := filepath.Abs(hostRoot)
	if err != nil {
		hostAbs = hostRoot
	}
	return func(p string) string {
		pAbs, err := filepath.Abs(p)
		if err != nil {
			pAbs = p
		}
		rel, err := filepath.Rel(hostAbs, pAbs)
		if err != nil || strings.HasPrefix(rel, "..") {
			return p // not under the data root; pass through unchanged
		}
		return filepath.Join(sidecarRoot, rel)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
