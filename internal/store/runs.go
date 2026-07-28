package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"weaver/internal/workflow"
)

const defaultTimeoutSeconds = 300

// maxRunHistory caps a single history page. History grows without bound on a
// scheduled workflow, so an unbounded query would eventually try to return years of
// it to a browser.
const maxRunHistory = 50

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
	DependsOn   []string   // upstream task names, so the UI can draw the run's DAG
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

// RunHistoryEntry is one row of a workflow's run history: enough to list and pick
// a run without loading its tasks.
type RunHistoryEntry struct {
	ID              string
	WorkflowVersion int
	Status          string
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
	TaskCount       int
	// TaskCounts is status -> how many of this run's tasks are in it. A map rather
	// than a column per status, so adding a state to the schema does not mean
	// changing this struct, the query, and the JSON shape in step. Statuses with no
	// tasks are absent rather than zero.
	TaskCounts map[string]int
}

// ListRunHistory returns the most recent runs for the workflow named by the given
// id, newest first.
//
// It resolves that id to a workflow *name* and returns runs across every version of
// it, rather than only the exact version passed in. Registering a workflow again
// creates a new row with a new id, so scoping to one id would silently hide the
// history the moment a definition was edited -- which is exactly when comparing
// against previous runs is most useful. The version each run used comes back with
// it, so the ones that ran a different definition are still identifiable.
func (s *Store) ListRunHistory(ctx context.Context, workflowID string, limit int) ([]RunHistoryEntry, error) {
	if limit <= 0 || limit > maxRunHistory {
		limit = maxRunHistory
	}

	// The task counts come from one aggregate subquery rather than a join against
	// the outer query, so a run with twenty tasks still produces exactly one row.
	//
	// The inner query groups by status and the outer one folds those groups into a
	// single JSON object, which is what keeps this to one round trip: counting each
	// status separately would mean either a column per status or a second query per
	// run. A run whose tasks were all deleted yields no groups at all, so both
	// aggregates are coalesced to an empty result rather than NULL.
	rows, err := s.pool.Query(ctx,
		`SELECT r.id, w.version, r.status, r.created_at, r.started_at, r.finished_at,
		        coalesce(t.total, 0), coalesce(t.by_status, '{}'::jsonb)
		   FROM runs r
		   JOIN workflows w ON w.id = r.workflow_id
		   LEFT JOIN LATERAL (
		        SELECT coalesce(sum(g.n), 0)::int AS total,
		               jsonb_object_agg(g.status, g.n) AS by_status
		          FROM (
		               SELECT status, count(*) AS n
		                 FROM tasks
		                WHERE run_id = r.id
		                GROUP BY status
		          ) g
		   ) t ON true
		  WHERE w.name = (SELECT name FROM workflows WHERE id = $1)
		  ORDER BY r.created_at DESC
		  LIMIT $2`,
		workflowID, limit,
	)
	if err != nil {
		// History is a page, not a lookup, so an unparseable id answers the way an
		// unknown one does -- with nothing -- rather than with an error the caller
		// would have to handle separately for the same situation.
		if isMalformedID(err) {
			return []RunHistoryEntry{}, nil
		}
		return nil, fmt.Errorf("query run history: %w", err)
	}
	defer rows.Close()

	out := make([]RunHistoryEntry, 0, limit)
	for rows.Next() {
		var e RunHistoryEntry
		var byStatus []byte
		if err := rows.Scan(&e.ID, &e.WorkflowVersion, &e.Status, &e.CreatedAt,
			&e.StartedAt, &e.FinishedAt, &e.TaskCount, &byStatus); err != nil {
			return nil, fmt.Errorf("scan run history: %w", err)
		}
		if err := json.Unmarshal(byStatus, &e.TaskCounts); err != nil {
			return nil, fmt.Errorf("decode task counts: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		// pgx reports a rejected parameter when the rows are read rather than when
		// the query is sent, so the malformed-id case can surface at either point.
		if isMalformedID(err) {
			return []RunHistoryEntry{}, nil
		}
		return nil, fmt.Errorf("iterate run history: %w", err)
	}
	return out, nil
}

// GetRunState returns a run, all of its tasks, and the dependency edges between
// them. Three queries rather than one join, so run fields are not repeated on every
// task row and a task with several upstreams stays a single row.
func (s *Store) GetRunState(ctx context.Context, runID string) (*RunState, error) {
	var r RunState
	err := s.pool.QueryRow(ctx,
		`SELECT id, workflow_id, status, created_at, started_at, finished_at
		   FROM runs
		  WHERE id = $1`,
		runID,
	).Scan(&r.ID, &r.WorkflowID, &r.Status, &r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isMalformedID(err) {
			return nil, ErrNotFound
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

	// The edges. These come from the dependencies rows rather than from the
	// workflow definition on purpose: those rows are what the scheduler consults to
	// decide what is ready, so a graph drawn from them cannot disagree with what is
	// actually going to run.
	//
	// Upstreams are returned as task names because a name is what identifies a task
	// within a run (the UNIQUE (run_id, name) constraint), and it is stable across
	// runs, whereas the row's UUID is regenerated for every run.
	depRows, err := s.pool.Query(ctx,
		`SELECT down.name, up.name
		   FROM dependencies d
		   JOIN tasks up   ON up.id   = d.upstream_task_id
		   JOIN tasks down ON down.id = d.downstream_task_id
		  WHERE d.run_id = $1
		  ORDER BY down.name, up.name`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("query dependencies: %w", err)
	}
	defer depRows.Close()

	// Downstream task name -> the names it waits on.
	upstreams := make(map[string][]string)
	for depRows.Next() {
		var downstream, upstream string
		if err := depRows.Scan(&downstream, &upstream); err != nil {
			return nil, fmt.Errorf("scan dependency: %w", err)
		}
		upstreams[downstream] = append(upstreams[downstream], upstream)
	}
	if err := depRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dependencies: %w", err)
	}

	// Attach by index: ranging by value would copy each task and the assignment
	// would be thrown away.
	for i := range r.Tasks {
		r.Tasks[i].DependsOn = upstreams[r.Tasks[i].Name]
	}

	return &r, nil
}

// CreateRun materializes a workflow definition into a run: one runs row, one
// tasks row per task, and one dependencies row per edge. Everything starts
// pending. All of it happens in one transaction so a crash midway leaves no
// half-built run. scheduled_for is left NULL, marking this a manual or
// API-triggered run rather than one the scheduler produced for a cron slot.
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

	if err := materializeTasks(ctx, tx, runID, def); err != nil {
		return "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return runID, nil
}

// CreateScheduledRun creates a run for a specific cron slot, guarded against
// duplication across scheduler instances. The insert is ON CONFLICT DO NOTHING on
// (workflow_id, scheduled_for): if another scheduler already created the run for
// this slot, the insert matches no row, created is false, and no tasks are built.
// Otherwise it materializes the run exactly like a manual one. Returning created
// lets the caller tell "I made the run for this slot" from "someone beat me to it".
func (s *Store) CreateScheduledRun(ctx context.Context, workflowID string, def workflow.WorkflowDef, slot time.Time) (runID string, created bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx,
		`INSERT INTO runs (workflow_id, status, scheduled_for)
		 VALUES ($1, 'pending', $2)
		 ON CONFLICT (workflow_id, scheduled_for) DO NOTHING
		 RETURNING id`,
		workflowID, slot,
	).Scan(&runID)
	if err != nil {
		// No row back means the slot was already taken: not an error, just a
		// scheduler that lost the race.
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("insert scheduled run: %w", err)
	}

	if err := materializeTasks(ctx, tx, runID, def); err != nil {
		return "", false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("commit: %w", err)
	}
	return runID, true, nil
}

// materializeTasks inserts one task row per task and one dependencies row per edge
// for an already-created run, then marks the roots Ready. It runs inside the
// caller's transaction so the whole run is built atomically with the runs row.
func materializeTasks(ctx context.Context, tx pgx.Tx, runID string, def workflow.WorkflowDef) error {
	// Task name -> generated UUID, so DependsOn names resolve to real IDs.
	taskIDs := make(map[string]string, len(def.Tasks))

	for _, t := range def.Tasks {
		timeout := t.TimeoutSeconds
		if timeout == 0 {
			timeout = defaultTimeoutSeconds
		}

		var taskID string
		err := tx.QueryRow(ctx,
			`INSERT INTO tasks (run_id, name, handler, max_attempts, timeout_seconds, status)
			 VALUES ($1, $2, $3, $4, $5, 'pending')
			 RETURNING id`,
			// Retries counts extra attempts, so max_attempts is one more.
			runID, t.ID, t.Handler, t.Retries+1, timeout,
		).Scan(&taskID)
		if err != nil {
			return fmt.Errorf("insert task %q: %w", t.ID, err)
		}
		taskIDs[t.ID] = taskID
	}

	// Second pass: every task row now exists, so edges can reference them.
	for _, t := range def.Tasks {
		for _, upstreamName := range t.DependsOn {
			upstreamID, ok := taskIDs[upstreamName]
			if !ok {
				return fmt.Errorf("task %q depends on unknown task %q", t.ID, upstreamName)
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO dependencies (run_id, upstream_task_id, downstream_task_id)
				 VALUES ($1, $2, $3)`,
				runID, upstreamID, taskIDs[t.ID],
			)
			if err != nil {
				return fmt.Errorf("insert edge %q -> %q: %w", upstreamName, t.ID, err)
			}
		}
	}

	// Roots are the tasks with no incoming edges. Deriving this from the
	// dependencies rows we just inserted (rather than from DependsOn) means
	// readiness can never disagree with the edges actually stored.
	_, err := tx.Exec(ctx,
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
		return fmt.Errorf("mark roots ready: %w", err)
	}
	return nil
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
