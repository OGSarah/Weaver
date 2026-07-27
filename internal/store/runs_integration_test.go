package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"weaver/internal/workflow"
)

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
