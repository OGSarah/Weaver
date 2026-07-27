package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrLeaseLost means the worker's lease on the task was already gone when it went
// to record a result: the lease expired and the reaper (Phase 6) handed the task
// to someone else. The result is dropped rather than written on top of whoever
// holds the task now -- the price of at-least-once execution, and the reason
// handlers must be idempotent.
var ErrLeaseLost = errors.New("lease no longer held by this worker")

const (
	// Base delay before the first retry. Each further attempt doubles it.
	baseBackoff = 1 * time.Second
	// Ceiling so a task with many attempts does not wait absurdly long.
	maxBackoff = 5 * time.Minute
)

// CompleteTask records a successful task run in one transaction: it releases the
// lease, marks the task Succeeded, promotes any downstream task whose upstreams
// have now all succeeded, and marks the run Succeeded once every task is done.
//
// Doing all of it atomically is what keeps completion and unblocking in step: a
// crash after "succeeded" but before "downstream ready" is impossible, so a run
// can never stall with a finished parent and a forever-pending child.
func (s *Store) CompleteTask(ctx context.Context, task *ClaimedTask, workerID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	held, err := releaseLease(ctx, tx, task.ID, workerID)
	if err != nil {
		return err
	}
	if !held {
		return ErrLeaseLost
	}

	// Guard on status = 'running': we still hold the lease, so the row is ours,
	// but scoping the write to the running state keeps it honest against any
	// future path that could move the row out from under us.
	_, err = tx.Exec(ctx,
		`UPDATE tasks
		    SET status = 'succeeded', finished_at = now(), error = NULL
		  WHERE id = $1 AND status = 'running'`,
		task.ID,
	)
	if err != nil {
		return fmt.Errorf("mark task %s succeeded: %w", task.ID, err)
	}

	if _, err := markReadyTasks(ctx, tx, task.RunID); err != nil {
		return err
	}

	if err := markRunSucceededIfComplete(ctx, tx, task.RunID); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit completion: %w", err)
	}
	return nil
}

// FailTask records a failed task run in one transaction. If attempts remain it
// releases the lease and returns the task to Ready with a backoff delay, so the
// same row becomes claimable again once its scheduled_at passes. If attempts are
// exhausted it marks the task Dead and fails the run, since a dead task means the
// run can never fully succeed.
//
// cause is the error (or timeout, or panic) message, recorded on the task so the
// UI and logs can show why it failed.
func (s *Store) FailTask(ctx context.Context, task *ClaimedTask, workerID, cause string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	held, err := releaseLease(ctx, tx, task.ID, workerID)
	if err != nil {
		return err
	}
	if !held {
		return ErrLeaseLost
	}

	// task.Attempt is the attempt that just ran, incremented at claim time. So
	// attempt 3 of max 3 has no attempts left.
	if task.Attempt < task.MaxAttempts {
		// Push the task into the future rather than retrying instantly: a
		// transient failure gets time to clear, and a stampede of tasks that
		// failed together is spread out by the jitter in backoffDelay.
		delay := backoffDelay(task.Attempt)
		_, err = tx.Exec(ctx,
			`UPDATE tasks
			    SET status = 'ready',
			        error = $2,
			        started_at = NULL,
			        scheduled_at = now() + make_interval(secs => $3)
			  WHERE id = $1 AND status = 'running'`,
			task.ID, cause, delay.Seconds(),
		)
		if err != nil {
			return fmt.Errorf("schedule retry for task %s: %w", task.ID, err)
		}
	} else {
		_, err = tx.Exec(ctx,
			`UPDATE tasks
			    SET status = 'dead', finished_at = now(), error = $2
			  WHERE id = $1 AND status = 'running'`,
			task.ID, cause,
		)
		if err != nil {
			return fmt.Errorf("mark task %s dead: %w", task.ID, err)
		}
		if err := markRunFailed(ctx, tx, task.RunID); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit failure: %w", err)
	}
	return nil
}

// releaseLease deletes this worker's lease on the task and reports whether there
// was one to delete. A missing lease means the worker no longer owns the task --
// its lease expired and the reaper handed the task on -- so the caller must drop
// whatever result it was about to write.
func releaseLease(ctx context.Context, tx pgx.Tx, taskID, workerID string) (bool, error) {
	tag, err := tx.Exec(ctx,
		`DELETE FROM leases WHERE task_id = $1 AND worker_id = $2`,
		taskID, workerID,
	)
	if err != nil {
		return false, fmt.Errorf("release lease for task %s: %w", taskID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// markRunSucceededIfComplete flips the run to Succeeded once none of its tasks are
// in any state other than succeeded. A single dead task leaves a non-succeeded
// row behind forever, so this can never fire on a run that had a task die.
func markRunSucceededIfComplete(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := tx.Exec(ctx,
		`UPDATE runs
		    SET status = 'succeeded', finished_at = now()
		  WHERE id = $1
		    AND status NOT IN ('succeeded', 'failed', 'cancelled')
		    AND NOT EXISTS (
		        SELECT 1 FROM tasks
		         WHERE run_id = $1 AND status <> 'succeeded'
		    )`,
		runID,
	)
	if err != nil {
		return fmt.Errorf("mark run %s succeeded: %w", runID, err)
	}
	return nil
}

// markRunFailed flips the run to Failed, unless it has already reached a terminal
// state. The guard keeps a second dying task from clobbering finished_at.
func markRunFailed(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := tx.Exec(ctx,
		`UPDATE runs
		    SET status = 'failed', finished_at = now()
		  WHERE id = $1 AND status NOT IN ('succeeded', 'failed', 'cancelled')`,
		runID,
	)
	if err != nil {
		return fmt.Errorf("mark run %s failed: %w", runID, err)
	}
	return nil
}

// backoffDelay returns how long a task should wait before its next attempt:
// exponential in the attempt number (base, 2*base, 4*base, ...) capped at
// maxBackoff, then full-jittered to a random point in [d/2, d]. attempt is the
// attempt that just failed, so the first retry waits about baseBackoff.
func backoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 30 { // keep the shift below from overflowing int64
		attempt = 30
	}
	d := baseBackoff << (attempt - 1)
	if d <= 0 || d > maxBackoff {
		d = maxBackoff
	}
	// Full jitter: half the delay plus a random slice of the other half.
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}
