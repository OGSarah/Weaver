package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"weaver/internal/workflow"
)

// The guards in this file are the ones that decide whether a task runs at all:
// who owns a claim, what is eligible to be claimed, and what a cancelled or
// finished run is still allowed to do. Each is a way a run could quietly do the
// wrong thing rather than fail loudly.

func oneTaskRun(t *testing.T, s *Store, retries int) string {
	t.Helper()
	return seedRun(t, s, workflow.WorkflowDef{
		Name:  fmt.Sprintf("guard-%s-%d", t.Name(), time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{{ID: "only", Handler: "h", Retries: retries}},
	})
}

// A result is only writable by the worker that holds the lease. Without the check
// a stale worker could overwrite the state of an attempt someone else is running.
func TestOnlyTheLeaseHolderCanRecordAResult(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	t.Run("complete by another worker", func(t *testing.T) {
		runID := oneTaskRun(t, s, 2)
		ct := claimOne(t, s, "owner", runID)

		if err := s.CompleteTask(ctx, ct, "impostor"); err != ErrLeaseLost {
			t.Fatalf("err = %v, want ErrLeaseLost", err)
		}
		if got := statusByName(t, s, runID)["only"].Status; got != "running" {
			t.Errorf("status = %q, want running: the impostor's write must not land", got)
		}
		// The real holder can still finish, so the rejection cost it nothing.
		if err := s.CompleteTask(ctx, ct, "owner"); err != nil {
			t.Fatalf("owner complete: %v", err)
		}
	})

	t.Run("fail by another worker", func(t *testing.T) {
		runID := oneTaskRun(t, s, 2)
		ct := claimOne(t, s, "owner", runID)

		if err := s.FailTask(ctx, ct, "impostor", "not mine to say"); err != ErrLeaseLost {
			t.Fatalf("err = %v, want ErrLeaseLost", err)
		}
		state := statusByName(t, s, runID)["only"]
		if state.Status != "running" {
			t.Errorf("status = %q, want running", state.Status)
		}
		if state.Error != nil {
			t.Errorf("the impostor's cause was recorded: %v", *state.Error)
		}
	})

	// At-least-once delivery means a worker can be handed the same task twice, or
	// retry its own completion after a timeout. The second write has to be refused
	// rather than applied on top of whatever happened since.
	t.Run("completing twice", func(t *testing.T) {
		runID := oneTaskRun(t, s, 2)
		ct := claimOne(t, s, "owner", runID)

		if err := s.CompleteTask(ctx, ct, "owner"); err != nil {
			t.Fatalf("first complete: %v", err)
		}
		if err := s.CompleteTask(ctx, ct, "owner"); err != ErrLeaseLost {
			t.Errorf("second complete: err = %v, want ErrLeaseLost", err)
		}
		if got := statusByName(t, s, runID)["only"].Status; got != "succeeded" {
			t.Errorf("status = %q, want succeeded", got)
		}
	})

	// Failing after completing must not resurrect a finished task into a retry.
	t.Run("failing after completing", func(t *testing.T) {
		runID := oneTaskRun(t, s, 2)
		ct := claimOne(t, s, "owner", runID)

		if err := s.CompleteTask(ctx, ct, "owner"); err != nil {
			t.Fatalf("complete: %v", err)
		}
		if err := s.FailTask(ctx, ct, "owner", "late failure"); err != ErrLeaseLost {
			t.Errorf("err = %v, want ErrLeaseLost", err)
		}
		state := statusByName(t, s, runID)["only"]
		if state.Status != "succeeded" || state.Error != nil {
			t.Errorf("task = %q with error %v; a finished task must stay finished", state.Status, state.Error)
		}
	})
}

// scheduled_at is the only thing gating a retry, so a task waiting out its backoff
// must be invisible to the claim query until its time comes. If it were claimable
// early, the backoff would not exist.
func TestBackedOffTaskIsNotClaimableUntilItsTime(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := oneTaskRun(t, s, 3)
	ct := claimOne(t, s, "w1", runID)
	if err := s.FailTask(ctx, ct, "w1", "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	// The delay is at least half a second of real time, so an immediate poll must
	// come back with nothing for this run.
	for i := 0; i < 3; i++ {
		got, err := s.ClaimTask(ctx, "w2", 30*time.Second)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if got != nil && got.RunID == runID {
			t.Fatalf("a task inside its backoff was claimed as attempt %d", got.Attempt)
		}
	}

	// Once the delay has passed it becomes claimable again, and the attempt
	// carries on from where it left off rather than restarting at 1.
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET scheduled_at = now() WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}
	got := claimOne(t, s, "w2", runID)
	if got.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", got.Attempt)
	}
}

