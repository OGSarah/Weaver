package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"weaver/internal/workflow"
)

// TestCreateScheduledRunIsExactlyOncePerSlot is the distributed-scheduling
// guarantee: when many scheduler instances all notice the same slot has come due
// and race to create its run, exactly one wins. Everyone else's insert hits the
// (workflow_id, scheduled_for) unique index and returns created=false without
// building a duplicate run. This is what makes running more than one scheduler
// safe.
func TestCreateScheduledRunIsExactlyOncePerSlot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	def := workflow.WorkflowDef{
		Name:     fmt.Sprintf("sched-%d", time.Now().UnixNano()),
		Schedule: "0 6 * * *",
		Tasks: []workflow.TaskDef{
			{ID: "extract", Handler: "extractData"},
			{ID: "load", Handler: "loadWarehouse", DependsOn: []string{"extract"}},
		},
	}
	workflowID, _, err := s.CreateWorkflow(ctx, def)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Cleanup(func() {
		s.pool.Exec(ctx, `DELETE FROM runs WHERE workflow_id = $1`, workflowID)
		s.pool.Exec(ctx, `DELETE FROM workflows WHERE id = $1`, workflowID)
	})

	// A fixed slot every racer targets, standing in for "the 06:00 run came due".
	slot := time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC)

	const racers = 8
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		createdCnt int
		runIDs     = map[string]struct{}{}
		start      = make(chan struct{})
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all goroutines together to maximize contention
			runID, created, err := s.CreateScheduledRun(ctx, workflowID, def, slot)
			if err != nil {
				t.Errorf("create scheduled run: %v", err)
				return
			}
			if created {
				mu.Lock()
				createdCnt++
				runIDs[runID] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if createdCnt != 1 {
		t.Fatalf("want exactly 1 scheduled run created, got %d", createdCnt)
	}

	// And the database itself holds exactly one run for that slot.
	var runs int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM runs WHERE workflow_id = $1 AND scheduled_for = $2`,
		workflowID, slot,
	).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("want 1 run row for the slot, got %d", runs)
	}

	// LastScheduledSlot should now report that slot as the baseline for the next
	// due computation.
	last, err := s.LastScheduledSlot(ctx, workflowID)
	if err != nil {
		t.Fatalf("last scheduled slot: %v", err)
	}
	if last == nil || !last.Equal(slot) {
		t.Fatalf("want last scheduled slot %s, got %v", slot, last)
	}
}
