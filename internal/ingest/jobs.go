package ingest

import (
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// JobState is the lifecycle state of one import.
type JobState string

const (
	StateQueued  JobState = "queued"  // accepted, waiting for the pipeline lock
	StateRunning JobState = "running" // actively processing a step
	StateDone    JobState = "done"    // imported into the library
	StateFailed  JobState = "failed"  // moved to failed/ with a reason
	StateSkipped JobState = "skipped" // duplicate content, nothing to do
)

func (s JobState) terminal() bool {
	return s == StateDone || s == StateFailed || s == StateSkipped
}

// Job is a tracked import. It is the unit the UI shows. A Job is copied out
// (never the live pointer) when handed to callers, so concurrent updates are
// safe.
type Job struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`   // original filename
	Format    string    `json:"format"` // "acsm" | "epub" | "cbz" | "cbr"
	State     JobState  `json:"state"`
	Step      string    `json:"step,omitempty"`     // human label: "fulfilling", "decrypting", "indexing"
	Progress  float64   `json:"progress,omitempty"` // 0..1 for steps that can report it
	Detail    string    `json:"detail,omitempty"`   // optional extra ("page 142/610")
	Err       string    `json:"error,omitempty"`    // set when State == failed
	BookSlug  string    `json:"bookSlug,omitempty"` // set on success, so the UI can link
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
}

// retention bounds the finished-job history so the registry does not grow
// unbounded. Active jobs are never pruned; finished ones are kept until both
// limits are satisfied (newest kept).
const (
	maxFinishedJobs = 50
	finishedTTL     = 10 * time.Minute
)

// Jobs is the in-memory registry of import jobs plus an SSE fan-out. Jobs do not
// survive a restart (they are transient UI state; the catalog is the durable
// truth). The zero value is not usable; use newJobs.
type Jobs struct {
	mu     sync.Mutex
	jobs   map[string]*Job
	order  []string // insertion order, for stable Snapshot and pruning
	subs   map[chan *Job]struct{}
	nextID atomic.Uint64
	now    func() time.Time // injectable for tests
}

// NewJobs returns an empty import-job registry. The Importer creates its own via
// JobRegistry(); this is exported so the web layer (and its tests) can construct
// one directly.
func NewJobs() *Jobs { return newJobs() }

func newJobs() *Jobs {
	return &Jobs{
		jobs: map[string]*Job{},
		subs: map[chan *Job]struct{}{},
		now:  time.Now,
	}
}

// Start creates a queued job and returns its id. Broadcasts the new job.
func (j *Jobs) Start(name, format string) string {
	id := strconv.FormatUint(j.nextID.Add(1), 10)
	jb := &Job{
		ID:        id,
		Name:      name,
		Format:    format,
		State:     StateQueued,
		StartedAt: j.now(),
	}
	j.mu.Lock()
	j.jobs[id] = jb
	j.order = append(j.order, id)
	j.pruneLocked()
	snap := copyJob(jb)
	j.mu.Unlock()

	j.broadcast(snap)
	return id
}

// Update mutates a job under the lock and broadcasts the result. A no-op if the
// id is unknown (e.g. already pruned). Do not set terminal state here; use
// Finish so EndedAt is stamped consistently.
func (j *Jobs) Update(id string, mutate func(*Job)) {
	j.mu.Lock()
	jb := j.jobs[id]
	if jb == nil {
		j.mu.Unlock()
		return
	}
	mutate(jb)
	if jb.State == StateQueued {
		jb.State = StateRunning
	}
	snap := copyJob(jb)
	j.mu.Unlock()

	j.broadcast(snap)
}

// Finish moves a job to a terminal state, stamps EndedAt, records the error or
// resulting book slug, and broadcasts. Safe to call once per job.
func (j *Jobs) Finish(id string, state JobState, errMsg, slug string) {
	j.mu.Lock()
	jb := j.jobs[id]
	if jb == nil {
		j.mu.Unlock()
		return
	}
	jb.State = state
	jb.EndedAt = j.now()
	jb.Step = ""
	jb.Detail = ""
	if state == StateDone {
		jb.Progress = 1
	}
	jb.Err = errMsg
	jb.BookSlug = slug
	snap := copyJob(jb)
	j.pruneLocked()
	j.mu.Unlock()

	j.broadcast(snap)
}

// Snapshot returns a copy of every tracked job, newest last (insertion order).
func (j *Jobs) Snapshot() []*Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]*Job, 0, len(j.order))
	for _, id := range j.order {
		if jb := j.jobs[id]; jb != nil {
			out = append(out, copyJob(jb))
		}
	}
	return out
}

// Subscribe registers an SSE listener. It returns a receive channel of job
// updates and an unsubscribe func the caller must invoke when done (e.g. on
// client disconnect). The channel is buffered; if a subscriber falls behind,
// updates are dropped for it rather than blocking imports (it can resync via
// Snapshot).
func (j *Jobs) Subscribe() (<-chan *Job, func()) {
	ch := make(chan *Job, 64)
	j.mu.Lock()
	j.subs[ch] = struct{}{}
	j.mu.Unlock()

	unsub := func() {
		j.mu.Lock()
		if _, ok := j.subs[ch]; ok {
			delete(j.subs, ch)
			close(ch)
		}
		j.mu.Unlock()
	}
	return ch, unsub
}

func (j *Jobs) broadcast(jb *Job) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for ch := range j.subs {
		select {
		case ch <- jb:
		default:
			// Slow subscriber: drop this update. It can resync via Snapshot.
		}
	}
}

// pruneLocked removes finished jobs beyond the retention window. Caller holds mu.
func (j *Jobs) pruneLocked() {
	// Count finished jobs; keep the most recent maxFinishedJobs and those within
	// finishedTTL. Active (non-terminal) jobs are always kept.
	cutoff := j.now().Add(-finishedTTL)
	var finished []string
	for _, id := range j.order {
		if jb := j.jobs[id]; jb != nil && jb.State.terminal() {
			finished = append(finished, id)
		}
	}
	// Oldest finished first (order is insertion order).
	excess := len(finished) - maxFinishedJobs
	remove := map[string]bool{}
	for i, id := range finished {
		jb := j.jobs[id]
		if i < excess || jb.EndedAt.Before(cutoff) {
			remove[id] = true
		}
	}
	if len(remove) == 0 {
		return
	}
	newOrder := j.order[:0:0]
	for _, id := range j.order {
		if remove[id] {
			delete(j.jobs, id)
			continue
		}
		newOrder = append(newOrder, id)
	}
	j.order = newOrder
}

func copyJob(jb *Job) *Job {
	c := *jb
	return &c
}
