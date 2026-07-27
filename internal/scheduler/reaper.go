package scheduler

import (
	"context"
	"log"
	"time"

	"weaver/internal/store"
)

// How often the reaper sweeps for expired leases. Short relative to the lease
// TTL so a dead worker's task is reclaimed quickly, but not so short that the
// sweep query becomes its own load.
const defaultReapInterval = 5 * time.Second

// Reaper periodically returns tasks stranded by dead workers to the queue. It is
// the counterpart to the workers' heartbeats: workers say "still alive" by
// renewing leases, and the reaper acts on the silence when they stop. Running it
// in the scheduler (a single control-plane process) keeps recovery centralized,
// though it is safe to run several -- SKIP LOCKED means two reapers never fight
// over the same lease.
type Reaper struct {
	Interval time.Duration

	store *store.Store
}

// NewReaper builds a reaper with a sensible sweep interval.
func NewReaper(s *store.Store) *Reaper {
	return &Reaper{
		Interval: defaultReapInterval,
		store:    s,
	}
}

// Run sweeps on a timer until ctx is cancelled. It sweeps once immediately so a
// fresh scheduler does not wait a whole interval before recovering work left
// behind by a crash it just restarted from.
func (r *Reaper) Run(ctx context.Context) error {
	log.Printf("reaper: started (interval=%s)", r.Interval)
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	r.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			log.Printf("reaper: stopping (%v)", ctx.Err())
			return ctx.Err()
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

// sweep runs one reap pass and logs what it recovered. A failed sweep is logged
// and swallowed: the next tick tries again, so a transient database blip does not
// take the scheduler down.
func (r *Reaper) sweep(ctx context.Context) {
	requeued, killed, err := r.store.ReapExpiredLeases(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("reaper: sweep failed: %v", err)
		}
		return
	}
	if requeued > 0 || killed > 0 {
		log.Printf("reaper: recovered %d task(s): %d requeued, %d killed", requeued+killed, requeued, killed)
	}
}
