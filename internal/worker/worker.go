package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"weaver/internal/store"
)

const (
	// How long to wait after an empty poll before asking again. Short enough to
	// feel responsive, long enough not to hammer the database when idle.
	defaultPollInterval = 500 * time.Millisecond

	// How far in the future a fresh lease expires. Heartbeats push this out while
	// a handler runs; if the worker dies they stop and the reaper reclaims the
	// task once this passes.
	defaultLeaseTTL = 30 * time.Second

	// How often a running handler renews its lease. Comfortably shorter than the
	// TTL so a task survives a missed beat (a slow query, a brief GC pause)
	// without being reaped out from under a worker that is still alive.
	defaultHeartbeatInterval = defaultLeaseTTL / 3
)

// Worker is one polling loop: claim a task, run it, repeat. Many workers share
// one database and one queue table, kept from colliding by the claim query, not
// by anything in this struct.
type Worker struct {
	ID                string
	PollInterval      time.Duration
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration

	store    *store.Store
	registry *Registry
}

// New builds a worker with sensible defaults. ID should be unique per running
// worker so leases are attributable to it.
func New(id string, s *store.Store, reg *Registry) *Worker {
	return &Worker{
		ID:                id,
		PollInterval:      defaultPollInterval,
		LeaseTTL:          defaultLeaseTTL,
		HeartbeatInterval: defaultHeartbeatInterval,
		store:             s,
		registry:          reg,
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

// execute runs a claimed task's handler and records the outcome: Succeeded on a
// clean return, Failed (retry or Dead) on an error, panic, or timeout. Either
// way the lease is released and any unblocked downstream tasks are promoted --
// all in the store, in one transaction per outcome.
func (w *Worker) execute(ctx context.Context, task *store.ClaimedTask) {
	log.Printf("worker %s: claimed task %s (%s) attempt %d/%d",
		w.ID, task.Name, task.Handler, task.Attempt, task.MaxAttempts)

	handler, ok := w.registry.Lookup(task.Handler)
	if !ok {
		// A task naming a handler this worker does not know is a definition bug.
		// Route it through the failure path so it is visible (and eventually
		// Dead) rather than silently stranding the run.
		w.fail(ctx, task, fmt.Errorf("no handler registered for %q", task.Handler))
		return
	}

	// Enforce the per-task timeout with a child context. Go cannot forcibly stop
	// a running goroutine, so this is cooperative: a well-behaved handler watches
	// ctx and returns; the deadline check below covers one that returns late.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(task.TimeoutSeconds)*time.Second)
	defer cancel()

	// Renew the lease on a timer for as long as the handler runs. This is the
	// signal that keeps the reaper away: stop beating (because this process died)
	// and the lease lapses, letting another worker take over. A live-but-slow
	// task keeps beating and holds its claim.
	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	var hbDone sync.WaitGroup
	hbDone.Add(1)
	go func() {
		defer hbDone.Done()
		w.heartbeat(hbCtx, task)
	}()

	err := runHandler(runCtx, handler, *task)

	// Stop heartbeating before we record the outcome, so the lease is not being
	// renewed underneath CompleteTask/FailTask as they release it.
	stopHeartbeat()
	hbDone.Wait()

	// A cancelled parent means the worker is shutting down, not that the task
	// failed. Leave the row Running and let its lease expire; the reaper (Phase 6)
	// returns it to Ready for another worker. Recording a result here would be a
	// lie about work we abandoned.
	if ctx.Err() != nil {
		log.Printf("worker %s: task %s abandoned on shutdown", w.ID, task.Name)
		return
	}

	// The handler exhausting the deadline is a failure even if it returned nil.
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		w.fail(ctx, task, fmt.Errorf("timed out after %ds", task.TimeoutSeconds))
		return
	}

	if err != nil {
		w.fail(ctx, task, err)
		return
	}
	w.complete(ctx, task)
}

// complete records a successful run, unblocking downstream tasks. ErrLeaseLost is
// expected, not exceptional: the lease expired mid-handler and the task is
// someone else's now, so there is nothing to write.
func (w *Worker) complete(ctx context.Context, task *store.ClaimedTask) {
	switch err := w.store.CompleteTask(ctx, task, w.ID); {
	case err == nil:
		log.Printf("worker %s: task %s succeeded", w.ID, task.Name)
	case errors.Is(err, store.ErrLeaseLost):
		log.Printf("worker %s: task %s lease lost before completion; dropping result", w.ID, task.Name)
	default:
		log.Printf("worker %s: recording task %s success failed: %v", w.ID, task.Name, err)
	}
}

// fail records a failed run: a backoff-delayed retry if attempts remain, else
// Dead. cause is what went wrong, stored on the task for the UI and logs.
func (w *Worker) fail(ctx context.Context, task *store.ClaimedTask, cause error) {
	log.Printf("worker %s: task %s attempt %d/%d failed: %v",
		w.ID, task.Name, task.Attempt, task.MaxAttempts, cause)
	switch err := w.store.FailTask(ctx, task, w.ID, cause.Error()); {
	case err == nil:
	case errors.Is(err, store.ErrLeaseLost):
		log.Printf("worker %s: task %s lease lost before failure recorded; dropping result", w.ID, task.Name)
	default:
		log.Printf("worker %s: recording task %s failure failed: %v", w.ID, task.Name, err)
	}
}

// heartbeat renews the task's lease every HeartbeatInterval until ctx is
// cancelled (the handler returned, or the worker is shutting down). It only
// keeps the lease alive; it does not stop the handler. If the lease is already
// gone -- the reaper decided this worker was dead and reclaimed the task -- there
// is nothing to renew, so it stops beating and lets the handler finish into the
// ErrLeaseLost drop that CompleteTask/FailTask already handle.
func (w *Worker) heartbeat(ctx context.Context, task *store.ClaimedTask) {
	ticker := time.NewTicker(w.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			held, err := w.store.Heartbeat(ctx, task.ID, w.ID, w.LeaseTTL)
			switch {
			case err != nil:
				// A cancelled ctx surfaces here as an error too; do not shout
				// about it, the loop is about to exit on the ctx.Done case.
				if ctx.Err() == nil {
					log.Printf("worker %s: heartbeat for task %s failed: %v", w.ID, task.Name, err)
				}
			case !held:
				log.Printf("worker %s: lost lease on task %s (reaped); heartbeats stopping", w.ID, task.Name)
				return
			}
		}
	}
}

// runHandler invokes a handler and turns a panic into an ordinary error, so a
// task that blows up is failed and retried like any other rather than taking the
// whole worker process down with it.
func runHandler(ctx context.Context, handler HandlerFunc, task store.ClaimedTask) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	return handler(ctx, task)
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
