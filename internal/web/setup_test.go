package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/steve/library/internal/audible"
)

// mockAudible returns an audible.Client pointed at a stub sidecar. health is the
// /health body (controls Configured); onSetup, if non-nil, receives the parsed
// /setup form and returns the response body.
func mockAudible(t *testing.T, health string, onSetup func(req map[string]any) (status int, body string)) *audible.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(health))
		case "/setup":
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			status, resp := 200, `{"ok":true,"activation":true}`
			if onSetup != nil {
				status, resp = onSetup(req)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(resp))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	t.Cleanup(srv.Close)
	return audible.New(srv.URL)
}

func TestSetupStateAudiobookUnconfigured(t *testing.T) {
	s, _ := newTestServer(t)
	// Reachable but no activation bytes -> the audiobook section should show.
	s.Audible = mockAudible(t, `{"ok":false,"activation":false}`, nil)
	st := s.setupState(context.Background())
	if !st.Audiobook {
		t.Error("Audiobook setup should be needed when the sidecar is unconfigured")
	}
	if st.Ebook {
		t.Error("Ebook setup should be false when no ebook sidecar is set")
	}
	if !st.Any() {
		t.Error("Any() should be true")
	}
}

func TestSetupStateAudiobookConfigured(t *testing.T) {
	s, _ := newTestServer(t)
	s.Audible = mockAudible(t, `{"ok":true,"activation":true}`, nil)
	if st := s.setupState(context.Background()); st.Audiobook {
		t.Error("a configured audiobook sidecar should not need setup")
	}
}

func TestSetupStateUnreachableHidden(t *testing.T) {
	s, _ := newTestServer(t)
	s.Audible = audible.New("http://127.0.0.1:0") // dead address
	if st := s.setupState(context.Background()); st.Audiobook {
		t.Error("an unreachable sidecar must not show a setup section (don't block the library)")
	}
}

func TestApiSetupAudiobookBytes(t *testing.T) {
	s, _ := newTestServer(t)
	var gotBytes string
	s.Audible = mockAudible(t, `{"ok":false,"activation":false}`, func(req map[string]any) (int, string) {
		gotBytes, _ = req["bytes"].(string)
		return 200, `{"ok":true,"activation":true}`
	})

	form := url.Values{"mode": {"bytes"}, "bytes": {"1a2b3c4d"}}
	req := httptest.NewRequest(http.MethodPost, "/api/setup/audiobook", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	s.apiSetupAudiobook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if gotBytes != "1a2b3c4d" {
		t.Errorf("sidecar received bytes %q, want 1a2b3c4d", gotBytes)
	}
}

func TestApiSetupAudiobookLogin(t *testing.T) {
	s, _ := newTestServer(t)
	var gotMode, gotMail string
	s.Audible = mockAudible(t, `{"ok":false}`, func(req map[string]any) (int, string) {
		gotMail, _ = req["mail"].(string)
		if _, ok := req["mail"]; ok {
			gotMode = "login"
		}
		// Login can fail (e.g. Amazon demands a CAPTCHA/2FA); the handler must
		// propagate the sidecar's error as a 502.
		return 500, `{"ok":false,"error":"Audible login failed: bad credentials"}`
	})

	form := url.Values{"mode": {"login"}, "mail": {"a@b.c"}, "password": {"pw"}}
	req := httptest.NewRequest(http.MethodPost, "/api/setup/audiobook", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.apiSetupAudiobook(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (sidecar error propagated)", w.Code)
	}
	if gotMode != "login" || gotMail != "a@b.c" {
		t.Errorf("sidecar got mode=%q mail=%q, want login a@b.c", gotMode, gotMail)
	}
}

func TestApiSetupAudiobookNoClient(t *testing.T) {
	s, _ := newTestServer(t) // Audible is nil
	req := httptest.NewRequest(http.MethodPost, "/api/setup/audiobook", strings.NewReader("mode=bytes&bytes=1a2b3c4d"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.apiSetupAudiobook(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when no audiobook sidecar", w.Code)
	}
}

func TestApiSetupAudiobookMissingBytes(t *testing.T) {
	s, _ := newTestServer(t)
	s.Audible = mockAudible(t, `{"ok":false}`, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/setup/audiobook", strings.NewReader("mode=bytes&bytes="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.apiSetupAudiobook(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for empty bytes", w.Code)
	}
}

// TestIndexRendersAudiobookSetup confirms the index template actually renders the
// audiobook setup card (with the paste/login tabs) when the audiobook sidecar is
// unconfigured, and the library below it. This exercises the real template, not
// just the handler logic.
func TestIndexRendersAudiobookSetup(t *testing.T) {
	s, _ := newTestServer(t)
	s.Audible = mockAudible(t, `{"ok":false,"activation":false}`, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	s.index(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("index status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Audiobook DRM",                 // the section heading
		`action="/api/setup/audiobook"`, // the form target
		`audMode('bytes')`,              // the paste tab
		`audMode('login')`,              // the login tab
		"First-run setup",               // the banner wrapper
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index HTML missing %q", want)
		}
	}
}