// A worker crash on a cancelled run is the case where two safety mechanisms meet:
// cancellation stops anything not yet running, and the reaper hands abandoned work
// back. If the reaper requeues without asking whether the run still wants the
// work, the task becomes claimable again and a cancelled run executes.
func TestReaperDoesNotResurrectACancelledRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := oneTaskRun(t, s, 5)
	ct := claimOne(t, s, "doomed-worker", runID)

	// Cancel while the task is running: the row stays running because a handler
	// cannot be stopped, and its lease is still live.
	if err := s.CancelRun(ctx, runID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := statusByName(t, s, runID)["only"].Status; got != "running" {
		t.Fatalf("setup: status = %q, want running", got)
	}

	// Now the worker dies: its lease lapses and the reaper sweeps.
	expireLease(t, s, ct.ID)
	if _, _, err := s.ReapExpiredLeases(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}

	state := statusByName(t, s, runID)["only"]
	if state.Status == "failed" {
		t.Fatalf("the reaper requeued a task whose run was cancelled; it is claimable again")
	}
	if state.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", state.Status)
	}

	// The real damage the status is protecting against: nothing may pick this up.
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET scheduled_at = now() WHERE run_id = $1`, runID); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}
	if got, err := s.ClaimTask(ctx, "w2", 30*time.Second); err != nil {
		t.Fatalf("claim: %v", err)
	} else if got != nil && got.RunID == runID {
		t.Error("a cancelled run's task was claimed after the reaper touched it")
	}

	// And the run stays cancelled rather than being recorded as a failure, which
	// would misreport a deliberate stop as something going wrong.
	run, err := s.GetRunState(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "cancelled" {
		t.Errorf("run status = %q, want cancelled", run.Status)
	}
}

func TestCancelRunRejectsWhatItCannotCancel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CancelRun(ctx, "00000000-0000-0000-0000-000000000000"); err != ErrNotFound {
		t.Errorf("cancelling an unknown run: err = %v, want ErrNotFound", err)
	}
	if err := s.CancelRun(ctx, "not-a-uuid"); err != ErrNotFound {
		t.Errorf("cancelling a malformed id: err = %v, want ErrNotFound", err)
	}

	// A run that already finished is not cancellable, and saying so is what lets a
	// UI tell "I stopped it" from "it was already over".
	runID := oneTaskRun(t, s, 0)
	ct := claimOne(t, s, "w1", runID)
	if err := s.CompleteTask(ctx, ct, "w1"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := s.CancelRun(ctx, runID); err != ErrNotCancellable {
		t.Errorf("cancelling a succeeded run: err = %v, want ErrNotCancellable", err)
	}

	// Cancelling twice is the same story, and must not clobber the first cancel's
	// finished_at.
	other := oneTaskRun(t, s, 0)
	if err := s.CancelRun(ctx, other); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	before, err := s.GetRunState(ctx, other)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if err := s.CancelRun(ctx, other); err != ErrNotCancellable {
		t.Errorf("second cancel: err = %v, want ErrNotCancellable", err)
	}
	after, err := s.GetRunState(ctx, other)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !before.FinishedAt.Equal(*after.FinishedAt) {
		t.Errorf("the refused cancel moved finished_at from %s to %s", before.FinishedAt, after.FinishedAt)
	}
}

// Two workers cancelling the same run at once, or a cancel racing a completion,
// must resolve to exactly one cancellation. The run row is locked for the check,
// so the losers see a run that is no longer cancellable rather than both
// "succeeding" and writing over each other.
func TestConcurrentCancelsResolveToOne(t *testing.T) {
	s := newTestStore(t)
	runID := oneTaskRun(t, s, 0)

	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s.CancelRun(context.Background(), runID)
		}(i)
	}
	wg.Wait()

	var cancelled, refused int
	for i, err := range results {
		switch err {
		case nil:
			cancelled++
		case ErrNotCancellable:
			refused++
		default:
			t.Errorf("racer %d: unexpected error %v", i, err)
		}
	}
	if cancelled != 1 {
		t.Errorf("%d cancels reported success, want exactly 1", cancelled)
	}
	if refused != racers-1 {
		t.Errorf("%d cancels were refused, want %d", refused, racers-1)
	}
}

// Cancelling a run while workers are claiming from it is the ordinary case, not an
// exotic one: a user hits cancel on a run that is midway through. Both paths write
// the same two tables, so if they take the locks in opposite orders Postgres
// aborts one of them with a deadlock -- a cancel that 500s, or a worker whose
// claim fails, at random and only under load.
func TestCancelDoesNotDeadlockAgainstClaims(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Enough tasks that claims are still arriving when the cancel lands.
	tasks := make([]workflow.TaskDef, 40)
	for i := range tasks {
		tasks[i] = workflow.TaskDef{ID: fmt.Sprintf("t-%02d", i), Handler: "h"}
	}

	for round := 0; round < 5; round++ {
		runID := seedRun(t, s, workflow.WorkflowDef{
			Name:  fmt.Sprintf("cancel-race-%d-%d", round, time.Now().UnixNano()),
			Tasks: tasks,
		})

		var wg sync.WaitGroup
		errs := make(chan error, 16)

		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(workerID string) {
				defer wg.Done()
				for i := 0; i < 12; i++ {
					ct, err := s.ClaimTask(ctx, workerID, 30*time.Second)
					if err != nil {
						errs <- fmt.Errorf("claim: %w", err)
						return
					}
					if ct == nil {
						return
					}
					if ct.RunID != runID {
						continue // another test's work; leave it claimed and move on
					}
					if err := s.CompleteTask(ctx, ct, workerID); err != nil && err != ErrLeaseLost {
						errs <- fmt.Errorf("complete: %w", err)
						return
					}
				}
			}(fmt.Sprintf("racer-%d", w))
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.CancelRun(ctx, runID); err != nil && err != ErrNotCancellable {
				errs <- fmt.Errorf("cancel: %w", err)
			}
		}()

		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("round %d: %v", round, err)
		}
	}
}

// The log cap is what keeps a runaway handler from handing a browser an unbounded
// response. When it trips, the newest lines are the ones kept -- a failure is at
// the end of a log, not the beginning -- and the caller is told it is partial.
func TestTaskLogsAreCappedKeepingTheNewest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	runID := oneTaskRun(t, s, 0)
	ct := claimOne(t, s, "w1", runID)

	const written = defaultLogLimit + 100
	for i := 0; i < written; i++ {
		if err := s.AppendTaskLog(ctx, ct.ID, ct.Attempt, LogInfo, fmt.Sprintf("line %04d", i)); err != nil {
			t.Fatalf("append log %d: %v", i, err)
		}
	}

	lines, truncated, err := s.ListTaskLogs(ctx, ct.ID)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if !truncated {
		t.Error("truncated = false after writing past the cap")
	}
	if len(lines) != defaultLogLimit {
		t.Fatalf("got %d lines, want the cap of %d", len(lines), defaultLogLimit)
	}
	// Oldest-first within what survived, and what survived is the tail.
	if !strings.Contains(lines[len(lines)-1].Message, fmt.Sprintf("line %04d", written-1)) {
		t.Errorf("last line is %q, want the most recent one", lines[len(lines)-1].Message)
	}
	if strings.Contains(lines[0].Message, "line 0000") {
		t.Error("the oldest line survived the cap; the wrong end was dropped")
	}
	for i := 1; i < len(lines); i++ {
		if lines[i].LoggedAt.Before(lines[i-1].LoggedAt) {
			t.Fatalf("line %d is older than the one before it", i)
		}
	}
}

// A page of history is bounded whatever the caller asks for, because a scheduled
// workflow accumulates runs forever and the browser is on the other end.
func TestRunHistoryIsAlwaysBounded(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	def := workflow.WorkflowDef{
		Name:  fmt.Sprintf("history-cap-%d", time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{{ID: "only", Handler: "h"}},
	}
	workflowID, _, err := s.CreateWorkflow(ctx, def)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Cleanup(func() {
		s.pool.Exec(ctx, `DELETE FROM runs WHERE workflow_id = $1`, workflowID)
		s.pool.Exec(ctx, `DELETE FROM workflows WHERE id = $1`, workflowID)
	})

	for i := 0; i < maxRunHistory+5; i++ {
		if _, err := s.CreateRun(ctx, workflowID, def); err != nil {
			t.Fatalf("create run %d: %v", i, err)
		}
	}

	for _, limit := range []int{0, -1, maxRunHistory, maxRunHistory + 1, 1_000_000} {
		got, err := s.ListRunHistory(ctx, workflowID, limit)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if len(got) != maxRunHistory {
			t.Errorf("limit %d returned %d runs, want the cap of %d", limit, len(got), maxRunHistory)
		}
	}

	// A limit inside the cap is honoured exactly, so paging still works.
	got, err := s.ListRunHistory(ctx, workflowID, 3)
	if err != nil {
		t.Fatalf("limit 3: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("limit 3 returned %d runs", len(got))
	}
	// Newest first: the page a caller gets is the most recent runs, not the oldest.
	for i := 1; i < len(got); i++ {
		if got[i-1].CreatedAt.Before(got[i].CreatedAt) {
			t.Errorf("history is not newest-first at position %d", i)
		}
	}
}

// The scheduler acts on exactly what this returns, so the filtering is the whole
// behaviour: an unscheduled workflow must not appear, and a name must appear once,
// as its newest version. Get it wrong and either a schedule silently stops firing
// or an old definition keeps running alongside the current one.
func TestListScheduledWorkflowsReturnsOneCurrentVersionPerName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	scheduledName := fmt.Sprintf("sched-%d", time.Now().UnixNano())
	plainName := fmt.Sprintf("plain-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		s.pool.Exec(ctx, `DELETE FROM workflows WHERE name IN ($1, $2)`, scheduledName, plainName)
	})

	// Two versions of a scheduled workflow, the second with a different spec.
	v1 := workflow.WorkflowDef{
		Name:     scheduledName,
		Schedule: "0 6 * * *",
		Tasks:    []workflow.TaskDef{{ID: "a", Handler: "h"}},
	}
	if _, _, err := s.CreateWorkflow(ctx, v1); err != nil {
		t.Fatalf("create v1: %v", err)
	}
	v2 := v1
	v2.Schedule = "*/5 * * * *"
	v2.Tasks = []workflow.TaskDef{{ID: "a", Handler: "h"}, {ID: "b", Handler: "h"}}
	if _, _, err := s.CreateWorkflow(ctx, v2); err != nil {
		t.Fatalf("create v2: %v", err)
	}

	// And one with no schedule at all, which the scheduler must never pick up.
	if _, _, err := s.CreateWorkflow(ctx, workflow.WorkflowDef{
		Name:  plainName,
		Tasks: []workflow.TaskDef{{ID: "a", Handler: "h"}},
	}); err != nil {
		t.Fatalf("create unscheduled: %v", err)
	}

	got, err := s.ListScheduledWorkflows(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var mine []ScheduledWorkflow
	for _, wf := range got {
		if wf.Name == plainName {
			t.Error("an unscheduled workflow was returned to the scheduler")
		}
		if wf.Name == scheduledName {
			mine = append(mine, wf)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("%q appears %d times, want once", scheduledName, len(mine))
	}
	if mine[0].Schedule != "*/5 * * * *" {
		t.Errorf("schedule = %q, want the newest version's", mine[0].Schedule)
	}
	// The definition rides along, because the scheduler materializes runs from it
	// without a second lookup.
	if len(mine[0].Def.Tasks) != 2 {
		t.Errorf("definition has %d tasks, want the newest version's 2", len(mine[0].Def.Tasks))
	}
	if mine[0].CreatedAt.IsZero() {
		t.Error("created_at is zero; it is the baseline for the first slot")
	}
}

// Two registrations of the same name race to the same version number. The unique
// constraint is what makes one of them lose, and the loser has to surface as an
// error the API can turn into a retry rather than a silently dropped definition.
func TestConcurrentRegistrationsOfOneNameDoNotBothWin(t *testing.T) {
	s := newTestStore(t)
	name := fmt.Sprintf("race-%d", time.Now().UnixNano())
	def := workflow.WorkflowDef{
		Name:  name,
		Tasks: []workflow.TaskDef{{ID: "a", Handler: "h"}},
	}
	t.Cleanup(func() {
		s.pool.Exec(context.Background(), `DELETE FROM workflows WHERE name = $1`, name)
	})

	const racers = 6
	var wg sync.WaitGroup
	versions := make([]int, racers)
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, versions[i], errs[i] = s.CreateWorkflow(context.Background(), def)
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for i, err := range errs {
		if err != nil {
			continue // the constraint rejected this one, which is the intended outcome
		}
		if seen[versions[i]] {
			t.Errorf("version %d was handed out twice", versions[i])
		}
		seen[versions[i]] = true
	}
	if len(seen) == 0 {
		t.Fatal("every concurrent registration failed; at least one must win")
	}
}
