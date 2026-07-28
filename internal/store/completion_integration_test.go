package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"weaver/internal/workflow"
)

// newTestStore connects to the test database or skips. Integration tests need a
// real Postgres (the whole point is the transactional state machine), so they
// are skipped rather than failed when DATABASE_URL is unset.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	s, err := New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// seedRun stores a workflow and materializes a run from it, registering cleanup
// that cascades tasks, deps and leases before removing the workflow.
func seedRun(t *testing.T, s *Store, def workflow.WorkflowDef) string {
	t.Helper()
	ctx := context.Background()
	workflowID, _, err := s.CreateWorkflow(ctx, def)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	runID, err := s.CreateRun(ctx, workflowID, def)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() {
		s.pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, runID)
		s.pool.Exec(ctx, `DELETE FROM workflows WHERE id = $1`, workflowID)
	})
	return runID
}

// claimOne claims until it gets a task belonging to runID.
//
// The claim query is global by design -- a worker takes the next eligible task in
// the whole queue, not one scoped to a run -- so a test cannot assume the only
// claimable work is its own. Runs left behind by earlier tests, or by anything
// else pointed at the same database, are real work as far as the queue is
// concerned; they are simply not what this test is asserting about. Skipping past
// them is what keeps a failure here meaning "the claim path is wrong" rather than
// "something else was in the queue".
func claimOne(t *testing.T, s *Store, workerID, runID string) *ClaimedTask {
	t.Helper()
	return claimOneWithTTL(t, s, workerID, runID, 30*time.Second)
}

