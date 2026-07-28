package scheduler

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"weaver/internal/store"
	"weaver/internal/testsupport"
	"weaver/internal/workflow"

	"github.com/jackc/pgx/v5/pgxpool"
)

// scheduleDue is where cron specs become runs, and the thing it must never do is
// create a slot twice or skip one silently. Both are only observable against a
// real store, since the guard against duplicates is a unique index.

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	s, err := store.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// baseline is the workflow's creation time in these tests, and every "now" is
// derived from it. Both ends are fixed rather than taken from the clock: with a
// real now, a test asserting "five minutes have passed means five slots" is right
// or wrong depending on where in the current minute it happens to run. scheduleDue
// takes now as an argument precisely so it can be pinned.
//
// The timestamps do not have to be near the present. Nothing compares them against
// the clock, and each test registers its own workflow, so the unique index on
// (workflow_id, scheduled_for) cannot collide across tests.
var baseline = time.Date(2026, 3, 1, 12, 0, 30, 0, time.UTC)

// scheduledWorkflow registers a workflow with a schedule, presenting it to the
// scheduler as though it were created at baseline: the point missed slots are
// counted from.
func scheduledWorkflow(t *testing.T, s *store.Store, cron string) store.ScheduledWorkflow {
	t.Helper()
	ctx := context.Background()

	def := workflow.WorkflowDef{
		Name:     fmt.Sprintf("sched-%s-%d", t.Name(), time.Now().UnixNano()),
		Schedule: cron,
		Tasks:    []workflow.TaskDef{{ID: "only", Handler: "h"}},
	}
	id, _, err := s.CreateWorkflow(ctx, def)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Cleanup(func() {
		cleanupWorkflow(t, s, id)
	})

	return store.ScheduledWorkflow{
		ID:        id,
		Name:      def.Name,
		Schedule:  cron,
		CreatedAt: baseline,
		Def:       def,
	}
}

// A scheduler that was down has to backfill every slot it missed, in order, rather
// than skipping to the latest. Otherwise a nightly job that was down for a week
// silently loses six days of work.
func TestScheduleDueBackfillsEveryMissedSlot(t *testing.T) {
	s := newTestStore(t)
	sc := NewScheduler(s)

	// Registered at 12:00:30 and not looked at again until 12:05:30: the minute
	// boundaries at 12:01 through 12:05 all came due while nothing was watching.
	wf := scheduledWorkflow(t, s, "* * * * *")
	now := baseline.Add(5 * time.Minute)

	if err := sc.scheduleDue(context.Background(), wf, now); err != nil {
		t.Fatalf("scheduleDue: %v", err)
	}

	slots := scheduledSlots(t, s, wf.ID)
	want := []time.Time{
		baseline.Add(30 * time.Second),               // 12:01
		baseline.Add(time.Minute + 30*time.Second),   // 12:02
		baseline.Add(2*time.Minute + 30*time.Second), // 12:03
		baseline.Add(3*time.Minute + 30*time.Second), // 12:04
		baseline.Add(4*time.Minute + 30*time.Second), // 12:05
	}
	if len(slots) != len(want) {
		t.Fatalf("created %d runs, want %d (one per missed minute): %v", len(slots), len(want), slots)
	}
	for i := range want {
		if !slots[i].Equal(want[i]) {
			t.Errorf("slot %d = %s, want %s", i, slots[i], want[i])
		}
	}
	// The boundary itself: a slot exactly at now is due, and nothing past it is.
	if slots[len(slots)-1].After(now) {
		t.Errorf("last slot %s is past now (%s)", slots[len(slots)-1], now)
	}
}

// Running the same tick twice, or two schedulers racing on it, must not double a
// slot: the run for a given (workflow, slot) is created exactly once.
func TestScheduleDueIsIdempotentPerSlot(t *testing.T) {
	s := newTestStore(t)
	sc := NewScheduler(s)

	wf := scheduledWorkflow(t, s, "* * * * *")
	now := baseline.Add(3 * time.Minute)

	for i := 0; i < 3; i++ {
		if err := sc.scheduleDue(context.Background(), wf, now); err != nil {
			t.Fatalf("scheduleDue pass %d: %v", i, err)
		}
	}

	slots := scheduledSlots(t, s, wf.ID)
	if len(slots) != 3 {
		t.Fatalf("three passes created %d runs, want 3: %v", len(slots), slots)
	}
	seen := map[time.Time]bool{}
	for _, slot := range slots {
		if seen[slot] {
			t.Errorf("slot %s has more than one run", slot)
		}
		seen[slot] = true
	}
}

