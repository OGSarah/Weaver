package store

import (
	"context"
	"fmt"
	"errors"
	"time"

	"weaver/internal/workflow"
	"github.com/jackc/pgx/v5"
)

const defaultTimeoutSeconds = 300

// TaskState is one task's runtime state within a run.
type TaskState struct {
	ID          string
	Name        string
	Handler     string
	Status      string
	Attempt     int
	MaxAttempts int
	ScheduledAt time.Time
	StartedAt   *time.Time // nil until a worker claims it
	FinishedAt  *time.Time // nil while still in flight
	Error       *string    // nil unless the last attempt failed
}

// RunState is a run plus every task in it.
type RunState struct {
	ID         string
	WorkflowID string
	Status     string
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	Tasks      []TaskState
}

// GetRunState returns a run and all of its tasks. Two queries rather than a
// join, so run fields are not repeated on every task row.
func (s *Store) GetRunState(ctx context.Context, runID string) (*RunState, error) {
	var r RunState
	err := s.pool.QueryRow(ctx,
		`SELECT id, workflow_id, status, created_at, started_at, finished_at
		   FROM runs
		  WHERE id = $1`,
		runID,
	).Scan(&r.ID, &r.WorkflowID, &r.Status, &r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("run %s not found", runID)
		}
		return nil, fmt.Errorf("query run: %w", err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT id, name, handler, status, attempt, max_attempts,
		        scheduled_at, started_at, finished_at, error
		   FROM tasks
		  WHERE run_id = $1
		  ORDER BY name`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	// Must be closed or the connection is not returned to the pool.
	defer rows.Close()

	for rows.Next() {
		var t TaskState
		if err := rows.Scan(
			&t.ID, &t.Name, &t.Handler, &t.Status, &t.Attempt, &t.MaxAttempts,
			&t.ScheduledAt, &t.StartedAt, &t.FinishedAt, &t.Error,
		); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		r.Tasks = append(r.Tasks, t)
	}
	// Errors during iteration surface here, not from Scan.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}

	return &r, nil
}

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

// MarkReadyTasks promotes pending tasks whose upstreams have all succeeded.
// Returns how many were promoted. Call this inside the same transaction that
// marks a task succeeded, so completion and unblocking are atomic.
func markReadyTasks(ctx context.Context, tx pgx.Tx, runID string) (int64, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE tasks
		    SET status = 'ready'
		  WHERE run_id = $1
		    AND status = 'pending'
		    AND NOT EXISTS (
		        SELECT 1
		          FROM dependencies d
		          JOIN tasks up ON up.id = d.upstream_task_id
		         WHERE d.downstream_task_id = tasks.id
		           AND up.status <> 'succeeded'
		    )`,
		runID,
	)
	if err != nil {
		return 0, fmt.Errorf("mark ready tasks: %w", err)
	}
	return tag.RowsAffected(), nil
}