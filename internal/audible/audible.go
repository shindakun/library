// Package audible orchestrates Audible audiobook DRM removal by driving the
// audiobook sidecar. Like internal/drm, the Go side stays pure: it never runs
// ffmpeg or a browser itself, it only sends jobs to the sidecar over HTTP and
// moves the resulting files.
//
// Pipeline (see docs/proposals/AUDIOBOOK_SUPPORT.md):
//
//	*.aax  --decrypt-->  clean .m4b   (ffmpeg -activation_bytes, lossless copy)
//
// "decrypt" decodes the AAX-encrypted audio with the account's activation bytes
// (stored in /secrets by setup) and copies the audio + chapters through. It is
// a long, I/O-bound job (a multi-hundred-MB book is minutes), so the timeout is
// generous and progress is polled from a sibling file (see Progress).
package audible

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Client talks to the audiobook sidecar worker. Its method shapes mirror
// drm.Client (Health/Configured/Decrypt/Setup) so the importer and web layer
// treat the two sidecars symmetrically, but the setup payload and output differ.
type Client struct {
	BaseURL string // e.g. http://audiobook-sidecar:7100
	HTTP    *http.Client
}

// New returns a Client with a generous timeout: an audiobook decrypt is a
// stream copy (not a re-encode) but a multi-hundred-MB book on slow storage can
// still take several minutes.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Minute},
	}
}

type jobRequest struct {
	Op    string `json:"op"`    // "decrypt"
	Input string `json:"input"` // sidecar-visible path under the shared work dir
}

type jobResponse struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"` // sidecar-visible path of the clean .m4b
	Format string `json:"format"` // "m4b"
	Error  string `json:"error"`
	Log    string `json:"log"`
}

// Decrypt removes Audible DRM from a .aax, returning the sidecar-visible path of
// the clean .m4b. inputPath must be a path the sidecar can see (under the shared
// work volume). The call is synchronous (returns when ffmpeg finishes);
// callers wanting live progress poll Progress(outputPath) during the call.
func (c *Client) Decrypt(ctx context.Context, inputPath string) (string, error) {
	resp, err := c.do(ctx, jobRequest{Op: "decrypt", Input: inputPath})
	if err != nil {
		return "", err
	}
	return resp.Output, nil
}

// Progress reads the fraction (0..1) the sidecar has written to the sibling
// "<output>.progress" file for an in-flight decrypt, or (0, false) if there is
// no readable progress yet. progressPath is "<output>.progress" on a path the
// caller can see (the host view of the shared work dir).
func Progress(progressPath string) (float64, bool) {
	data, err := os.ReadFile(progressPath)
	if err != nil {
		return 0, false
	}
	var p struct {
		Progress float64 `json:"progress"`
		Detail   string  `json:"detail"`
	}
	if json.Unmarshal(data, &p) != nil {
		return 0, false
	}
	return p.Progress, true
}

type healthResponse struct {
	OK         bool `json:"ok"`
	Activation bool `json:"activation"`
}

// Health pings the sidecar; used at startup to surface a misconfigured pipeline
// early rather than at first import.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.health(ctx)
	return err
}

// Configured reports whether the sidecar has activation bytes stored. False
// (with nil error) means first-run setup is still needed; the web UI uses this
// to decide whether to show the audiobook setup form. A transport error means
// the sidecar is unreachable, distinct from "reachable but unconfigured".
func (c *Client) Configured(ctx context.Context) (bool, error) {
	h, err := c.health(ctx)
	if err != nil {
		return false, err
	}
	return h.Activation, nil
}

func (c *Client) health(ctx context.Context) (*healthResponse, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/health", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	// /health returns 200 when configured, 503 when not; both carry the
	// activation flag in the body, so decode regardless of status.
	var h healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, fmt.Errorf("decode sidecar health: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return nil, fmt.Errorf("sidecar health: status %d", resp.StatusCode)
	}
	return &h, nil
}

// SetupBytes stores user-pasted activation bytes (8 hex chars) on the sidecar.
// This is the reliable primary setup path. Returns an error if already
// configured or if the value is rejected.
func (c *Client) SetupBytes(ctx context.Context, activationBytes string) error {
	return c.setup(ctx, map[string]any{"bytes": activationBytes})
}

// SetupLogin asks the sidecar to retrieve activation bytes via an Audible login
// (Selenium). This is the best-effort secondary path and may be unavailable in a
// given sidecar build (it returns a clear error then); callers should fall back
// to SetupBytes.
func (c *Client) SetupLogin(ctx context.Context, mail, password string) error {
	return c.setup(ctx, map[string]any{"mail": mail, "password": password})
}

func (c *Client) setup(ctx context.Context, payload map[string]any) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/setup", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("sidecar unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("decode setup response: %w", err)
	}
	if !r.OK {
		// Return the sidecar's message verbatim; the web/CLI caller adds the
		// user-facing "audiobook setup failed: " context, so wrapping it here
		// would double the prefix.
		return errors.New(r.Error)
	}
	return nil
}

func (c *Client) do(ctx context.Context, jr jobRequest) (*jobResponse, error) {
	body, _ := json.Marshal(jr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/job", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sidecar unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var jres jobResponse
	if err := json.NewDecoder(resp.Body).Decode(&jres); err != nil {
		return nil, fmt.Errorf("decode sidecar response: %w", err)
	}
	if !jres.OK {
		return nil, fmt.Errorf("sidecar %s failed: %s", jr.Op, jres.Error)
	}
	return &jres, nil
}
