// Package drm orchestrates the Adobe DRM ingest pipeline by driving the Python
// sidecar. The Go side stays pure: it never imports Python, it only sends jobs
// to the sidecar worker over HTTP and moves the resulting files.
//
// Pipeline (see docs/DESIGN.md §4):
//
//	*.acsm  --fulfill-->  encrypted .epub (ADEPT)  --decrypt-->  clean .epub
//	*.epub  ----------------------------------------decrypt-->  clean .epub
//
// "fulfill" contacts Adobe using the activation in /secrets and is TIME
// SENSITIVE for library loans (the .acsm carries an <expiration>). "decrypt"
// uses the account .der key, also in /secrets.
package drm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Client talks to the sidecar worker.
type Client struct {
	BaseURL string // e.g. http://ebook-sidecar:7000
	HTTP    *http.Client
}

// New returns a Client with a sane timeout. Fulfillment can be slow (it
// downloads the book from the distributor), so the timeout is generous.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 5 * time.Minute},
	}
}

type jobRequest struct {
	Op    string `json:"op"`    // "fulfill" | "decrypt"
	Input string `json:"input"` // sidecar-visible path under the shared work dir
}

type jobResponse struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"` // sidecar-visible path of the result
	Format string `json:"format"` // "epub" | "pdf"
	Error  string `json:"error"`
	Log    string `json:"log"`
}

// Fulfill turns an .acsm into an encrypted .epub. inputPath must be a path the
// sidecar can see (under the shared work volume). Returns the sidecar-visible
// output path.
func (c *Client) Fulfill(ctx context.Context, inputPath string) (string, error) {
	resp, err := c.do(ctx, jobRequest{Op: "fulfill", Input: inputPath})
	if err != nil {
		return "", err
	}
	if resp.Format != "epub" {
		return "", fmt.Errorf("fulfilled a %q, only epub is supported in v1 (log: %s)", resp.Format, resp.Log)
	}
	return resp.Output, nil
}

// Decrypt strips ADEPT DRM from an .epub using the account key. Returns the
// sidecar-visible path of the clean epub.
func (c *Client) Decrypt(ctx context.Context, inputPath string) (string, error) {
	resp, err := c.do(ctx, jobRequest{Op: "decrypt", Input: inputPath})
	if err != nil {
		return "", err
	}
	return resp.Output, nil
}

type healthResponse struct {
	OK         bool `json:"ok"`
	Activation bool `json:"activation"`
	Key        bool `json:"key"`
}

// Health pings the sidecar; used at startup to surface a misconfigured pipeline
// early rather than at first import.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.health(ctx)
	return err
}

// Configured reports whether the sidecar has a complete Adobe setup (activation
// + key). False (with nil error) means first-run setup is still needed; the web
// UI uses this to decide whether to show the setup form. A transport error means
// the sidecar is unreachable, distinct from "reachable but unconfigured".
func (c *Client) Configured(ctx context.Context) (bool, error) {
	h, err := c.health(ctx)
	if err != nil {
		return false, err
	}
	return h.Activation && h.Key, nil
}

func (c *Client) health(ctx context.Context) (*healthResponse, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/health", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	// /health returns 200 when fully configured, 503 when not; both carry the
	// activation/key flags in the body, so decode regardless of status.
	var h healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, fmt.Errorf("decode sidecar health: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return nil, fmt.Errorf("sidecar health: status %d", resp.StatusCode)
	}
	return &h, nil
}

// Setup runs one-time Adobe authorization on the sidecar (the web first-run
// form calls this). Returns an error if already configured or if Adobe rejects
// the credentials.
func (c *Client) Setup(ctx context.Context, mail, password string, adeVersion int) error {
	body, _ := json.Marshal(map[string]any{
		"mail": mail, "password": password, "ade_version": adeVersion,
	})
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
		// user-facing "setup failed: " context, so wrapping here would double it.
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
