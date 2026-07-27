package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"weaver/internal/workflow"
)

// TestClaimTaskNoDoubleClaim is the Phase 4 proof: many workers hammering the
// same queue table divide the work with no task ever going to two workers. It
// exercises the real claim path (FOR UPDATE SKIP LOCKED) under contention.
func TestClaimTaskNoDoubleClaim(t *testing.T) {
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

	const (
		numTasks   = 50
		numWorkers = 8
	)

	// Every task is a root (no dependencies), so all land Ready at once and the
	// workers have a full queue to fight over.
	tasks := make([]workflow.TaskDef, numTasks)
	for i := range tasks {
		tasks[i] = workflow.TaskDef{ID: fmt.Sprintf("t-%03d", i), Handler: "demoTask"}
	}
	def := workflow.WorkflowDef{
		Name:  fmt.Sprintf("claim-test-%d", time.Now().UnixNano()),
		Tasks: tasks,
	}

	workflowID, err := s.CreateWorkflow(ctx, def)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	runID, err := s.CreateRun(ctx, workflowID, def)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	// Cascade order: deleting the run clears tasks, deps and leases; then the
	// workflow can go.
	t.Cleanup(func() {
		s.pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, runID)
		s.pool.Exec(ctx, `DELETE FROM workflows WHERE id = $1`, workflowID)
	})

	// claims maps a claimed task id to the worker that took it. A second write
	// to an existing key is a double claim -- the exact bug this test guards.
	// A worker claims across every run, so we scope to this run's tasks: stray
	// Ready rows from other runs are real work, just not ours to assert about.
	var mu sync.Mutex
	claims := make(map[string]string, numTasks)
	var doubleClaims []string

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID string) {
			defer wg.Done()
			// Drain until a poll comes back empty: every remaining Ready task is
			// then either taken or locked by a peer who will take it.
			for {
				ct, err := s.ClaimTask(ctx, workerID, 30*time.Second)
				if err != nil {
					t.Errorf("worker %s claim: %v", workerID, err)
					return
				}
				if ct == nil {
					return
				}
				if ct.RunID != runID {
					continue // a task from some other run; not under test
				}
				mu.Lock()
				if prev, seen := claims[ct.ID]; seen {
					doubleClaims = append(doubleClaims,
						fmt.Sprintf("task %s claimed by both %s and %s", ct.ID, prev, workerID))
				}
				claims[ct.ID] = workerID
				if ct.Attempt != 1 {
					t.Errorf("task %s: first claim should be attempt 1, got %d", ct.ID, ct.Attempt)
				}
				mu.Unlock()
			}
		}(fmt.Sprintf("worker-%d", w))
	}
	wg.Wait()

	if len(doubleClaims) > 0 {
		for _, d := range doubleClaims {
			t.Error(d)
		}
	}
	if len(claims) != numTasks {
		t.Fatalf("want %d distinct tasks claimed, got %d", numTasks, len(claims))
	}

	// Every task should now be Running, and each should carry exactly one lease
	// held by the worker our map says claimed it.
	var running int
	err = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE run_id = $1 AND status = 'running'`, runID,
	).Scan(&running)
	if err != nil {
		t.Fatalf("count running: %v", err)
	}
	if running != numTasks {
		t.Errorf("want %d running tasks, got %d", numTasks, running)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT l.task_id, l.worker_id
		   FROM leases l
		   JOIN tasks t ON t.id = l.task_id
		  WHERE t.run_id = $1`, runID)
	if err != nil {
		t.Fatalf("query leases: %v", err)
	}
	defer rows.Close()

	leaseCount := 0
	for rows.Next() {
		var taskID, leaseWorker string
		if err := rows.Scan(&taskID, &leaseWorker); err != nil {
			t.Fatalf("scan lease: %v", err)
		}
		leaseCount++
		if claims[taskID] != leaseWorker {
			t.Errorf("task %s: lease held by %s but claimed by %s", taskID, leaseWorker, claims[taskID])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate leases: %v", err)
	}
	if leaseCount != numTasks {
		t.Errorf("want %d leases, got %d", numTasks, leaseCount)
	}
}
