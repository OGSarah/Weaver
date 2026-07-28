package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"weaver/internal/workflow"
)

// TestRunHistorySpansVersionsAndCountsStatuses covers the two things the history
// query decides that are not obvious from its result: that history follows a
// workflow's *name* rather than the exact version row asked for, and that the task
// counts break down by status and add up.
func TestRunHistorySpansVersionsAndCountsStatuses(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	name := fmt.Sprintf("history-%d", time.Now().UnixNano())
	def := workflow.WorkflowDef{
		Name: name,
		Tasks: []workflow.TaskDef{
			{ID: "ok", Handler: "h"},
			{ID: "doomed", Handler: "h"},
			{ID: "blocked", Handler: "h", DependsOn: []string{"doomed"}},
		},
	}

	// Version 1, with a run driven to a mixed final state: one succeeded, one dead,
	// one left pending behind the dead task.
	v1ID, _, err := s.CreateWorkflow(ctx, def)
	if err != nil {
		t.Fatalf("create workflow v1: %v", err)
	}
	runV1, err := s.CreateRun(ctx, v1ID, def)
	if err != nil {
		t.Fatalf("create run v1: %v", err)
	}

	// Version 2 of the same name, with its own untouched run.
	v2ID, version, err := s.CreateWorkflow(ctx, def)
	if err != nil {
		t.Fatalf("create workflow v2: %v", err)
	}
	if version != 2 {
		t.Fatalf("want version 2, got %d", version)
	}
	runV2, err := s.CreateRun(ctx, v2ID, def)
	if err != nil {
		t.Fatalf("create run v2: %v", err)
	}
	t.Cleanup(func() {
		s.pool.Exec(ctx, `DELETE FROM runs WHERE id = ANY($1)`, []string{runV1, runV2})
		s.pool.Exec(ctx, `DELETE FROM workflows WHERE id = ANY($1)`, []string{v1ID, v2ID})
	})

	// Drive v1's run: succeed "ok", kill "doomed" so "blocked" never unblocks.
	for i := 0; i < 2; i++ {
		ct := claimOne(t, s, "w1", runV1)
		if ct.Name == "doomed" {
			if err := s.FailTask(ctx, ct, "w1", "boom"); err != nil {
				t.Fatalf("fail doomed: %v", err)
			}
		} else if err := s.CompleteTask(ctx, ct, "w1"); err != nil {
			t.Fatalf("complete %s: %v", ct.Name, err)
		}
	}

	// Asking with the v1 id must still return the v2 run: history follows the name.
	hist, err := s.ListRunHistory(ctx, v1ID, 10)
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	byRun := make(map[string]RunHistoryEntry, len(hist))
	for _, e := range hist {
		byRun[e.ID] = e
	}
	if _, ok := byRun[runV2]; !ok {
		t.Errorf("history queried by the v1 id should include the v2 run; got %d entries", len(hist))
	}

	v1 := byRun[runV1]
	if v1.WorkflowVersion != 1 {
		t.Errorf("v1 run: want version 1, got %d", v1.WorkflowVersion)
	}
	// "doomed" has no retries, so its single failure is terminal.
	want := map[string]int{"succeeded": 1, "dead": 1, "pending": 1}
	for status, n := range want {
		if v1.TaskCounts[status] != n {
			t.Errorf("v1 run: want %d %s, got %d (counts %v)", n, status, v1.TaskCounts[status], v1.TaskCounts)
		}
	}
	// A breakdown that does not add up to the total is worse than no breakdown.
	sum := 0
	for _, n := range v1.TaskCounts {
		sum += n
	}
	if sum != v1.TaskCount {
		t.Errorf("counts %v sum to %d, want taskCount %d", v1.TaskCounts, sum, v1.TaskCount)
	}

	// The untouched run reports the state CreateRun leaves behind: its two roots
	// Ready and the task behind "doomed" still Pending. Not all-pending, because
	// marking roots ready is part of creating the run.
	v2 := byRun[runV2]
	if v2.TaskCounts["ready"] != 2 || v2.TaskCounts["pending"] != 1 || v2.TaskCount != 3 {
		t.Errorf("v2 run: want 2 ready + 1 pending of 3, got %v of %d", v2.TaskCounts, v2.TaskCount)
	}

	// Newest first.
	if len(hist) >= 2 && hist[0].CreatedAt.Before(hist[1].CreatedAt) {
		t.Errorf("history should be newest first, got %s then %s", hist[0].CreatedAt, hist[1].CreatedAt)
	}
}

func TestCreateRunMarksRootsReady(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// A unique name per run keeps the workflows name/version unique constraint
	// from tripping when the suite runs twice against the same database; seedRun
	// registers cleanup so neither the run nor the workflow row is left behind.
	runID := seedRun(t, s, workflow.WorkflowDef{
		Name: fmt.Sprintf("diamond-%d", time.Now().UnixNano()),
		Tasks: []workflow.TaskDef{
			{ID: "extract", Handler: "extractData"},
			{ID: "transform", Handler: "transformData", DependsOn: []string{"extract"}},
			{ID: "validate", Handler: "validateData", DependsOn: []string{"extract"}},
			{ID: "load", Handler: "loadWarehouse", DependsOn: []string{"transform", "validate"}},
		},
	})
	t.Logf("run id: %s", runID)

	state, err := s.GetRunState(ctx, runID)
	if err != nil {
		t.Fatalf("get run state: %v", err)
	}

	if len(state.Tasks) != 4 {
		t.Fatalf("want 4 tasks, got %d", len(state.Tasks))
	}

	want := map[string]string{
		"extract":   "ready",
		"transform": "pending",
		"validate":  "pending",
		"load":      "pending",
	}
	for _, task := range state.Tasks {
		if got := task.Status; got != want[task.Name] {
			t.Errorf("task %s: want status %q, got %q", task.Name, want[task.Name], got)
		}
		if task.StartedAt != nil {
			t.Errorf("task %s should not have started yet", task.Name)
		}
	}
}
