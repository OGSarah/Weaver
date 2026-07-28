package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TaskDetail is a single task's full state, including its result payload, for the
// per-task endpoint. It is a superset of the TaskState the run view returns.
type TaskDetail struct {
	TaskState
	RunID          string
	TimeoutSeconds int
	Result         json.RawMessage // nil unless the handler stored one
	CreatedAt      time.Time
}

// GetTask returns one task within a run, with its result and error, or
// ErrNotFound if the run/task pair does not exist. Scoping by run_id as well as id
// keeps a task id from one run from being read through another run's URL.
func (s *Store) GetTask(ctx context.Context, runID, taskID string) (*TaskDetail, error) {
	var d TaskDetail
	var result []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, run_id, name, handler, status, attempt, max_attempts,
		        timeout_seconds, scheduled_at, created_at, started_at, finished_at,
		        result, error
		   FROM tasks
		  WHERE run_id = $1 AND id = $2`,
		runID, taskID,
	).Scan(
		&d.ID, &d.RunID, &d.Name, &d.Handler, &d.Status, &d.Attempt, &d.MaxAttempts,
		&d.TimeoutSeconds, &d.ScheduledAt, &d.CreatedAt, &d.StartedAt, &d.FinishedAt,
		&result, &d.Error,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isMalformedID(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query task: %w", err)
	}
	if result != nil {
		d.Result = json.RawMessage(result)
	}
	return &d, nil
}

// CancelRun cancels an in-flight run. It marks every task that is not currently
// executing (pending, ready, or failed and awaiting a retry) as cancelled so none
// of them is ever claimed, then flips the run to cancelled. Tasks already running
// are left alone: Go cannot forcibly stop a
// handler, so they finish on their own and their results are dropped, because the
// completion guards refuse to advance a run that is no longer pending or running.
// Returns ErrNotFound if the run does not exist, or ErrNotCancellable if it has
// already reached a terminal state.
func (s *Store) CancelRun(ctx context.Context, runID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Does the run exist at all? An unlocked read, because taking the run's row
	// lock here is what used to make this deadlock: ClaimTask locks a task row and
	// then updates the run, so a cancel that locked the run first and reached for
	// the tasks afterwards closed the cycle, and Postgres aborted one of the two
	// with a deadlock error -- a spurious 500 on a cancel, or a failed claim in a
	// worker. Every writer now takes tasks before runs.
	//
	// Nothing is decided from this read: a run that finishes between here and the
	// update below is caught by the update's own WHERE clause.
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isMalformedID(err) {
			return ErrNotFound
		}
		return fmt.Errorf("read run: %w", err)
	}
	if status != "pending" && status != "running" {
		return ErrNotCancellable
	}

	// Log the cancellation before the update, while the rows still carry the
	// statuses being cancelled. Same transaction, so the lines and the state change
	// are all-or-nothing, and the WHERE clause matches the update's exactly.
	_, err = tx.Exec(ctx,
		`INSERT INTO task_logs (task_id, attempt, level, message)
		 SELECT id, attempt, 'info', 'run cancelled; this task will not run'
		   FROM tasks
		  WHERE run_id = $1 AND status IN ('pending', 'ready', 'failed')`,
		runID,
	)
	if err != nil {
		return fmt.Errorf("log task cancellations: %w", err)
	}

	// Stop everything not currently executing from ever being claimed. 'failed'
	// belongs in this list because it is a waiting state, not a terminal one: a
	// failed task still has attempts left and would be picked up the moment its
	// backoff elapsed, running work for a run that was cancelled.
	_, err = tx.Exec(ctx,
		`UPDATE tasks
		    SET status = 'cancelled', finished_at = now(), error = 'run cancelled'
		  WHERE run_id = $1 AND status IN ('pending', 'ready', 'failed')`,
		runID,
	)
	if err != nil {
		return fmt.Errorf("cancel tasks: %w", err)
	}

	// The run row is taken last, and the state check lives in the WHERE clause
	// rather than in the read above. That is what makes the decision atomic now
	// that the read holds no lock: two concurrent cancels, or a cancel racing a
	// completion, both reach here, and exactly one matches a row. The loser sees
	// zero rows affected and reports the run as no longer cancellable rather than
	// overwriting a finished run's status and timestamp.
	tag, err := tx.Exec(ctx,
		`UPDATE runs
		    SET status = 'cancelled', finished_at = now()
		  WHERE id = $1 AND status IN ('pending', 'running')`,
		runID,
	)
	if err != nil {
		return fmt.Errorf("cancel run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Rolls back the task cancellations above: the run reached a terminal state
		// under us, so nothing about this cancel applies.
		return ErrNotCancellable
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit cancel: %w", err)
	}
	return nil
}

// ErrNotCancellable means a run cannot be cancelled because it has already
// finished (succeeded, failed, or was already cancelled).
var ErrNotCancellable = errors.New("run is not in a cancellable state")