// Nothing due yet means nothing created. A tick that fires every ten seconds
// against an hourly schedule must not produce a run each time it looks.
func TestScheduleDueCreatesNothingBeforeTheFirstSlot(t *testing.T) {
	s := newTestStore(t)
	sc := NewScheduler(s)

	// Hourly, registered at 12:00:30: the next slot is 13:00, so half an hour of
	// ticks must produce nothing.
	wf := scheduledWorkflow(t, s, "0 * * * *")

	for i := 0; i < 3; i++ {
		if err := sc.scheduleDue(context.Background(), wf, baseline.Add(30*time.Minute)); err != nil {
			t.Fatalf("scheduleDue: %v", err)
		}
	}
	if slots := scheduledSlots(t, s, wf.ID); len(slots) != 0 {
		t.Errorf("created %d runs before the first slot was due: %v", len(slots), slots)
	}
}

// A schedule the parser rejects is an error the tick reports and moves past, not
// something that silently produces runs or takes the loop down. The API validates
// on registration, so reaching this means the row was written some other way.
func TestScheduleDueRejectsUnparseableSchedules(t *testing.T) {
	s := newTestStore(t)
	sc := NewScheduler(s)

	for _, spec := range []string{"", "not a cron", "* * *", "0 0 6 * * *", "99 * * * *", "@yearly"} {
		wf := scheduledWorkflow(t, s, "* * * * *")
		wf.Schedule = spec // bypass the API's validation, as a hand-edited row would

		err := sc.scheduleDue(context.Background(), wf, baseline.Add(time.Hour))
		if err == nil {
			t.Errorf("schedule %q was accepted", spec)
			continue
		}
		if slots := scheduledSlots(t, s, wf.ID); len(slots) != 0 {
			t.Errorf("schedule %q created %d runs before failing", spec, len(slots))
		}
	}
}

