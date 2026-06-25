package audible

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// mockSidecar serves canned responses. health controls the /health body;
// jobHandler handles /job; setupHandler handles /setup (receives the parsed
// request map).
func mockSidecar(t *testing.T, health string, jobHandler func(w http.ResponseWriter), setupHandler func(req map[string]any, w http.ResponseWriter)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(health))
		case "/job":
			jobHandler(w)
		case "/setup":
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			setupHandler(req, w)
		default:
			http.Error(w, "not found", 404)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func TestDecryptSuccess(t *testing.T) {
	c := mockSidecar(t, "", func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"ok":true,"output":"/work/book.m4b","format":"m4b"}`))
	}, nil)
	out, err := c.Decrypt(context.Background(), "/work/book.aax")
	if err != nil {
		t.Fatal(err)
	}
	if out != "/work/book.m4b" {
		t.Errorf("output = %q, want /work/book.m4b", out)
	}
}

func TestDecryptPropagatesSidecarError(t *testing.T) {
	c := mockSidecar(t, "", func(w http.ResponseWriter) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"ok":false,"error":"wrong activation bytes"}`))
	}, nil)
	_, err := c.Decrypt(context.Background(), "/work/book.aax")
	if err == nil {
		t.Fatal("expected error")
	}
	if want := "wrong activation bytes"; !contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
}

func TestConfigured(t *testing.T) {
	// 503 + activation:false -> not configured (nil error).
	notReady := mockSidecar(t, `{"ok":false,"activation":false}`, nil, nil)
	if ok, err := notReady.Configured(context.Background()); ok || err != nil {
		t.Errorf("unconfigured: got (%v,%v), want (false,nil)", ok, err)
	}
	// 200 + activation:true -> configured.
	ready := mockSidecar(t, `{"ok":true,"activation":true}`, nil, nil)
	if ok, err := ready.Configured(context.Background()); !ok || err != nil {
		t.Errorf("configured: got (%v,%v), want (true,nil)", ok, err)
	}
}

func TestConfiguredUnreachableIsError(t *testing.T) {
	// A client pointed at a dead address: Configured must return an error, not
	// a misleading "false" (so the UI can distinguish down vs unconfigured).
	c := New("http://127.0.0.1:0")
	if _, err := c.Configured(context.Background()); err == nil {
		t.Error("expected a transport error for an unreachable sidecar")
	}
}

func TestSetupBytes(t *testing.T) {
	var gotBytes string
	c := mockSidecar(t, "", nil, func(req map[string]any, w http.ResponseWriter) {
		if b, ok := req["bytes"].(string); ok {
			gotBytes = b
		}
		_, _ = w.Write([]byte(`{"ok":true,"activation":true}`))
	})
	if err := c.SetupBytes(context.Background(), "1a2b3c4d"); err != nil {
		t.Fatal(err)
	}
	if gotBytes != "1a2b3c4d" {
		t.Errorf("sidecar received bytes %q, want 1a2b3c4d", gotBytes)
	}
}

func TestSetupBytesRejected(t *testing.T) {
	c := mockSidecar(t, "", nil, func(req map[string]any, w http.ResponseWriter) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"ok":false,"error":"activation bytes must be exactly 8 hex characters"}`))
	})
	if err := c.SetupBytes(context.Background(), "nothex"); err == nil {
		t.Error("expected the sidecar's validation error to propagate")
	}
}

func TestSetupLoginSendsCreds(t *testing.T) {
	var gotMail, gotMarket string
	c := mockSidecar(t, "", nil, func(req map[string]any, w http.ResponseWriter) {
		gotMail, _ = req["mail"].(string)
		gotMarket, _ = req["marketplace"].(string)
		// Login can still fail (e.g. CAPTCHA); the client must propagate it.
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"ok":false,"error":"Audible asked for a CAPTCHA"}`))
	})
	_, err := c.SetupLogin(context.Background(), "a@b.c", "pw", "uk")
	if err == nil || !contains(err.Error(), "CAPTCHA") {
		t.Errorf("login error = %v, want it to propagate the sidecar message", err)
	}
	if gotMail != "a@b.c" {
		t.Errorf("sidecar received mail %q, want a@b.c", gotMail)
	}
	if gotMarket != "uk" {
		t.Errorf("sidecar received marketplace %q, want uk", gotMarket)
	}
}

func TestSetupLoginReturnsOTPRequired(t *testing.T) {
	c := mockSidecar(t, "", nil, func(req map[string]any, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"ok":true,"otp_required":true,"login_id":"abc123","message":"Enter the code"}`))
	})
	res, err := c.SetupLogin(context.Background(), "a@b.c", "pw", "us")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OTPRequired || res.LoginID != "abc123" {
		t.Errorf("got %+v, want OTPRequired with login_id abc123", res)
	}
}

func TestSetupLoginOTPCompletes(t *testing.T) {
	var gotID, gotOTP string
	c := mockSidecar(t, "", nil, func(req map[string]any, w http.ResponseWriter) {
		gotID, _ = req["login_id"].(string)
		gotOTP, _ = req["otp"].(string)
		_, _ = w.Write([]byte(`{"ok":true,"activation":true}`))
	})
	res, err := c.SetupLoginOTP(context.Background(), "abc123", "123456")
	if err != nil {
		t.Fatal(err)
	}
	if res.OTPRequired {
		t.Error("completed login should not still require OTP")
	}
	if gotID != "abc123" || gotOTP != "123456" {
		t.Errorf("sidecar got login_id=%q otp=%q, want abc123/123456", gotID, gotOTP)
	}
}

func TestProgressReadsSiblingFile(t *testing.T) {
	dir := t.TempDir()
	pp := filepath.Join(dir, "book.m4b.progress")

	// No file yet -> (0, false).
	if frac, ok := Progress(pp); ok || frac != 0 {
		t.Errorf("no progress file: got (%v,%v), want (0,false)", frac, ok)
	}
	// Mid-convert fraction.
	if err := os.WriteFile(pp, []byte(`{"progress":0.42,"detail":"100/240 s"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if frac, ok := Progress(pp); !ok || frac != 0.42 {
		t.Errorf("progress = (%v,%v), want (0.42,true)", frac, ok)
	}
	// Malformed file -> (0, false), not a panic.
	if err := os.WriteFile(pp, []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if frac, ok := Progress(pp); ok || frac != 0 {
		t.Errorf("malformed progress: got (%v,%v), want (0,false)", frac, ok)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
