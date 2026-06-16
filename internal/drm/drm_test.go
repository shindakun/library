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
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req jobRequest
		json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		handler(req.Op, w)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL)
	return c
}

func TestFulfillSuccess(t *testing.T) {
	c := mockSidecar(t, func(op string, w http.ResponseWriter) {
		json.NewEncoder(w).Encode(jobResponse{OK: true, Output: "/work/book.epub", Format: "epub"})
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
		json.NewEncoder(w).Encode(jobResponse{OK: true, Output: "/work/book.pdf", Format: "pdf"})
	})
	if _, err := c.Fulfill(context.Background(), "/work/x.acsm"); err == nil {
		t.Error("expected error for non-epub fulfillment")
	}
}

func TestFulfillPropagatesSidecarError(t *testing.T) {
	// Sidecar reports failure (e.g. expired .acsm); the client must surface it.
	c := mockSidecar(t, func(op string, w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(jobResponse{OK: false, Error: "E_ADEPT_REQUEST_EXPIRED"})
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
		json.NewEncoder(w).Encode(jobResponse{OK: true, Output: "/work/clean.epub", Format: "epub"})
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
