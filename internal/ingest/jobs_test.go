package ingest

import (
	"testing"
	"time"
)

func TestJobLifecycle(t *testing.T) {
	j := newJobs()
	id := j.Start("book.epub", "epub")

	snap := j.Snapshot()
	if len(snap) != 1 || snap[0].State != StateQueued {
		t.Fatalf("after Start: want 1 queued job, got %+v", snap)
	}
	if snap[0].Name != "book.epub" || snap[0].Format != "epub" {
		t.Errorf("job fields = %+v, want name=book.epub format=epub", snap[0])
	}

	// Update flips queued -> running and records the step.
	j.Update(id, func(jb *Job) { jb.Step = "decrypting" })
	if got := j.Snapshot()[0]; got.State != StateRunning || got.Step != "decrypting" {
		t.Errorf("after Update: state=%s step=%q, want running/decrypting", got.State, got.Step)
	}

	// Finish -> terminal, stamps EndedAt + slug, clears step.
	j.Finish(id, StateDone, "", "abc123def456")
	got := j.Snapshot()[0]
	if got.State != StateDone || got.BookSlug != "abc123def456" {
		t.Errorf("after Finish: state=%s slug=%q, want done/abc123def456", got.State, got.BookSlug)
	}
	if got.Step != "" || got.Progress != 1 || got.EndedAt.IsZero() {
		t.Errorf("done job not finalized cleanly: %+v", got)
	}
}

func TestJobFinishFailedAndSkipped(t *testing.T) {
	j := newJobs()
	fid := j.Start("bad.acsm", "acsm")
	// A failure can arrive mid-progress; the terminal frame must zero it so no
	// consumer sees a stale "60% done" on a failed job.
	j.Update(fid, func(jb *Job) { jb.Progress = 0.6 })
	j.Finish(fid, StateFailed, "fulfill: expired", "")
	sid := j.Start("dup.epub", "epub")
	j.Finish(sid, StateSkipped, "already in the library", "")

	snap := j.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("want 2 jobs, got %d", len(snap))
	}
	if snap[0].State != StateFailed || snap[0].Err != "fulfill: expired" {
		t.Errorf("failed job = %+v", snap[0])
	}
	if snap[0].Progress != 0 {
		t.Errorf("failed job kept stale progress %v, want 0", snap[0].Progress)
	}
	if snap[1].State != StateSkipped {
		t.Errorf("skipped job = %+v", snap[1])
	}
}

func TestUpdateUnknownIDIsNoop(t *testing.T) {
	j := newJobs()
	j.Update("nope", func(jb *Job) { jb.Step = "x" }) // must not panic
	j.Finish("nope", StateDone, "", "")               // must not panic
	if len(j.Snapshot()) != 0 {
		t.Error("operations on unknown id should not create jobs")
	}
}

func TestSubscribeReceivesUpdates(t *testing.T) {
	j := newJobs()
	ch, unsub := j.Subscribe()
	defer unsub()

	id := j.Start("book.epub", "epub")
	if got := <-ch; got.ID != id || got.State != StateQueued {
		t.Fatalf("first event = %+v, want queued %s", got, id)
	}
	j.Update(id, func(jb *Job) { jb.Step = "indexing" })
	if got := <-ch; got.Step != "indexing" {
		t.Errorf("update event step = %q, want indexing", got.Step)
	}
	j.Finish(id, StateDone, "", "slug")
	if got := <-ch; got.State != StateDone {
		t.Errorf("finish event state = %s, want done", got.State)
	}
}

func TestSubscribeBroadcastsACopy(t *testing.T) {
	// A subscriber must not be able to mutate the registry's job via the event.
	j := newJobs()
	ch, unsub := j.Subscribe()
	defer unsub()
	id := j.Start("book.epub", "epub")
	got := <-ch
	got.Step = "tampered"
	if j.Snapshot()[0].Step == "tampered" {
		t.Error("broadcast leaked the live job pointer; mutation bled through")
	}
	_ = id
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	j := newJobs()
	ch, unsub := j.Subscribe()
	unsub()
	// Channel is closed; a receive should be the zero value, not block.
	if _, ok := <-ch; ok {
		t.Error("channel should be closed after unsubscribe")
	}
	// Further broadcasts must not panic (send on closed chan) for this sub.
	j.Start("book.epub", "epub")
}

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	// Fill a subscriber's buffer, then keep producing; Start must not block.
	j := newJobs()
	_, unsub := j.Subscribe()
	defer unsub()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ { // far more than the 64 buffer
			j.Start("b.epub", "epub")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start blocked on a slow subscriber")
	}
}

