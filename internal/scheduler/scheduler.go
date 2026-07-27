package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"weaver/internal/store"

	"github.com/robfig/cron/v3"
)

const (
	// How often the scheduler checks whether any workflow's schedule has come
	// due. Short relative to typical cron granularity (minutes) so a due slot
	// turns into a run within a few seconds of its time.
	defaultTickInterval = 10 * time.Second

	// Upper bound on how many missed slots a single tick will backfill for one
	// workflow. It caps the work per tick so a long-idle workflow with a frequent
	// schedule cannot create an unbounded flood of runs in one pass; whatever is
	// left is picked up on the next tick, since the baseline advances as runs are
	// created.
	maxCatchupSteps = 1000
)

// Scheduler turns time into work: each tick it asks the store for every scheduled
// workflow, computes whether a cron slot has come due since the last run it made,
// and creates a run for that slot. Making the run go through the store's
// slot-guarded insert is what keeps two schedulers from both firing the same slot.
type Scheduler struct {
	Interval time.Duration

	store  *store.Store
	parser cron.Parser
}

// NewScheduler builds a scheduler with a sensible tick interval. It parses
// standard 5-field cron specs ("m h dom mon dow"), matching the README examples.
func NewScheduler(s *store.Store) *Scheduler {
	return &Scheduler{
		Interval: defaultTickInterval,
		store:    s,
		// Standard cron: minute, hour, day-of-month, month, day-of-week.
		parser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
	}
}

// Run ticks on a timer until ctx is cancelled, checking due schedules once
// immediately so a freshly started scheduler does not wait a whole interval.
func (sc *Scheduler) Run(ctx context.Context) error {
	log.Printf("scheduler: started (interval=%s)", sc.Interval)
	ticker := time.NewTicker(sc.Interval)
	defer ticker.Stop()

	sc.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			log.Printf("scheduler: stopping (%v)", ctx.Err())
			return ctx.Err()
		case <-ticker.C:
			sc.tick(ctx)
		}
	}
}

// tick creates runs for any schedule that has come due. A failure on one workflow
// is logged and skipped so it cannot stall the others or bring the loop down; the
// next tick tries again.
func (sc *Scheduler) tick(ctx context.Context) {
	wfs, err := sc.store.ListScheduledWorkflows(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("scheduler: list scheduled workflows failed: %v", err)
		}
		return
	}

	now := time.Now().UTC()
	for _, wf := range wfs {
		if err := sc.scheduleDue(ctx, wf, now); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("scheduler: workflow %q (%s): %v", wf.Name, wf.ID, err)
		}
	}
}

// scheduleDue creates a run for every slot that has come due for the given
// workflow since the last run it produced, in order. A scheduler that was down
// therefore backfills each missed slot rather than skipping to the latest, so no
// scheduled run is silently dropped. Work is bounded to maxCatchupSteps slots per
// tick; anything beyond that is picked up on the next tick, since each created run
// advances the baseline.
func (sc *Scheduler) scheduleDue(ctx context.Context, wf store.ScheduledWorkflow, now time.Time) error {
	schedule, err := sc.parser.Parse(wf.Schedule)
	if err != nil {
		return fmt.Errorf("invalid schedule %q: %w", wf.Schedule, err)
	}

	// Baseline is the last slot we already created a run for; for a workflow that
	// has never fired, its creation time. Each due slot is the next activation
	// strictly after the previous one.
	last, err := sc.store.LastScheduledSlot(ctx, wf.ID)
	if err != nil {
		return err
	}
	baseline := wf.CreatedAt.UTC()
	if last != nil {
		baseline = last.UTC()
	}

	// Walk activations forward, creating a run for each slot at or before now.
	cursor := baseline
	for i := 0; i < maxCatchupSteps; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		due := schedule.Next(cursor)
		if due.After(now) {
			// Caught up: nothing more has come due.
			return nil
		}
		cursor = due

		runID, created, err := sc.store.CreateScheduledRun(ctx, wf.ID, wf.Def, due)
		if err != nil {
			return err
		}
		if created {
			log.Printf("scheduler: workflow %q due at %s -> run %s", wf.Name, due.Format(time.RFC3339), runID)
		}
	}

	// Hit the per-tick cap with slots still outstanding. Say so rather than let it
	// look like we caught up; the next tick resumes from the advanced baseline.
	log.Printf("scheduler: workflow %q backfilled %d slots this tick, more remain; continuing next tick", wf.Name, maxCatchupSteps)
	return nil
}
