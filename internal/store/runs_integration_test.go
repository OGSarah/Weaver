package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"weaver/internal/workflow"
)

func TestCreateRunMarksRootsReady(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	s, err := New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer s.Close()

	def := workflow.WorkflowDef{
		Name: "diamond-test",
		Tasks: []workflow.TaskDef{
			{ID: "extract", Handler: "extractData"},
			{ID: "transform", Handler: "transformData", DependsOn: []string{"extract"}},
			{ID: "validate", Handler: "validateData", DependsOn: []string{"extract"}},
			{ID: "load", Handler: "loadWarehouse", DependsOn: []string{"transform", "validate"}},
		},
	}

	// Insert the workflow row directly, since CreateWorkflow does not exist yet.
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var workflowID string
	err = s.pool.QueryRow(ctx,
		`INSERT INTO workflows (name, definition) VALUES ($1, $2) RETURNING id`,
		def.Name, raw,
	).Scan(&workflowID)
	if err != nil {
		t.Fatalf("insert workflow: %v", err)
	}

	runID, err := s.CreateRun(ctx, workflowID, def)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
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
