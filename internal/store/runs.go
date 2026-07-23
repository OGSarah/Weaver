package store

import (
	"context"
	"fmt"

	"weaver/internal/workflow"
)

const defaultTimeoutSeconds = 300

// CreateRun materializes a workflow definition into a run: one runs row, one
// tasks row per task, and one dependencies row per edge. Everything starts
// pending. All of it happens in one transaction so a crash midway leaves no
// half-built run.
func (s *Store) CreateRun(ctx context.Context, workflowID string, def workflow.WorkflowDef) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	// Rolls back unless Commit already succeeded, in which case it is a no-op.
	defer tx.Rollback(ctx)

	var runID string
	err = tx.QueryRow(ctx,
		`INSERT INTO runs (workflow_id, status)
		 VALUES ($1, 'pending')
		 RETURNING id`,
		workflowID,
	).Scan(&runID)
	if err != nil {
		return "", fmt.Errorf("insert run: %w", err)
	}

	// Task name -> generated UUID, so DependsOn names resolve to real IDs.
	taskIDs := make(map[string]string, len(def.Tasks))

	for _, t := range def.Tasks {
		timeout := t.TimeoutSeconds
		if timeout == 0 {
			timeout = defaultTimeoutSeconds
		}

		var taskID string
		err = tx.QueryRow(ctx,
			`INSERT INTO tasks (run_id, name, handler, max_attempts, timeout_seconds, status)
			 VALUES ($1, $2, $3, $4, $5, 'pending')
			 RETURNING id`,
			// Retries counts extra attempts, so max_attempts is one more.
			runID, t.ID, t.Handler, t.Retries+1, timeout,
		).Scan(&taskID)
		if err != nil {
			return "", fmt.Errorf("insert task %q: %w", t.ID, err)
		}
		taskIDs[t.ID] = taskID
	}

	// Second pass: every task row now exists, so edges can reference them.
	for _, t := range def.Tasks {
		for _, upstreamName := range t.DependsOn {
			upstreamID, ok := taskIDs[upstreamName]
			if !ok {
				return "", fmt.Errorf("task %q depends on unknown task %q", t.ID, upstreamName)
			}
			_, err = tx.Exec(ctx,
				`INSERT INTO dependencies (run_id, upstream_task_id, downstream_task_id)
				 VALUES ($1, $2, $3)`,
				runID, upstreamID, taskIDs[t.ID],
			)
			if err != nil {
				return "", fmt.Errorf("insert edge %q -> %q: %w", upstreamName, t.ID, err)
			}
		}
	}

	// Roots are the tasks with no incoming edges. Deriving this from the
	// dependencies rows we just inserted (rather than from DependsOn) means
	// readiness can never disagree with the edges actually stored.
	_, err = tx.Exec(ctx,
		`UPDATE tasks
		    SET status = 'ready'
		  WHERE run_id = $1
		    AND NOT EXISTS (
		        SELECT 1 FROM dependencies d
		         WHERE d.downstream_task_id = tasks.id
		    )`,
		runID,
	)
	if err != nil {
		return "", fmt.Errorf("mark roots ready: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return runID, nil
}
