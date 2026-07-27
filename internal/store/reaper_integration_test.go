package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"weaver/internal/workflow"
)

// expireLease drags a task's lease into the past so the reaper treats its worker
// as dead, the same state a real crash produces once heartbeats stop.
func expireLease(t *testing.T, s *Store, taskID string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE leases SET expires_at = now() - interval '1 second' WHERE task_id = $1`,
		taskID,
	); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
}

func leaseCount(t *testing.T, s *Store, taskID string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM leases WHERE task_id = $1`, taskID,
	).Scan(&n); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	return n
}

// TestReaperRequeuesDeadWorkersTask simulates a worker dying mid-task: it claims
// a task, its lease expires, and the reaper returns the task to Ready so another
// worker can finish it. This is the Phase 6 payoff in miniature.
func TestReaperRequeuesDeadWorkersTask(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := seedRun(t, s, workflow.WorkflowDef{
		Name:  fmt.Sprintf("reap-requeue-%d", time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{{ID: "job", Handler: "h", Retries: 2}}, // 3 attempts
	})

	// Worker "dead" claims the task, then "dies" -- we just stop heartbeating and
	// force its lease to expire.
	ct := claimOne(t, s, "dead", runID)
	if ct.Attempt != 1 {
		t.Fatalf("first claim: want attempt 1, got %d", ct.Attempt)
	}
	expireLease(t, s, ct.ID)

	// Counts are global to the shared test database, so assert on this task's
	// outcome, not the totals: Ready (not Dead) proves it was requeued.
	if _, _, err := s.ReapExpiredLeases(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}

	job := statusByName(t, s, runID)["job"]
	if job.Status != "ready" {
		t.Fatalf("reclaimed task: want ready, got %s", job.Status)
	}
	if leaseCount(t, s, ct.ID) != 0 {
		t.Errorf("reaper should have deleted the dead lease")
	}

	// A live worker picks it up and finishes it. The attempt count carried over,
	// so this is attempt 2 -- the dead worker's attempt was not free.
	ct2 := claimOne(t, s, "live", runID)
	if ct2.Attempt != 2 {
		t.Fatalf("reclaimed task: want attempt 2, got %d", ct2.Attempt)
	}
	if err := s.CompleteTask(ctx, ct2, "live"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if state, _ := s.GetRunState(ctx, runID); state.Status != "succeeded" {
		t.Errorf("run: want succeeded, got %s", state.Status)
	}
}

// TestReaperKillsExhaustedTask proves the recovery loop is bounded: a task whose
// worker dies on its final attempt is marked Dead, not requeued, so a task that
// reliably kills its worker cannot be reclaimed forever.
func TestReaperKillsExhaustedTask(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := seedRun(t, s, workflow.WorkflowDef{
		Name:  fmt.Sprintf("reap-kill-%d", time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{{ID: "poison", Handler: "h", Retries: 0}}, // 1 attempt
	})

	ct := claimOne(t, s, "dead", runID)
	if ct.Attempt != 1 || ct.MaxAttempts != 1 {
		t.Fatalf("want attempt 1 of 1, got %d of %d", ct.Attempt, ct.MaxAttempts)
	}
	expireLease(t, s, ct.ID)

	// Assert on this task, not the global counts: Dead (not Ready) proves the
	// reaper refused to requeue a task with no attempts left.
	if _, _, err := s.ReapExpiredLeases(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}

	state, _ := s.GetRunState(ctx, runID)
	poison := state.Tasks[0]
	if poison.Status != "dead" {
		t.Errorf("exhausted task: want dead, got %s", poison.Status)
	}
	if poison.FinishedAt == nil {
		t.Errorf("dead task should have finished_at set")
	}
	if state.Status != "failed" {
		t.Errorf("run: want failed, got %s", state.Status)
	}
	if leaseCount(t, s, ct.ID) != 0 {
		t.Errorf("reaper should have deleted the dead lease")
	}

	// The dead task must never be claimable again.
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET scheduled_at = now() WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}
	if got, err := s.ClaimTask(ctx, "live", 30*time.Second); err != nil {
		t.Fatalf("claim after dead: %v", err)
	} else if got != nil && got.RunID == runID {
		t.Errorf("dead task was claimed again as attempt %d", got.Attempt)
	}
}

// TestReaperLeavesLiveLeasesAlone confirms the reaper only touches expired
// leases: a task with time left on its lease -- a live, heartbeating worker --
// stays Running and untouched.
func TestReaperLeavesLiveLeasesAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := seedRun(t, s, workflow.WorkflowDef{
		Name:  fmt.Sprintf("reap-live-%d", time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{{ID: "job", Handler: "h"}},
	})

	// A generous TTL means the lease is nowhere near expiry.
	ct := claimOne(t, s, "alive", runID)

	// The sweep may reclaim unrelated expired leases in the shared database; what
	// matters is that it leaves this live one untouched.
	if _, _, err := s.ReapExpiredLeases(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if got := statusByName(t, s, runID)["job"].Status; got != "running" {
		t.Errorf("live task: want running, got %s", got)
	}
	if leaseCount(t, s, ct.ID) != 1 {
		t.Errorf("live lease should still be held, count = %d", leaseCount(t, s, ct.ID))
	}
}

// TestHeartbeatExtendsLease checks that a heartbeat pushes expiry out and, in
// doing so, keeps the reaper away from a task whose lease would otherwise lapse.
func TestHeartbeatExtendsLease(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := seedRun(t, s, workflow.WorkflowDef{
		Name:  fmt.Sprintf("heartbeat-%d", time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{{ID: "job", Handler: "h"}},
	})

	// Claim with a lease already on the edge of expiry.
	ct, err := s.ClaimTask(ctx, "alive", 1*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if ct == nil || ct.RunID != runID {
		t.Fatalf("wanted this run's task, got %v", ct)
	}

	// A heartbeat renews it well into the future.
	held, err := s.Heartbeat(ctx, ct.ID, "alive", 30*time.Second)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !held {
		t.Fatalf("heartbeat: want lease still held")
	}

	// Even after the original 1s TTL would have lapsed, the renewed lease keeps
	// the reaper away from this task.
	time.Sleep(1100 * time.Millisecond)
	if _, _, err := s.ReapExpiredLeases(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if got := statusByName(t, s, runID)["job"].Status; got != "running" {
		t.Errorf("heartbeated task: want running, got %s", got)
	}
}

// TestHeartbeatOnLostLease confirms a worker learns its lease is gone: after the
// reaper reclaims a task, the original worker's heartbeat reports the lease is no
// longer held rather than silently resurrecting it.
func TestHeartbeatOnLostLease(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := seedRun(t, s, workflow.WorkflowDef{
		Name:  fmt.Sprintf("heartbeat-lost-%d", time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{{ID: "job", Handler: "h", Retries: 1}},
	})

	ct := claimOne(t, s, "dead", runID)
	expireLease(t, s, ct.ID)
	if _, _, err := s.ReapExpiredLeases(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}

	// The dead worker, none the wiser, tries to heartbeat the lease the reaper
	// already deleted. It must be told the lease is gone.
	held, err := s.Heartbeat(ctx, ct.ID, "dead", 30*time.Second)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if held {
		t.Errorf("heartbeat on a reclaimed task should report the lease lost")
	}
	if leaseCount(t, s, ct.ID) != 0 {
		t.Errorf("a lost heartbeat must not recreate the lease")
	}
}