// TestProgressStreamConvergesOnLatest models the CBR case: a long conversion
// emits many fractional progress frames in a tight loop. A subscriber that
// drains slowly must end up seeing the LATEST fraction (the stream coalesces to
// newest on a full buffer), not stall on a stale early one.
func TestProgressStreamConvergesOnLatest(t *testing.T) {
	j := newJobs()
	ch, unsub := j.Subscribe()
	defer unsub()

	id := j.Start("big.cbr", "cbr")
	const frames = 500 // far exceeds the 64 buffer, forcing coalescing
	for i := 1; i <= frames; i++ {
		frac := float64(i) / float64(frames)
		j.Update(id, func(jb *Job) {
			jb.Step = "converting"
			jb.Progress = frac
			jb.Detail = "page"
		})
	}

	// Drain everything currently buffered; the last frame read must be the most
	// recent state we produced (Progress == 1.0), proving no permanent stall.
	var last *Job
	draining := true
	for draining {
		select {
		case jb := <-ch:
			last = jb
		default:
			draining = false
		}
	}
	if last == nil {
		t.Fatal("subscriber received nothing")
	}
	if last.Progress != 1.0 {
		t.Errorf("latest delivered progress = %v, want 1.0 (stream did not converge)", last.Progress)
	}
}

// TestTerminalFrameSurvivesFullBuffer guarantees the invariant the UI depends
// on: even when a slow subscriber's buffer is saturated with progress frames,
// the terminal (done/failed/skipped) frame is never dropped, so a row can never
// be left stuck mid-progress.
func TestTerminalFrameSurvivesFullBuffer(t *testing.T) {
	j := newJobs()
	ch, unsub := j.Subscribe()
	defer unsub()

	id := j.Start("big.cbr", "cbr")
	for i := 0; i < 300; i++ { // saturate the buffer with intermediate frames
		j.Update(id, func(jb *Job) { jb.Step = "converting"; jb.Progress = 0.5 })
	}
	j.Finish(id, StateDone, "", "abc123")

	// The terminal frame must be somewhere in the delivered stream.
	sawTerminal := false
	draining := true
	for draining {
		select {
		case jb := <-ch:
			if jb.State == StateDone && jb.BookSlug == "abc123" {
				sawTerminal = true
			}
		default:
			draining = false
		}
	}
	if !sawTerminal {
		t.Error("terminal frame was dropped under a saturated buffer")
	}
}

func TestRetentionPrunesFinishedByCount(t *testing.T) {
	j := newJobs()
	// Create and finish more than maxFinishedJobs; oldest finished get pruned.
	for i := 0; i < maxFinishedJobs+10; i++ {
		id := j.Start("b.epub", "epub")
		j.Finish(id, StateDone, "", "")
	}
	if got := len(j.Snapshot()); got > maxFinishedJobs {
		t.Errorf("retained %d finished jobs, want <= %d", got, maxFinishedJobs)
	}
}

func TestRetentionKeepsActiveJobs(t *testing.T) {
	j := newJobs()
	active := j.Start("running.epub", "epub")
	for i := 0; i < maxFinishedJobs+5; i++ {
		id := j.Start("b.epub", "epub")
		j.Finish(id, StateDone, "", "")
	}
	// The still-running job must survive pruning regardless of finished churn.
	found := false
	for _, jb := range j.Snapshot() {
		if jb.ID == active {
			found = true
		}
	}
	if !found {
		t.Error("an active job was pruned")
	}
}

func TestRetentionPrunesByTTL(t *testing.T) {
	j := newJobs()
	now := time.Unix(1_000_000, 0)
	j.now = func() time.Time { return now }

	old := j.Start("old.epub", "epub")
	j.Finish(old, StateDone, "", "")

	// Advance past the TTL; a new operation triggers prune.
	now = now.Add(finishedTTL + time.Minute)
	fresh := j.Start("fresh.epub", "epub")

	for _, jb := range j.Snapshot() {
		if jb.ID == old {
			t.Error("a job older than finishedTTL was not pruned")
		}
	}
	if len(j.Snapshot()) == 0 {
		t.Error("the fresh job should remain")
	}
	_ = fresh
}
