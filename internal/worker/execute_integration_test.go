package worker

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"weaver/internal/store"
	"weaver/internal/workflow"
)

// These drive Worker.execute against a real store, because everything worth
// checking about it is what ends up in the database: which state a panic leaves
// behind, whether a timed-out handler's result is written, what happens to work a
// worker no longer owns.

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

// seedRun stores a definition and materializes one run from it.
func seedRun(t *testing.T, s *store.Store, tasks []workflow.TaskDef) string {
	t.Helper()
	def := workflow.WorkflowDef{
		Name:  fmt.Sprintf("worker-test-%s-%d", t.Name(), time.Now().UnixNano()),
		Tasks: tasks,
	}
	if err := workflow.ValidateDef(def); err != nil {
		t.Fatalf("test definition is invalid: %v", err)
	}
	ctx := context.Background()
	workflowID, _, err := s.CreateWorkflow(ctx, def)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	runID, err := s.CreateRun(ctx, workflowID, def)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return runID
}

// claimFrom claims until it gets a task belonging to runID.
//
// The queue is global: a task left behind by an earlier test, or one belonging to
// a test in another package (go test runs packages concurrently against one
// database), is offered here just like this run's. Each sweep holds the foreign
// ones so the queue drains, then hands them back before trying again, so two
// searches running at once cannot starve each other.
func claimFrom(t *testing.T, s *store.Store, workerID, runID string, ttl time.Duration) *store.ClaimedTask {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)

	for attempt := 0; ; attempt++ {
		var borrowed []*store.ClaimedTask
		var found *store.ClaimedTask

		for found == nil {
			task, err := s.ClaimTask(ctx, workerID, ttl)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if task == nil {
				break
			}
			if task.RunID == runID {
				found = task
				break
			}
			borrowed = append(borrowed, task)
		}

		// FailTask is the only way back from here: this package cannot reach the
		// pool. It leaves the task claimable again after its backoff, which is what
		// the other search is waiting for.
		for _, other := range borrowed {
			if err := s.FailTask(ctx, other, workerID, "released by a test searching for another run"); err != nil {
				t.Logf("releasing foreign task %s: %v", other.ID, err)
			}
		}
		if found != nil {
			return found
		}
		if time.Now().After(deadline) {
			t.Fatalf("no claimable task in run %s after %d sweeps", runID, attempt+1)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func taskStates(t *testing.T, s *store.Store, runID string) map[string]store.TaskState {
	t.Helper()
	state, err := s.GetRunState(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run state: %v", err)
	}
	out := make(map[string]store.TaskState, len(state.Tasks))
	for _, task := range state.Tasks {
		out[task.Name] = task
	}
	return out
}

func testWorker(s *store.Store, id string, reg *Registry) *Worker {
	w := New(id, s, reg)
	// Beat far faster than the lease TTLs these tests use, so a handler that runs
	// for a second is never reaped out from under a worker that is alive.
	w.HeartbeatInterval = 200 * time.Millisecond
	return w
}

// A handler that returns cleanly succeeds and unblocks what was waiting on it.
func TestExecuteSucceedsAndUnblocksDownstream(t *testing.T) {
	s := newTestStore(t)
	runID := seedRun(t, s, []workflow.TaskDef{
		{ID: "first", Handler: "ok"},
		{ID: "second", Handler: "ok", DependsOn: []string{"first"}},
	})

	reg := NewRegistry()
	reg.Register("ok", func(_ context.Context, _ store.ClaimedTask, log *TaskLogger) error {
		log.Printf("did the work")
		return nil
	})
	w := testWorker(s, "worker-success", reg)

	task := claimFrom(t, s, w.ID, runID, 30*time.Second)
	w.execute(context.Background(), task)

	states := taskStates(t, s, runID)
	if got := states["first"].Status; got != "succeeded" {
		t.Errorf("first = %q, want succeeded", got)
	}
	if states["first"].Error != nil {
		t.Errorf("a succeeded task kept an error: %v", *states["first"].Error)
	}
	if states["first"].FinishedAt == nil {
		t.Error("a succeeded task has no finished_at")
	}
	// The point of doing both in one transaction: the downstream task is ready the
	// instant its upstream is done, with no sweep in between.
	if got := states["second"].Status; got != "ready" {
		t.Errorf("second = %q, want ready", got)
	}

	// The handler's own line is stored against the attempt that wrote it, next to
	// the lifecycle lines the claim and completion added.
	detail, err := s.GetTask(context.Background(), runID, states["first"].ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	lines, _, err := s.ListTaskLogs(context.Background(), detail.ID)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	var joined []string
	for _, l := range lines {
		if l.Attempt != 1 {
			t.Errorf("line %q recorded against attempt %d, want 1", l.Message, l.Attempt)
		}
		joined = append(joined, l.Message)
	}
	if !containsSubstring(joined, "did the work") {
		t.Errorf("the handler's line is missing from %v", joined)
	}
}

// A panicking handler must be recorded as a failure -- not crash the worker, and
// not be mistaken for a success because nothing returned an error.
func TestExecuteRecordsPanicAsFailure(t *testing.T) {
	s := newTestStore(t)
	runID := seedRun(t, s, []workflow.TaskDef{{ID: "boom", Handler: "panics", Retries: 1}})

	reg := NewRegistry()
	reg.Register("panics", func(context.Context, store.ClaimedTask, *TaskLogger) error {
		panic("handler exploded")
	})
	w := testWorker(s, "worker-panic", reg)

	task := claimFrom(t, s, w.ID, runID, 30*time.Second)
	w.execute(context.Background(), task) // must return normally

	state := taskStates(t, s, runID)["boom"]
	// Retries remain, so this is the waiting state rather than the terminal one.
	if state.Status != "failed" {
		t.Fatalf("status = %q, want failed", state.Status)
	}
	if state.Error == nil || !strings.Contains(*state.Error, "panicked") {
		t.Errorf("error = %v, want it to mention the panic", state.Error)
	}
	if !strings.Contains(*state.Error, "handler exploded") {
		t.Errorf("error = %v, want the panic value", *state.Error)
	}
	// Backoff pushes the next attempt into the future rather than retrying at once.
	if !state.ScheduledAt.After(time.Now()) {
		t.Errorf("scheduled_at = %s, want a time in the future", state.ScheduledAt)
	}
}

// A task naming a handler this worker does not have is a definition bug. It has to
// travel the failure path so it eventually dies visibly, rather than being dropped
// and leaving the run stalled with no explanation.
func TestExecuteFailsUnknownHandler(t *testing.T) {
	s := newTestStore(t)
	runID := seedRun(t, s, []workflow.TaskDef{{ID: "orphan", Handler: "nobody-registered-this"}})

	w := testWorker(s, "worker-unknown", NewRegistry())
	task := claimFrom(t, s, w.ID, runID, 30*time.Second)
	w.execute(context.Background(), task)

	state := taskStates(t, s, runID)["orphan"]
	// One attempt, no retries: straight to the terminal state.
	if state.Status != "dead" {
		t.Fatalf("status = %q, want dead", state.Status)
	}
	if state.Error == nil || !strings.Contains(*state.Error, "no handler registered") {
		t.Errorf("error = %v, want it to name the missing handler", state.Error)
	}

	// A dead task means the run can never fully succeed, so it fails now rather
	// than hanging until someone notices.
	run, err := s.GetRunState(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "failed" {
		t.Errorf("run status = %q, want failed", run.Status)
	}
}

// The timeout is enforced by the worker, not by the handler's good behaviour. Both
// halves matter: a handler that watches its context gets cancelled, and one that
// ignores it and returns nil late is still a failure rather than a success written
// after its deadline.
func TestExecuteEnforcesTimeout(t *testing.T) {
	t.Run("handler observes the cancellation", func(t *testing.T) {
		s := newTestStore(t)
		runID := seedRun(t, s, []workflow.TaskDef{{ID: "slow", Handler: "slow", TimeoutSeconds: 1}})

		observed := make(chan error, 1)
		reg := NewRegistry()
		reg.Register("slow", func(ctx context.Context, _ store.ClaimedTask, _ *TaskLogger) error {
			<-ctx.Done()
			observed <- ctx.Err()
			return ctx.Err()
		})
		w := testWorker(s, "worker-timeout", reg)

		task := claimFrom(t, s, w.ID, runID, 30*time.Second)
		w.execute(context.Background(), task)

		select {
		case err := <-observed:
			if err != context.DeadlineExceeded {
				t.Errorf("handler saw %v, want DeadlineExceeded", err)
			}
		default:
			t.Error("the handler's context was never cancelled")
		}

		state := taskStates(t, s, runID)["slow"]
		if state.Status != "dead" {
			t.Fatalf("status = %q, want dead", state.Status)
		}
		if state.Error == nil || !strings.Contains(*state.Error, "timed out after 1s") {
			t.Errorf("error = %v, want the timeout message", state.Error)
		}
	})

	t.Run("handler ignores the deadline and returns nil", func(t *testing.T) {
		s := newTestStore(t)
		runID := seedRun(t, s, []workflow.TaskDef{{ID: "rude", Handler: "rude", TimeoutSeconds: 1}})

		reg := NewRegistry()
		reg.Register("rude", func(context.Context, store.ClaimedTask, *TaskLogger) error {
			time.Sleep(1500 * time.Millisecond) // straight through the deadline
			return nil
		})
		w := testWorker(s, "worker-rude", reg)

		task := claimFrom(t, s, w.ID, runID, 30*time.Second)
		w.execute(context.Background(), task)

		state := taskStates(t, s, runID)["rude"]
		if state.Status != "dead" {
			t.Errorf("status = %q, want dead: a handler cannot succeed past its deadline", state.Status)
		}
	})
}

// A worker whose lease was reclaimed is finishing work that belongs to someone
// else. Writing its result would overwrite whatever the new owner is doing, so it
// has to be dropped -- and the drop must not look like a crash.
func TestExecuteDropsResultAfterLeaseIsReclaimed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	runID := seedRun(t, s, []workflow.TaskDef{{ID: "stolen", Handler: "slow-ish", Retries: 2}})

	reg := NewRegistry()
	reg.Register("slow-ish", func(context.Context, store.ClaimedTask, *TaskLogger) error {
		time.Sleep(1500 * time.Millisecond)
		return nil
	})
	w := testWorker(s, "worker-doomed", reg)
	// No heartbeats: this worker is about to be declared dead, and a heartbeat
	// would be it proving otherwise.
	w.HeartbeatInterval = time.Hour

	// A lease this short expires while the handler is still running.
	task := claimFrom(t, s, w.ID, runID, 500*time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.execute(ctx, task)
	}()

	// Let the lease lapse, then let the reaper hand the task to someone else while
	// the handler is still going.
	time.Sleep(700 * time.Millisecond)
	// The counts cover whatever else the database is holding, so this run's task is
	// checked by name below rather than by assuming the sweep found only it.
	if _, _, err := s.ReapExpiredLeases(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if got := taskStates(t, s, runID)["stolen"].Status; got != "failed" {
		t.Fatalf("after reaping, status = %q, want failed: the lease was not reclaimed", got)
	}
	<-done

	// The reaper's verdict stands: the task is waiting for its next attempt, not
	// marked succeeded by the worker that no longer owned it.
	state := taskStates(t, s, runID)["stolen"]
	if state.Status != "failed" {
		t.Errorf("status = %q, want failed (the reaper's requeue, not the late success)", state.Status)
	}
	if state.Error == nil || !strings.Contains(*state.Error, "lease expired") {
		t.Errorf("error = %v, want the reaper's note", state.Error)
	}
}

// Shutdown is not failure. A worker told to stop mid-handler must leave the task
// exactly as it found it and let the lease lapse, so the reaper gives the work to
// someone else rather than the run recording an error nobody caused.
func TestExecuteLeavesTaskAloneOnShutdown(t *testing.T) {
	s := newTestStore(t)
	runID := seedRun(t, s, []workflow.TaskDef{{ID: "interrupted", Handler: "waits", Retries: 2}})

	reg := NewRegistry()
	reg.Register("waits", func(ctx context.Context, _ store.ClaimedTask, _ *TaskLogger) error {
		<-ctx.Done()
		return ctx.Err()
	})
	w := testWorker(s, "worker-shutdown", reg)

	task := claimFrom(t, s, w.ID, runID, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.execute(ctx, task)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	state := taskStates(t, s, runID)["interrupted"]
	if state.Status != "running" {
		t.Errorf("status = %q, want running: an abandoned attempt is the reaper's to resolve", state.Status)
	}
	if state.Error != nil {
		t.Errorf("shutdown recorded an error on the task: %v", *state.Error)
	}
}

// Run is the loop everything else hangs off: poll, claim, execute, repeat. This
// drives it the way the binary does -- start it, let it find work on its own, then
// shut it down -- rather than calling execute directly as the tests above do.
func TestRunClaimsAndFinishesWorkThenStops(t *testing.T) {
	s := newTestStore(t)
	runID := seedRun(t, s, []workflow.TaskDef{
		{ID: "first", Handler: "chain"},
		{ID: "second", Handler: "chain", DependsOn: []string{"first"}},
	})

	reg := NewRegistry()
	reg.Register("chain", func(_ context.Context, _ store.ClaimedTask, log *TaskLogger) error {
		// Errorf records detail without failing the task: the line lands in the
		// log, and the handler still returns success.
		log.Errorf("a note about something odd")
		return nil
	})
	w := testWorker(s, "worker-loop", reg)
	w.PollInterval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- w.Run(ctx) }()

	// The second task only becomes claimable once the first succeeds, so waiting
	// for both proves the loop kept going rather than stopping after one claim.
	deadline := time.Now().Add(20 * time.Second)
	var states map[string]store.TaskState
	for time.Now().Before(deadline) {
		states = taskStates(t, s, runID)
		if states["first"].Status == "succeeded" && states["second"].Status == "succeeded" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if states["first"].Status != "succeeded" || states["second"].Status != "succeeded" {
		t.Fatalf("tasks did not both finish: first=%q second=%q",
			states["first"].Status, states["second"].Status)
	}

	// Every task succeeded, so the run is recorded as succeeded too.
	run, err := s.GetRunState(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "succeeded" {
		t.Errorf("run status = %q, want succeeded", run.Status)
	}

	// The handler's error-level line was kept even though the attempt succeeded.
	lines, _, err := s.ListTaskLogs(context.Background(), states["first"].ID)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	var levels []string
	for _, l := range lines {
		if l.Level == store.LogError {
			levels = append(levels, l.Message)
		}
	}
	if !containsSubstring(levels, "something odd") {
		t.Errorf("the handler's error line is missing: %v", levels)
	}

	// Shutdown: the loop returns the context's error rather than hanging.
	cancel()
	select {
	case err := <-stopped:
		if err == nil {
			t.Error("Run returned nil on shutdown; want the context error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
