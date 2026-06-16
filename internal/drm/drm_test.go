package drm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockSidecar serves canned /job and /health responses and records the last
// job request body.
func mockSidecar(t *testing.T, handler func(op string, w http.ResponseWriter)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			// Mirror the real sidecar: 200 + a JSON body with the flags.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"activation":true,"key":true}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req jobRequest
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		handler(req.Op, w)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL)
	return c
}

func TestFulfillSuccess(t *testing.T) {
	c := mockSidecar(t, func(op string, w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(jobResponse{OK: true, Output: "/work/book.epub", Format: "epub"})
	})
	out, err := c.Fulfill(context.Background(), "/work/x.acsm")
	if err != nil {
		t.Fatal(err)
	}
	if out != "/work/book.epub" {
		t.Errorf("output = %q, want /work/book.epub", out)
	}
}

func TestFulfillRejectsNonEpub(t *testing.T) {
	// A PDF fulfillment must be rejected (v1 supports epub only).
	c := mockSidecar(t, func(op string, w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(jobResponse{OK: true, Output: "/work/book.pdf", Format: "pdf"})
	})
	if _, err := c.Fulfill(context.Background(), "/work/x.acsm"); err == nil {
		t.Error("expected error for non-epub fulfillment")
	}
}

func TestFulfillPropagatesSidecarError(t *testing.T) {
	// Sidecar reports failure (e.g. expired .acsm); the client must surface it.
	c := mockSidecar(t, func(op string, w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(jobResponse{OK: false, Error: "E_ADEPT_REQUEST_EXPIRED"})
	})
	_, err := c.Fulfill(context.Background(), "/work/x.acsm")
	if err == nil {
		t.Fatal("expected error when sidecar reports failure")
	}
	if !contains(err.Error(), "E_ADEPT_REQUEST_EXPIRED") {
		t.Errorf("error %q should include the sidecar message", err)
	}
}

func TestDecryptSuccess(t *testing.T) {
	c := mockSidecar(t, func(op string, w http.ResponseWriter) {
		if op != "decrypt" {
			t.Errorf("expected op=decrypt, got %q", op)
		}
		_ = json.NewEncoder(w).Encode(jobResponse{OK: true, Output: "/work/clean.epub", Format: "epub"})
	})
	out, err := c.Decrypt(context.Background(), "/work/enc.epub")
	if err != nil {
		t.Fatal(err)
	}
	if out != "/work/clean.epub" {
		t.Errorf("output = %q, want /work/clean.epub", out)
	}
}

func TestHealthOK(t *testing.T) {
	c := mockSidecar(t, func(op string, w http.ResponseWriter) {})
	if err := c.Health(context.Background()); err != nil {
		t.Errorf("Health = %v, want nil", err)
	}
}

func TestConfigured(t *testing.T) {
	// Reachable + fully configured (the mock /health reports all true).
	c := mockSidecar(t, func(op string, w http.ResponseWriter) {})
	ok, err := c.Configured(context.Background())
	if err != nil || !ok {
		t.Errorf("Configured = %v (err %v), want true", ok, err)
	}

	// Reachable but NOT configured: 503 + activation/key false.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"ok":false,"activation":false,"key":false}`))
	}))
	defer srv.Close()
	ok, err = New(srv.URL).Configured(context.Background())
	if err != nil {
		t.Errorf("Configured (unconfigured sidecar) errored: %v", err)
	}
	if ok {
		t.Error("Configured = true, want false for an unconfigured sidecar")
	}

	// Unreachable: must be an error, not a false "needs setup".
	if _, err := New("http://127.0.0.1:1").Configured(context.Background()); err == nil {
		t.Error("Configured against a dead sidecar should error")
	}
}

func TestSetup(t *testing.T) {
	var gotMail, gotPass string
	var gotVer float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		gotMail, _ = req["mail"].(string)
		gotPass, _ = req["password"].(string)
		gotVer, _ = req["ade_version"].(float64)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"activation":true,"key":true}`))
	}))
	defer srv.Close()
	if err := New(srv.URL).Setup(context.Background(), "a@b.com", "pw", 1); err != nil {
		t.Fatalf("Setup = %v, want nil", err)
	}
	if gotMail != "a@b.com" || gotPass != "pw" || gotVer != 1 {
		t.Errorf("sidecar got mail=%q pass=%q ver=%v, want a@b.com/pw/1", gotMail, gotPass, gotVer)
	}

	// Sidecar refuses (already configured): client surfaces the error.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"error":"already configured"}`))
	}))
	defer srv2.Close()
	if err := New(srv2.URL).Setup(context.Background(), "a@b.com", "pw", 1); err == nil {
		t.Error("expected error when sidecar refuses setup")
	}
}

func TestUnreachableSidecar(t *testing.T) {
	// Point at a dead address; the client must return an error, not hang/panic.
	c := New("http://127.0.0.1:1")
	if _, err := c.Decrypt(context.Background(), "/work/x.epub"); err == nil {
		t.Error("expected error for unreachable sidecar")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