// A backfilled run is a real run: it materializes the workflow's tasks with its
// roots ready, so a worker picks it up exactly like a manually triggered one.
func TestBackfilledRunIsRunnable(t *testing.T) {
	s := newTestStore(t)
	sc := NewScheduler(s)

	wf := scheduledWorkflow(t, s, "* * * * *")
	if err := sc.scheduleDue(context.Background(), wf, baseline.Add(90*time.Second)); err != nil {
		t.Fatalf("scheduleDue: %v", err)
	}

	history, err := s.ListRunHistory(context.Background(), wf.ID, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("no run was created")
	}
	run, err := s.GetRunState(context.Background(), history[0].ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(run.Tasks) != 1 {
		t.Fatalf("run has %d tasks, want 1", len(run.Tasks))
	}
	if run.Tasks[0].Status != "ready" {
		t.Errorf("root task is %q, want ready", run.Tasks[0].Status)
	}
	if run.Status != "pending" {
		t.Errorf("run status = %q, want pending", run.Status)
	}
}

// One workflow with an unusable schedule must not stop the others from being
// scheduled. tick is the loop that has to survive it: a single bad row would
// otherwise stall every scheduled workflow in the system until someone noticed.
func TestTickKeepsGoingPastABrokenWorkflow(t *testing.T) {
	s := newTestStore(t)
	sc := NewScheduler(s)
	ctx := context.Background()

	// A schedule the parser rejects, written straight to the row: registration
	// would have refused it, which is why this can only arrive some other way.
	broken := scheduledWorkflow(t, s, "* * * * *")
	if _, err := testPool.Exec(ctx,
		`UPDATE workflows SET schedule = 'not a cron' WHERE id = $1`, broken.ID); err != nil {
		t.Fatalf("break schedule: %v", err)
	}

	// A healthy one, backdated so slots are already due when the tick runs.
	healthy := scheduledWorkflow(t, s, "* * * * *")
	if _, err := testPool.Exec(ctx,
		`UPDATE workflows SET created_at = now() - interval '3 minutes' WHERE id = $1`, healthy.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	sc.tick(ctx) // must not panic, and must not stop at the broken row

	if got := len(scheduledSlots(t, s, broken.ID)); got != 0 {
		t.Errorf("the unparseable schedule produced %d runs", got)
	}
	if got := len(scheduledSlots(t, s, healthy.ID)); got == 0 {
		t.Error("the healthy workflow was skipped because another one was broken")
	}
}

// A cancelled context stops the tick rather than letting it work through the list
// against a database that is going away with the process.
func TestTickStopsOnCancelledContext(t *testing.T) {
	s := newTestStore(t)
	sc := NewScheduler(s)

	wf := scheduledWorkflow(t, s, "* * * * *")
	if _, err := testPool.Exec(context.Background(),
		`UPDATE workflows SET created_at = now() - interval '3 minutes' WHERE id = $1`, wf.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sc.tick(ctx) // returns rather than hanging or panicking

	if got := len(scheduledSlots(t, s, wf.ID)); got != 0 {
		t.Errorf("a cancelled tick created %d runs", got)
	}
}

// The reaper's sweep is the scheduled half of recovery: the store does the work,
// and this is the loop that has to call it and survive whatever comes back.
func TestReaperSweepRecoversStrandedWork(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// A run whose task is claimed and then abandoned with an expired lease.
	wf := scheduledWorkflow(t, s, "* * * * *")
	runID, created, err := s.CreateScheduledRun(ctx, wf.ID, wf.Def, baseline)
	if err != nil || !created {
		t.Fatalf("create run: %v (created=%v)", err, created)
	}
	var taskID string
	if err := testPool.QueryRow(ctx,
		`SELECT id FROM tasks WHERE run_id = $1`, runID).Scan(&taskID); err != nil {
		t.Fatalf("find task: %v", err)
	}
	// Attempts to spare, so the sweep's decision is "hand it back" rather than
	// "give up on it": the requeue is the path this test is about.
	if _, err := testPool.Exec(ctx,
		`UPDATE tasks SET status = 'running', attempt = 1, max_attempts = 3 WHERE id = $1`, taskID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO leases (task_id, worker_id, expires_at)
		 VALUES ($1, 'ghost', now() - interval '1 minute')`, taskID); err != nil {
		t.Fatalf("write expired lease: %v", err)
	}

	NewReaper(s).sweep(ctx)

	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1`, taskID).Scan(&status); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if status != "failed" {
		t.Errorf("status = %q, want failed (requeued for another attempt)", status)
	}
	var leases int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM leases WHERE task_id = $1`, taskID).Scan(&leases); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	if leases != 0 {
		t.Errorf("the dead worker's lease survived the sweep")
	}

	// A sweep with a cancelled context is a no-op that reports nothing, rather
	// than a crash on the way down.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	NewReaper(s).sweep(cancelled)
}

// Both loops sweep once immediately rather than waiting out a full interval, so a
// process that just restarted from a crash recovers now instead of in ten seconds,
// and both return when their context is cancelled rather than outliving it.
func TestLoopsWorkImmediatelyAndStopOnCancel(t *testing.T) {
	s := newTestStore(t)

	wf := scheduledWorkflow(t, s, "* * * * *")
	if _, err := testPool.Exec(context.Background(),
		`UPDATE workflows SET created_at = now() - interval '2 minutes' WHERE id = $1`, wf.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	sc := NewScheduler(s)
	sc.Interval = time.Hour // only the immediate first tick can fire

	reaper := NewReaper(s)
	reaper.Interval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	schedulerDone := make(chan error, 1)
	reaperDone := make(chan error, 1)
	go func() { schedulerDone <- sc.Run(ctx) }()
	go func() { reaperDone <- reaper.Run(ctx) }()

	// The first tick happens before the ticker ever fires, so runs appear well
	// inside the hour-long interval.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(scheduledSlots(t, s, wf.ID)) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := len(scheduledSlots(t, s, wf.ID)); got == 0 {
		t.Error("no runs after starting the scheduler; the first tick is not immediate")
	}

	cancel()
	for name, done := range map[string]chan error{"scheduler": schedulerDone, "reaper": reaperDone} {
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s returned nil on shutdown; want the context error", name)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s did not return after its context was cancelled", name)
		}
	}
}

// scheduled_for is what the exactly-once guard is built on, and no exported store
// method returns it, so these two read and clean up the column directly over their
// own connection.

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		pool, err := pgxpool.New(context.Background(), url)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scheduler tests: connect: %v\n", err)
			os.Exit(1)
		}
		testPool = pool
		defer pool.Close()
	}
	// Serialized against the other database-backed packages: see testsupport.
	os.Exit(testsupport.RunSerialized(m))
}

// scheduledSlots reads back the cron slots runs were created for, oldest first.
func scheduledSlots(t *testing.T, s *store.Store, workflowID string) []time.Time {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT scheduled_for FROM runs
		  WHERE workflow_id = $1 AND scheduled_for IS NOT NULL
		  ORDER BY scheduled_for`, workflowID)
	if err != nil {
		t.Fatalf("query slots: %v", err)
	}
	defer rows.Close()

	var slots []time.Time
	for rows.Next() {
		var slot time.Time
		if err := rows.Scan(&slot); err != nil {
			t.Fatalf("scan slot: %v", err)
		}
		slots = append(slots, slot.UTC())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate slots: %v", err)
	}
	return slots
}

func cleanupWorkflow(t *testing.T, s *store.Store, workflowID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `DELETE FROM runs WHERE workflow_id = $1`, workflowID); err != nil {
		t.Logf("cleanup runs: %v", err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM workflows WHERE id = $1`, workflowID); err != nil {
		t.Logf("cleanup workflow: %v", err)
	}
}
