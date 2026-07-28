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
		if errors.Is(err, pgx.ErrNoRows) {
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

	// Lock the run row and check it is still cancellable in one step, so two
	// concurrent cancels (or a cancel racing a completion) resolve to one outcome.
	var status string
	err = tx.QueryRow(ctx,
		`SELECT status FROM runs WHERE id = $1 FOR UPDATE`,
		runID,
	).Scan(&status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lock run: %w", err)
	}
	if status != "pending" && status != "running" {
		return ErrNotCancellable
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

	_, err = tx.Exec(ctx,
		`UPDATE runs SET status = 'cancelled', finished_at = now() WHERE id = $1`,
		runID,
	)
	if err != nil {
		return fmt.Errorf("cancel run: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit cancel: %w", err)
	}
	return nil
}

// ErrNotCancellable means a run cannot be cancelled because it has already
// finished (succeeded, failed, or was already cancelled).
var ErrNotCancellable = errors.New("run is not in a cancellable state")
