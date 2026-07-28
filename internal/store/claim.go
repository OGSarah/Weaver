package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ClaimedTask is everything a worker needs to run a task it has just claimed.
// It is a snapshot taken at claim time inside the claim transaction.
type ClaimedTask struct {
	ID             string
	RunID          string
	Name           string
	Handler        string
	Attempt        int // the attempt now starting (1 on the first run)
	MaxAttempts    int
	TimeoutSeconds int
}

// ClaimTask atomically claims the next runnable task for a worker, or returns
// (nil, nil) when the queue holds nothing this worker can take right now.
//
// The whole thing is one transaction so the three facts it establishes -- this
// row is mine, it is now running, and a lease proves I hold it -- either all
// become true together or none do. A crash between them leaves the task exactly
// as it was, still claimable by someone else.
//
// The select uses FOR UPDATE SKIP LOCKED: FOR UPDATE takes a row lock on the
// task it picks, and SKIP LOCKED tells Postgres to step over any row another
// worker's transaction has already locked rather than block on it. So a hundred
// workers can run this same query at once and each walks away with a different
// task (or with nothing), with no external lock service and no double claim.
func (s *Store) ClaimTask(ctx context.Context, workerID string, leaseTTL time.Duration) (*ClaimedTask, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	// Rolls back unless Commit already ran, in which case it is a no-op.
	defer tx.Rollback(ctx)

	// Two statuses are claimable, and the difference between them is history, not
	// eligibility: 'ready' has never been attempted since it was unblocked, 'failed'
	// has an attempt behind it and is waiting out its backoff. scheduled_at is what
	// actually gates both -- it is now() for a ready task and a point in the future
	// for a failed one -- so a retry becomes claimable the moment its delay elapses,
	// with nothing needed in between to promote it.
	var ct ClaimedTask
	err = tx.QueryRow(ctx,
		`SELECT id, run_id, name, handler, attempt, max_attempts, timeout_seconds
		   FROM tasks
		  WHERE status IN ('ready', 'failed')
		    AND scheduled_at <= now()
		  ORDER BY scheduled_at
		  FOR UPDATE SKIP LOCKED
		  LIMIT 1`,
	).Scan(&ct.ID, &ct.RunID, &ct.Name, &ct.Handler, &ct.Attempt, &ct.MaxAttempts, &ct.TimeoutSeconds)
	if err != nil {
		// Nothing claimable: the queue is empty, fully claimed, or everything left
		// is still waiting out a backoff. Not an error -- the caller polls again
		// shortly.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("select claimable task: %w", err)
	}

	// A new attempt is beginning; reflect it on the row and in what we return.
	ct.Attempt++

	_, err = tx.Exec(ctx,
		`UPDATE tasks
		    SET status = 'running',
		        attempt = $2,
		        started_at = now()
		  WHERE id = $1`,
		ct.ID, ct.Attempt,
	)
	if err != nil {
		return nil, fmt.Errorf("flip task %s running: %w", ct.ID, err)
	}

	// Open this attempt's entry in the task's log. Written in the claim
	// transaction so the line and the Running state appear together: there is no
	// instant where a task is running with nothing in its log to say who took it.
	if err := appendTaskLogTx(ctx, tx, ct.ID, ct.Attempt, LogInfo,
		fmt.Sprintf("attempt %d/%d started on worker %s", ct.Attempt, ct.MaxAttempts, workerID),
	); err != nil {
		return nil, err
	}

	// The lease is what makes the claim visible to the reaper (Phase 6). Its
	// task_id is the primary key, so a second lease for the same task is
	// impossible -- a structural backstop behind the row lock.
	_, err = tx.Exec(ctx,
		`INSERT INTO leases (task_id, worker_id, expires_at)
		 VALUES ($1, $2, now() + make_interval(secs => $3))`,
		ct.ID, workerID, leaseTTL.Seconds(),
	)
	if err != nil {
		return nil, fmt.Errorf("write lease for task %s: %w", ct.ID, err)
	}

	// First claim in a run moves it from pending to running and stamps its start.
	// coalesce keeps started_at pinned to the first task that ever ran.
	_, err = tx.Exec(ctx,
		`UPDATE runs
		    SET status = 'running', started_at = coalesce(started_at, now())
		  WHERE id = $1 AND status = 'pending'`,
		ct.RunID,
	)
	if err != nil {
		return nil, fmt.Errorf("mark run %s running: %w", ct.RunID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return &ct, nil
}