// claimOneWithTTL is claimOne for a test that cares about the lease duration.
func claimOneWithTTL(t *testing.T, s *Store, workerID, runID string, ttl time.Duration) *ClaimedTask {
	t.Helper()

	// Within a sweep, foreign tasks are held rather than released: a claimed task is
	// Running and so no longer claimable, which is what makes the queue drain.
	// Releasing them as we went would put them straight back and the loop could
	// circle the same tasks forever.
	//
	// Between sweeps everything borrowed goes back, and the search waits a moment
	// before trying again. This run's task can be missing for a while rather than
	// forever: go test runs packages concurrently against one database, so another
	// package's test may be holding it, and it will let go.
	deadline := time.Now().Add(20 * time.Second)
	for attempt := 0; ; attempt++ {
		ct, borrowed := sweepForTask(t, s, workerID, runID, ttl)
		for _, other := range borrowed {
			releaseClaim(t, s, other)
		}
		if ct != nil {
			return ct
		}
		if time.Now().After(deadline) {
			t.Fatalf("no claimable task in run %s after %d sweeps", runID, attempt+1)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// sweepForTask claims until it finds runID's task or the queue is empty, returning
// that task (or nil) plus every foreign task it had to hold along the way.
func sweepForTask(t *testing.T, s *Store, workerID, runID string, ttl time.Duration) (*ClaimedTask, []*ClaimedTask) {
	t.Helper()
	ctx := context.Background()

	var borrowed []*ClaimedTask
	for {
		ct, err := s.ClaimTask(ctx, workerID, ttl)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if ct == nil {
			return nil, borrowed
		}
		if ct.RunID == runID {
			return ct, borrowed
		}
		borrowed = append(borrowed, ct)
	}
}

// releaseClaim undoes a claim, putting the task back as though it had never been
// taken. Two single-row writes scoped to the task rather than FailTask, which also
// locks the run row -- enough to deadlock against a test in another package
// working on the same run, since go test runs packages concurrently against one
// database.
func releaseClaim(t *testing.T, s *Store, ct *ClaimedTask) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx,
		`UPDATE tasks SET status = 'ready', attempt = attempt - 1, started_at = NULL
		  WHERE id = $1 AND status = 'running'`, ct.ID); err != nil {
		t.Logf("release foreign task %s: %v", ct.ID, err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM leases WHERE task_id = $1`, ct.ID); err != nil {
		t.Logf("release foreign lease %s: %v", ct.ID, err)
	}
}

func statusByName(t *testing.T, s *Store, runID string) map[string]TaskState {
	t.Helper()
	state, err := s.GetRunState(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run state: %v", err)
	}
	byName := make(map[string]TaskState, len(state.Tasks))
	for _, task := range state.Tasks {
		byName[task.Name] = task
	}
	return byName
}

// TestRunHappyPath drives a diamond DAG from trigger to finish: every task is
// claimed once, succeeds, unblocks its downstream, and the run lands Succeeded.
func TestRunHappyPath(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := seedRun(t, s, workflow.WorkflowDef{
		Name: fmt.Sprintf("happy-%d", time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{
			{ID: "extract", Handler: "h"},
			{ID: "transform", Handler: "h", DependsOn: []string{"extract"}},
			{ID: "validate", Handler: "h", DependsOn: []string{"extract"}},
			{ID: "load", Handler: "h", DependsOn: []string{"transform", "validate"}},
		},
	})

	// Drain the run the way a single worker would: claim, succeed, repeat. The
	// loop only terminates because each success unblocks the next task and,
	// eventually, nothing is left Ready -- exactly the DAG's acyclic guarantee.
	seen := map[string]bool{}
	for {
		ct, err := s.ClaimTask(ctx, "w1", 30*time.Second)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if ct == nil {
			break
		}
		if ct.RunID != runID {
			t.Fatalf("claimed foreign run %s (dirty test database?)", ct.RunID)
		}
		// "load" must never be claimable before both of its upstreams succeeded.
		if ct.Name == "load" && !(seen["transform"] && seen["validate"]) {
			t.Fatalf("load claimed before its upstreams succeeded")
		}
		if err := s.CompleteTask(ctx, ct, "w1"); err != nil {
			t.Fatalf("complete %s: %v", ct.Name, err)
		}
		seen[ct.Name] = true
	}

	if len(seen) != 4 {
		t.Fatalf("want 4 tasks executed, got %d (%v)", len(seen), seen)
	}

	state, err := s.GetRunState(ctx, runID)
	if err != nil {
		t.Fatalf("get run state: %v", err)
	}
	for _, task := range state.Tasks {
		if task.Status != "succeeded" {
			t.Errorf("task %s: want succeeded, got %s", task.Name, task.Status)
		}
		if task.FinishedAt == nil {
			t.Errorf("task %s: finished_at not set", task.Name)
		}
	}
	if state.Status != "succeeded" {
		t.Errorf("run status: want succeeded, got %s", state.Status)
	}
	if state.FinishedAt == nil {
		t.Errorf("run finished_at not set")
	}

	// No lease should outlive a finished run.
	var leases int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM leases l JOIN tasks t ON t.id = l.task_id WHERE t.run_id = $1`,
		runID,
	).Scan(&leases); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	if leases != 0 {
		t.Errorf("want no leases after completion, got %d", leases)
	}
}

// TestRunRetryThenSucceed fails a task once, confirms it is scheduled to retry
// with a future backoff, then lets the retry succeed and completes the run.
func TestRunRetryThenSucceed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := seedRun(t, s, workflow.WorkflowDef{
		Name:  fmt.Sprintf("retry-%d", time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{{ID: "flaky", Handler: "h", Retries: 2}}, // 3 attempts
	})

	// First attempt fails.
	ct := claimOne(t, s, "w1", runID)
	if ct.Attempt != 1 {
		t.Fatalf("first claim: want attempt 1, got %d", ct.Attempt)
	}
	if err := s.FailTask(ctx, ct, "w1", "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	// Failed, not ready: the attempt is behind it and the next one is waiting on
	// the backoff below, which is a different thing from sitting in the queue.
	flaky := statusByName(t, s, runID)["flaky"]
	if flaky.Status != "failed" {
		t.Fatalf("after retry-eligible failure: want failed, got %s", flaky.Status)
	}
	if flaky.Error == nil || *flaky.Error != "boom" {
		t.Errorf("want error recorded as boom, got %v", flaky.Error)
	}
	if !flaky.ScheduledAt.After(time.Now()) {
		t.Errorf("retry should be scheduled in the future, got %s", flaky.ScheduledAt)
	}
	// The lease from the failed attempt must be gone.
	var leases int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM leases WHERE task_id = $1`, flaky.ID).Scan(&leases); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	if leases != 0 {
		t.Errorf("want lease released after failure, got %d", leases)
	}

	// The backoff keeps it unclaimable right now; simulate the delay elapsing.
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET scheduled_at = now() WHERE id = $1`, flaky.ID); err != nil {
		t.Fatalf("fast-forward backoff: %v", err)
	}

	// Second attempt succeeds.
	ct = claimOne(t, s, "w1", runID)
	if ct.Attempt != 2 {
		t.Fatalf("second claim: want attempt 2, got %d", ct.Attempt)
	}
	if err := s.CompleteTask(ctx, ct, "w1"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	state, err := s.GetRunState(ctx, runID)
	if err != nil {
		t.Fatalf("get run state: %v", err)
	}
	if state.Tasks[0].Status != "succeeded" {
		t.Errorf("task: want succeeded, got %s", state.Tasks[0].Status)
	}
	if state.Tasks[0].Error != nil {
		t.Errorf("succeeded task should clear its error, got %v", state.Tasks[0].Error)
	}
	if state.Status != "succeeded" {
		t.Errorf("run: want succeeded, got %s", state.Status)
	}
}

// TestRunExhaustRetries fails a task on every attempt and confirms it lands Dead
// once attempts run out, taking the run to Failed with it.
func TestRunExhaustRetries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := seedRun(t, s, workflow.WorkflowDef{
		Name:  fmt.Sprintf("dead-%d", time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{{ID: "doomed", Handler: "h", Retries: 1}}, // 2 attempts
	})

	// Attempt 1: retry-eligible failure.
	ct := claimOne(t, s, "w1", runID)
	if err := s.FailTask(ctx, ct, "w1", "fail 1"); err != nil {
		t.Fatalf("fail 1: %v", err)
	}
	if got := statusByName(t, s, runID)["doomed"].Status; got != "failed" {
		t.Fatalf("after attempt 1: want failed, got %s", got)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET scheduled_at = now() WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("fast-forward backoff: %v", err)
	}

	// Attempt 2: no attempts left, so this failure is terminal.
	ct = claimOne(t, s, "w1", runID)
	if ct.Attempt != 2 {
		t.Fatalf("want attempt 2, got %d", ct.Attempt)
	}
	if err := s.FailTask(ctx, ct, "w1", "fail 2"); err != nil {
		t.Fatalf("fail 2: %v", err)
	}

	state, err := s.GetRunState(ctx, runID)
	if err != nil {
		t.Fatalf("get run state: %v", err)
	}
	doomed := state.Tasks[0]
	if doomed.Status != "dead" {
		t.Errorf("task: want dead, got %s", doomed.Status)
	}
	if doomed.Error == nil || *doomed.Error != "fail 2" {
		t.Errorf("want last error recorded, got %v", doomed.Error)
	}
	if doomed.FinishedAt == nil {
		t.Errorf("dead task should have finished_at set")
	}
	if state.Status != "failed" {
		t.Errorf("run: want failed, got %s", state.Status)
	}
	if state.FinishedAt == nil {
		t.Errorf("failed run should have finished_at set")
	}

	// A dead task must not be claimable again -- the retry loop is truly over.
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET scheduled_at = now() WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}
	if ct, err := s.ClaimTask(ctx, "w1", 30*time.Second); err != nil {
		t.Fatalf("claim after dead: %v", err)
	} else if ct != nil && ct.RunID == runID {
		t.Errorf("dead task was claimed again as attempt %d", ct.Attempt)
	}
}

// TestCancelRunCancelsTaskAwaitingRetry covers the trap in making Failed a
// waiting state rather than a terminal one: a task between attempts still has
// attempts left, so cancelling its run has to stop it too. Miss it and the task
// sits in Failed until its backoff elapses, then gets claimed and runs work for a
// run that was cancelled.
func TestCancelRunCancelsTaskAwaitingRetry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := seedRun(t, s, workflow.WorkflowDef{
		Name:  fmt.Sprintf("cancel-retry-%d", time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{{ID: "flaky", Handler: "h", Retries: 3}},
	})

	// Fail once so the task is waiting out a backoff with attempts to spare.
	ct := claimOne(t, s, "w1", runID)
	if err := s.FailTask(ctx, ct, "w1", "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if got := statusByName(t, s, runID)["flaky"].Status; got != "failed" {
		t.Fatalf("setup: want failed, got %s", got)
	}

	if err := s.CancelRun(ctx, runID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := statusByName(t, s, runID)["flaky"].Status; got != "cancelled" {
		t.Fatalf("after cancel: want cancelled, got %s", got)
	}

	// The real assertion: even once the backoff has elapsed, nothing is claimable.
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET scheduled_at = now() WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("fast-forward backoff: %v", err)
	}
	if ct, err := s.ClaimTask(ctx, "w1", 30*time.Second); err != nil {
		t.Fatalf("claim after cancel: %v", err)
	} else if ct != nil && ct.RunID == runID {
		t.Errorf("cancelled task was claimed as attempt %d", ct.Attempt)
	}
}

// TestTaskLogsSpanAttempts checks the log survives a retry and stays attributable:
// the lifecycle lines are written in the same transactions as the state changes
// they describe, and every line carries the attempt that produced it, so a task
// that failed once and then succeeded reads as two distinct attempts rather than
// one confused stream.
func TestTaskLogsSpanAttempts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := seedRun(t, s, workflow.WorkflowDef{
		Name:  fmt.Sprintf("logs-%d", time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{{ID: "flaky", Handler: "h", Retries: 2}},
	})

	// Attempt 1 fails, with a handler line of its own before it does.
	ct := claimOne(t, s, "w1", runID)
	if err := s.AppendTaskLog(ctx, ct.ID, ct.Attempt, LogInfo, "doing the thing"); err != nil {
		t.Fatalf("append log: %v", err)
	}
	if err := s.FailTask(ctx, ct, "w1", "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	// Attempt 2 succeeds.
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET scheduled_at = now() WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("fast-forward backoff: %v", err)
	}
	ct = claimOne(t, s, "w1", runID)
	if ct.Attempt != 2 {
		t.Fatalf("second claim: want attempt 2, got %d", ct.Attempt)
	}
	if err := s.CompleteTask(ctx, ct, "w1"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	lines, truncated, err := s.ListTaskLogs(ctx, ct.ID)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if truncated {
		t.Errorf("a handful of lines should not report truncation")
	}

	// Every line belongs to one of the two attempts, and they come back in order.
	var byAttempt = map[int][]string{}
	prev := 0
	for _, l := range lines {
		if l.Attempt < prev {
			t.Fatalf("attempts out of order: %d after %d", l.Attempt, prev)
		}
		prev = l.Attempt
		byAttempt[l.Attempt] = append(byAttempt[l.Attempt], l.Message)
	}

	// Attempt 1: claimed, the handler's own line, failed, and the retry notice.
	if got := len(byAttempt[1]); got < 4 {
		t.Errorf("attempt 1: want at least 4 lines (start, handler, failure, retry), got %d: %v",
			got, byAttempt[1])
	}
	if !containsSubstring(byAttempt[1], "doing the thing") {
		t.Errorf("attempt 1 lost the handler's line: %v", byAttempt[1])
	}
	if !containsSubstring(byAttempt[1], "boom") {
		t.Errorf("attempt 1 should record the failure cause: %v", byAttempt[1])
	}
	// Attempt 2: claimed and succeeded, with no trace of attempt 1's failure.
	if !containsSubstring(byAttempt[2], "succeeded") {
		t.Errorf("attempt 2 should record success: %v", byAttempt[2])
	}
	if containsSubstring(byAttempt[2], "boom") {
		t.Errorf("attempt 1's failure leaked into attempt 2: %v", byAttempt[2])
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

// TestBackoffDelay checks the backoff is monotonic up to the cap and always
// within the full-jitter window [d/2, d], without needing a database.
func TestBackoffDelay(t *testing.T) {
	for attempt := 1; attempt <= 12; attempt++ {
		ideal := baseBackoff << (attempt - 1)
		if ideal <= 0 || ideal > maxBackoff {
			ideal = maxBackoff
		}
		for i := 0; i < 100; i++ {
			d := backoffDelay(attempt)
			if d < ideal/2 || d > ideal {
				t.Fatalf("attempt %d: delay %s outside [%s, %s]", attempt, d, ideal/2, ideal)
			}
		}
	}
	// Large attempts must stay capped, never overflow to a negative duration. The
	// shift is what would overflow, so these bracket the clamp: one just inside it,
	// one far past it, and one past the point where the untruncated shift would
	// have wrapped int64 around into a negative delay.
	for _, attempt := range []int{30, 31, 63, 64, 65, 1000, 1 << 30} {
		d := backoffDelay(attempt)
		if d <= 0 || d > maxBackoff {
			t.Errorf("attempt %d: delay %s outside (0, %s]", attempt, d, maxBackoff)
		}
	}

	// A caller that has not attempted anything yet, or that passes a nonsense
	// attempt, still has to get a usable delay rather than a zero (retry now, in a
	// tight loop) or a negative one (a scheduled_at in the past, same thing).
	for _, attempt := range []int{0, -1, -1000} {
		d := backoffDelay(attempt)
		if d < baseBackoff/2 || d > baseBackoff {
			t.Errorf("attempt %d: delay %s outside the first-retry window [%s, %s]",
				attempt, d, baseBackoff/2, baseBackoff)
		}
	}

	// Full jitter exists to spread a batch of tasks that failed together. If it
	// ever collapsed to a constant, a stampede would retry in lockstep, which the
	// range check above cannot catch on its own.
	seen := make(map[time.Duration]bool)
	for i := 0; i < 200; i++ {
		seen[backoffDelay(5)] = true
	}
	if len(seen) < 50 {
		t.Errorf("200 delays produced only %d distinct values; jitter is not spreading retries", len(seen))
	}
}
