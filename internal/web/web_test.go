package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steve/library/internal/catalog"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	imp := filepath.Join(dir, "import")
	_ = os.MkdirAll(imp, 0o755)
	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"), filepath.Join(dir, "library"), filepath.Join(dir, "covers"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	s, err := New(cat, imp, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, imp
}

func uploadReq(t *testing.T, filename, content, accept string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte(content))
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return req
}

func TestUploadAcceptsEpub(t *testing.T) {
	s, imp := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, "MyBook.epub", "PK-fake-zip", "application/json"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	// File should have landed in the import dir, named from the upload.
	if _, err := os.Stat(filepath.Join(imp, "MyBook.epub")); err != nil {
		t.Errorf("uploaded file not staged in import dir: %v", err)
	}
	// No leftover temp files.
	entries, _ := os.ReadDir(imp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Errorf("leftover temp file %q (upload not atomic)", e.Name())
		}
	}
}

func TestUploadRejectsBadExtension(t *testing.T) {
	s, imp := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, "malware.exe", "MZ", "application/json"))

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415 for .exe", rec.Code)
	}
	// Nothing should have been written.
	entries, _ := os.ReadDir(imp)
	if len(entries) != 0 {
		t.Errorf("rejected upload still wrote %d file(s)", len(entries))
	}
}

func TestUploadFormRedirects(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	// No JSON Accept header -> browser form path -> 303 redirect.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadReq(t, "Book.acsm", "<fulfillmentToken/>", ""))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 for form post", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/?uploaded=") {
		t.Errorf("redirect Location = %q, want /?uploaded=...", loc)
	}
}

func TestBookNotFound(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/book/999/file", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing book file: status = %d, want 404", rec.Code)
	}
}

func TestScanEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/scan", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("scan status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "indexed") {
		t.Errorf("scan response %q should report indexed count", rec.Body.String())
	}
}
