package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/steve/library/internal/ingest"
)

func TestApiImportsSnapshot(t *testing.T) {
	s, _ := newTestServer(t)
	jobs := s.Jobs
	id := jobs.Start("book.epub", "epub")
	jobs.Update(id, func(j *ingest.Job) { j.Step = "decrypting" })

	mux := http.NewServeMux()
	s.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/imports", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []*ingest.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 1 || got[0].Name != "book.epub" || got[0].Step != "decrypting" {
		t.Errorf("snapshot = %+v, want one decrypting book.epub job", got)
	}
}

func TestApiImportsSnapshotEmptyWhenNoJobs(t *testing.T) {
	s, _ := newTestServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/imports", nil))
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("empty snapshot body = %q, want []", rec.Body.String())
	}
}

// TestApiImportsStream exercises the SSE endpoint against a real listener: it
// must send the existing snapshot, then a live update, as data: events, and stop
// when the request context is cancelled.
func TestApiImportsStream(t *testing.T) {
	s, _ := newTestServer(t)
	jobs := s.Jobs
	// One job already present before the client connects -> must arrive in the
	// initial snapshot burst.
	pre := jobs.Start("pre.epub", "epub")

	srv := httptest.NewServer(http.HandlerFunc(s.apiImportsStream))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	events := make(chan *ingest.Job, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue // keepalive/comment or blank line
			}
			var j ingest.Job
			if json.Unmarshal([]byte(data), &j) == nil {
				events <- &j
			}
		}
	}()

	// 1. The pre-existing job arrives from the snapshot burst.
	if got := waitEvent(t, events); got.ID != pre || got.Name != "pre.epub" {
		t.Fatalf("first event = %+v, want snapshot of pre.epub (%s)", got, pre)
	}

	// 2. A live update after connect is streamed.
	live := jobs.Start("live.acsm", "acsm")
	jobs.Update(live, func(j *ingest.Job) { j.Step = "fulfilling" })
	// We may see the queued event and/or the updated one; wait until we see the
	// live job by id.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-events:
			if e.ID == live {
				if e.Name != "live.acsm" {
					t.Errorf("live event name = %q", e.Name)
				}
				cancel() // disconnect; the handler must return
				return
			}
		case <-deadline:
			t.Fatal("did not receive the live job event over SSE")
		}
	}
}

func waitEvent(t *testing.T, ch <-chan *ingest.Job) *ingest.Job {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an SSE event")
		return nil
	}
}
