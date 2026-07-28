package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Heartbeat extends the lease on a task this worker still holds, pushing its
// expires_at ttl into the future. It is how a live worker says "still working"
// so the reaper leaves the task alone.
//
// The bool reports whether the lease was still the caller's to extend. A false
// means the row was gone -- the lease already expired and the reaper handed the
// task to someone else -- so the worker is finishing work it no longer owns and
// should expect its eventual result to be dropped.
func (s *Store) Heartbeat(ctx context.Context, taskID, workerID string, ttl time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE leases
		    SET expires_at = now() + make_interval(secs => $3)
		  WHERE task_id = $1 AND worker_id = $2`,
		taskID, workerID, ttl.Seconds(),
	)
	if err != nil {
		return false, fmt.Errorf("heartbeat lease for task %s: %w", taskID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// expiredLease is one Running task whose worker stopped heartbeating, gathered by
// the reaper before it decides each task's fate.
type expiredLease struct {
	taskID      string
	runID       string
	attempt     int
	maxAttempts int
	runStatus   string
}

// ReapExpiredLeases recovers tasks stranded by dead workers. It finds every
// Running task whose lease has expired -- the worker holding it stopped
// heartbeating, so it is presumed gone -- and either hands the task back for
// another attempt or, if it has no attempts left, marks it Dead.
//
// The attempt check is what stops a genuinely poisonous task (one that reliably
// kills whatever worker claims it) from being reclaimed forever: each claim has
// already incremented the attempt count, so once it reaches max_attempts the
// reaper lets the task die instead of requeuing it. Returns the counts requeued
// and closed out, the latter covering both a task with no attempts left and one
// whose run has already finished or been cancelled.
//
// Everything runs in one transaction. FOR UPDATE OF l SKIP LOCKED locks only the
// lease rows and steps over any a worker is actively completing, so the reaper
// and a just-in-time finisher never fight over the same task: whoever commits
// first wins and the other sees its lease already gone.
func (s *Store) ReapExpiredLeases(ctx context.Context) (requeued, killed int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx,
		`SELECT l.task_id, t.run_id, t.attempt, t.max_attempts, r.status
		   FROM leases l
		   JOIN tasks t ON t.id = l.task_id
		   JOIN runs  r ON r.id = t.run_id
		  WHERE l.expires_at < now()
		    AND t.status = 'running'
		  FOR UPDATE OF l SKIP LOCKED`,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("select expired leases: %w", err)
	}

	// Drain the cursor before issuing more statements on this transaction: pgx
	// holds the connection while rows are open, so collect first, then act.
	var expired []expiredLease
	for rows.Next() {
		var e expiredLease
		if err := rows.Scan(&e.taskID, &e.runID, &e.attempt, &e.maxAttempts, &e.runStatus); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("scan expired lease: %w", err)
		}
		expired = append(expired, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate expired leases: %w", err)
	}

	for _, e := range expired {
		// A run that is no longer pending or running does not want this work back.
		// The case that matters is cancellation: cancelling stops every task that
		// is not executing, but a running one is left to finish because a handler
		// cannot be stopped. If its worker then dies, requeuing the task would make
		// it claimable again and a cancelled run would carry on executing -- the
		// exact outcome the cancel was for. So the task ends where the rest of its
		// run already is.
		if e.runStatus != "pending" && e.runStatus != "running" {
			if err := reapCancel(ctx, tx, e); err != nil {
				return 0, 0, err
			}
			killed++
			continue
		}

		// e.attempt is the attempt that was running when the worker died. If it
		// is below the ceiling another attempt is allowed, so hand the task back;
		// otherwise the retries are spent and the task dies here.
		if e.attempt < e.maxAttempts {
			if err := reapRequeue(ctx, tx, e); err != nil {
				return 0, 0, err
			}
			requeued++
		} else {
			if err := reapKill(ctx, tx, e); err != nil {
				return 0, 0, err
			}
			killed++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit reap: %w", err)
	}
	return requeued, killed, nil
}

// reapRequeue hands a stranded task back for another attempt and drops its dead
// lease, in the reaper's transaction. started_at is cleared so the next attempt
// times itself from scratch. The error note records why it came back, for the UI
// and logs; a later success clears it.
//
// The status is Failed, the same as a task whose handler returned an error, because
// the two mean the same thing: an attempt happened and did not finish, and another
// is coming. That keeps one meaning for each state -- Ready is "never attempted
// since it was unblocked", Failed is "an attempt is behind it" -- so a task's status
// alone says which it is, with no need to read the error column to find out.
//
// Unlike a handler failure this gets no backoff: scheduled_at = now() makes it
// claimable immediately. A dead worker is not evidence that the task itself is
// slow to settle, so there is nothing to wait out. The attempt ceiling is what
// stops a task that reliably kills workers from cycling forever.
func reapRequeue(ctx context.Context, tx pgx.Tx, e expiredLease) error {
	_, err := tx.Exec(ctx,
		`UPDATE tasks
		    SET status = 'failed',
		        started_at = NULL,
		        scheduled_at = now(),
		        error = 'worker lease expired; requeued by reaper'
		  WHERE id = $1 AND status = 'running'`,
		e.taskID,
	)
	if err != nil {
		return fmt.Errorf("requeue reaped task %s: %w", e.taskID, err)
	}
	// The worker that was running this attempt died without writing anything, so
	// this line is the only record of how the attempt ended. Without it the log
	// would jump from "attempt 2 started" straight to "attempt 3 started" with no
	// explanation of what happened in between.
	if err := appendTaskLogTx(ctx, tx, e.taskID, e.attempt, LogError,
		fmt.Sprintf("attempt %d abandoned: worker lease expired, requeued by reaper", e.attempt),
	); err != nil {
		return err
	}
	if err := deleteLease(ctx, tx, e.taskID); err != nil {
		return err
	}
	return nil
}

// reapCancel closes out a stranded task whose run has already reached a terminal
// state. Its attempt was abandoned and nothing further will run for this run, so
// the task lands cancelled rather than Failed (which is claimable) or Dead (which
// would report a deliberate stop as a failure). The run's own status is left
// exactly as it is: it already said how the run ended.
func reapCancel(ctx context.Context, tx pgx.Tx, e expiredLease) error {
	_, err := tx.Exec(ctx,
		`UPDATE tasks
		    SET status = 'cancelled',
		        finished_at = now(),
		        error = 'worker lease expired; run is no longer active'
		  WHERE id = $1 AND status = 'running'`,
		e.taskID,
	)
	if err != nil {
		return fmt.Errorf("cancel reaped task %s: %w", e.taskID, err)
	}
	if err := appendTaskLogTx(ctx, tx, e.taskID, e.attempt, LogError,
		fmt.Sprintf("attempt %d abandoned: worker lease expired and the run is no longer active", e.attempt),
	); err != nil {
		return err
	}
	return deleteLease(ctx, tx, e.taskID)
}

// reapKill marks a stranded task Dead once its attempts are spent, fails its run,
// and drops the lease -- the same terminal outcome as exhausting retries through
// FailTask, reached because the worker died instead of returning an error.
func reapKill(ctx context.Context, tx pgx.Tx, e expiredLease) error {
	_, err := tx.Exec(ctx,
		`UPDATE tasks
		    SET status = 'dead',
		        finished_at = now(),
		        error = 'worker lease expired; retries exhausted'
		  WHERE id = $1 AND status = 'running'`,
		e.taskID,
	)
	if err != nil {
		return fmt.Errorf("kill reaped task %s: %w", e.taskID, err)
	}
	if err := appendTaskLogTx(ctx, tx, e.taskID, e.attempt, LogError,
		fmt.Sprintf("attempt %d abandoned: worker lease expired, no attempts left; giving up", e.attempt),
	); err != nil {
		return err
	}
	if err := markRunFailed(ctx, tx, e.runID); err != nil {
		return err
	}
	if err := deleteLease(ctx, tx, e.taskID); err != nil {
		return err
	}
	return nil
}

// deleteLease removes a lease by task id. Unlike releaseLease it does not check a
// worker id: the reaper reclaims a lease precisely because its worker is gone.
func deleteLease(ctx context.Context, tx pgx.Tx, taskID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM leases WHERE task_id = $1`, taskID); err != nil {
		return fmt.Errorf("delete lease for task %s: %w", taskID, err)
	}
	return nil
}
