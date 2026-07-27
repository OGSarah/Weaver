package store

import (
	"context"
	"fmt"
	"os"
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

// claimOne claims the next task and asserts it belongs to runID. A clean test
// database has only this run's work ready, so a foreign task is a real problem.
func claimOne(t *testing.T, s *Store, workerID, runID string) *ClaimedTask {
	t.Helper()
	ct, err := s.ClaimTask(context.Background(), workerID, 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ct == nil {
		t.Fatalf("wanted a claimable task, got none")
	}
	if ct.RunID != runID {
		t.Fatalf("claimed task from run %s, want %s (dirty test database?)", ct.RunID, runID)
	}
	return ct
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

	flaky := statusByName(t, s, runID)["flaky"]
	if flaky.Status != "ready" {
		t.Fatalf("after retry-eligible failure: want ready, got %s", flaky.Status)
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
	if got := statusByName(t, s, runID)["doomed"].Status; got != "ready" {
		t.Fatalf("after attempt 1: want ready, got %s", got)
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
	// Large attempts must stay capped, never overflow to a negative duration.
	if d := backoffDelay(1000); d <= 0 || d > maxBackoff {
		t.Errorf("capped delay out of range: %s", d)
	}
}
