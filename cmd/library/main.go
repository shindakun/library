// Command library is the ebook library server: browser reader + OPDS feed for
// the Xteink X4, with an import watcher that drives the DRM sidecar.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/steve/library/internal/audible"
	"github.com/steve/library/internal/catalog"
	"github.com/steve/library/internal/drm"
	"github.com/steve/library/internal/ingest"
	"github.com/steve/library/internal/opds"
	"github.com/steve/library/internal/web"
)

// version is the build version, set via -ldflags "-X main.version=v0.1.0" from
// the git tag at release time. "dev" for local untagged builds.
var version = "dev"

func main() {
	var (
		addr    = flag.String("addr", env("LIBRARY_ADDR", ":8080"), "listen address")
		dataDir = flag.String("data", env("LIBRARY_DATA", "./data"), "data directory (books, import, catalog.db)")
		baseURL = flag.String("base-url", env("LIBRARY_BASE_URL", "http://localhost:8080"), "absolute base URL the X4 uses to reach this server")
		// EBOOK_SIDECAR_URL is the ebook DRM sidecar (renamed from DRM_SIDECAR_URL,
		// which is still honored as a fallback for existing deploys). Empty
		// disables ebook DRM (no-sidecar mode).
		sidecarURL = flag.String("sidecar", env("EBOOK_SIDECAR_URL", env("DRM_SIDECAR_URL", "http://localhost:7000")), "ebook DRM sidecar worker URL")
		// AUDIOBOOK_SIDECAR_URL is the audiobook DRM sidecar (.aax -> .m4b via
		// ffmpeg). Empty disables audiobook import (no-sidecar mode), independently
		// of the ebook sidecar.
		audiobookURL = flag.String("audiobook-sidecar", env("AUDIOBOOK_SIDECAR_URL", "http://localhost:7100"), "audiobook DRM sidecar worker URL")
		scanOnBoot   = flag.Bool("scan", true, "scan the books dir for new files on startup")
		reorganize   = flag.Bool("reorganize", false, "move existing books into Author/Title.epub layout on startup, then continue")
		showVersion  = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("library", version)
		return
	}
	log.Printf("library %s starting", version)

	libraryDir := filepath.Join(*dataDir, "library")
	importDir := filepath.Join(*dataDir, "import")
	coversDir := filepath.Join(*dataDir, "covers")
	dbPath := filepath.Join(*dataDir, "catalog.db")
	for _, d := range []string{libraryDir, importDir, coversDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("mkdir %s: %v", d, err)
		}
	}

	cat, err := catalog.Open(dbPath, libraryDir, coversDir)
	if err != nil {
		log.Fatalf("open catalog: %v", err)
	}
	defer func() { _ = cat.Close() }()

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

	// DRM sidecar client: drives fulfill/decrypt, and backs the web first-run
	// setup form. An empty -sidecar URL DISABLES DRM entirely (no sidecar): the
	// client is nil, so the setup form is hidden, no health probe runs, and DRM
	// imports (.acsm / ADEPT .epub) fail clearly. Comics and DRM-free epubs import
	// fine. This is the right default for anyone with no legacy-DRM content.
	var drmClient *drm.Client
	drmEnabled := strings.TrimSpace(*sidecarURL) != ""
	if drmEnabled {
		drmClient = drm.New(*sidecarURL)
	} else {
		log.Printf("DRM sidecar disabled (-sidecar empty): comics and DRM-free epubs import; .acsm / encrypted epub will be rejected")
	}

	// Audiobook sidecar client: drives the .aax -> .m4b decrypt. Independent of
	// the ebook sidecar (the "one / other / both / none" design): an empty
	// -audiobook-sidecar URL disables audiobook import entirely, so .aax files are
	// rejected with a clear reason while everything else imports normally.
	var audibleClient *audible.Client
	audiobookEnabled := strings.TrimSpace(*audiobookURL) != ""
	if audiobookEnabled {
		audibleClient = audible.New(*audiobookURL)
	} else {
		log.Printf("audiobook sidecar disabled (-audiobook-sidecar empty): .aax will be rejected")
	}

	// Importer is created before the web server so its job registry can back the
	// /imports page + SSE stream.
	importer := &ingest.Importer{
		Cat:        cat,
		DRM:        drmClient,
		Audible:    audibleClient,
		ImportDir:  importDir,
		LibraryDir: libraryDir,
		// The sidecar sees the shared volume at SIDECAR_DATA (default /data); the
		// Go service sees it at *dataDir. In compose both are /data, so this is a
		// no-op. When the Go service runs on the HOST (dev) against a containerized
		// worker, the host path must be rewritten to the container path.
		SidecarPath: pathMapper(*dataDir, env("SIDECAR_DATA", "/data")),
	}

	websrv, err := web.New(cat, importDir, drmClient, importer.JobRegistry())
	if err != nil {
		log.Fatalf("web: %v", err)
	}
	websrv.Register(mux)

	(&opds.Handler{Cat: cat, BaseURL: *baseURL}).Register(mux)

	// When DRM is enabled, the sidecar may not be up yet; that's fine. Only DRM
	// imports need it: .acsm (Adobe fulfillment) and ADEPT-encrypted .epub
	// (decryption). Comics (.cbz/.cbr) and DRM-free .epub import without ever
	// touching the sidecar. Probe it once for a friendly startup log.
	if drmEnabled {
		go func() {
			hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if err := drmClient.Health(hctx); err != nil {
				log.Printf("drm sidecar not ready (%v); DRM imports (.acsm / ADEPT .epub) will fail until it is, but comics and DRM-free epubs import fine", err)
			} else {
				log.Printf("drm sidecar healthy at %s", *sidecarURL)
			}
		}()
	}
	if audiobookEnabled {
		go func() {
			hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			// Health() succeeds even before activation bytes are set; Configured()
			// distinguishes "up but needs setup" from "up and ready".
			if err := audibleClient.Health(hctx); err != nil {
				log.Printf("audiobook sidecar not ready (%v); .aax import will fail until it is", err)
			} else if ok, _ := audibleClient.Configured(hctx); !ok {
				log.Printf("audiobook sidecar healthy at %s but needs setup (no activation bytes yet); .aax import will fail until set", *audiobookURL)
			} else {
				log.Printf("audiobook sidecar healthy at %s", *audiobookURL)
			}
		}()
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
		_ = srv.Shutdown(sctx)
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
