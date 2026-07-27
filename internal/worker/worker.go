package worker

import (
	"context"
	"log"
	"time"

	"weaver/internal/store"
)

const (
	// How long to wait after an empty poll before asking again. Short enough to
	// feel responsive, long enough not to hammer the database when idle.
	defaultPollInterval = 500 * time.Millisecond

	// How far in the future a fresh lease expires. Heartbeats (Phase 6) push
	// this out while a handler runs; if the worker dies they stop and the reaper
	// reclaims the task once this passes.
	defaultLeaseTTL = 30 * time.Second
)

// Worker is one polling loop: claim a task, run it, repeat. Many workers share
// one database and one queue table, kept from colliding by the claim query, not
// by anything in this struct.
type Worker struct {
	ID           string
	PollInterval time.Duration
	LeaseTTL     time.Duration

	store    *store.Store
	registry *Registry
}

// New builds a worker with sensible defaults. ID should be unique per running
// worker so leases are attributable to it.
func New(id string, s *store.Store, reg *Registry) *Worker {
	return &Worker{
		ID:           id,
		PollInterval: defaultPollInterval,
		LeaseTTL:     defaultLeaseTTL,
		store:        s,
		registry:     reg,
	}
}

// Run polls until ctx is cancelled. Each iteration claims at most one task and
// runs it to completion before polling again, so a single worker is
// single-threaded; concurrency comes from running several workers.
func (w *Worker) Run(ctx context.Context) error {
	log.Printf("worker %s: started (poll=%s lease=%s)", w.ID, w.PollInterval, w.LeaseTTL)
	for {
		if err := ctx.Err(); err != nil {
			log.Printf("worker %s: stopping (%v)", w.ID, err)
			return err
		}

		task, err := w.store.ClaimTask(ctx, w.ID, w.LeaseTTL)
		if err != nil {
			// A transient database error: log it and back off rather than
			// spinning tight on a failing query.
			log.Printf("worker %s: claim failed: %v", w.ID, err)
			w.wait(ctx, w.PollInterval)
			continue
		}
		if task == nil {
			// Nothing runnable right now. Sleep, then poll again.
			w.wait(ctx, w.PollInterval)
			continue
		}

		w.execute(ctx, task)
	}
}

// execute runs a claimed task's handler. Recording the result -- marking the
// task Succeeded or Failed, deleting the lease, and unblocking downstream tasks
// -- is Phase 5. For now a finished task simply stays Running, which is enough
// to prove claims never overlap: a Running row is not Ready, so no other worker
// can take it.
func (w *Worker) execute(ctx context.Context, task *store.ClaimedTask) {
	log.Printf("worker %s: claimed task %s (%s) attempt %d/%d",
		w.ID, task.Name, task.Handler, task.Attempt, task.MaxAttempts)

	handler, ok := w.registry.Lookup(task.Handler)
	if !ok {
		log.Printf("worker %s: no handler registered for %q; leaving task %s running (completion is Phase 5)",
			w.ID, task.Handler, task.Name)
		return
	}

	if err := handler(ctx, *task); err != nil {
		log.Printf("worker %s: task %s handler returned error: %v", w.ID, task.Name, err)
		return
	}
	log.Printf("worker %s: task %s handler finished ok", w.ID, task.Name)
}

// wait sleeps for d but returns early if ctx is cancelled, so a shutdown does
// not have to wait out a poll interval.
func (w *Worker) wait(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
