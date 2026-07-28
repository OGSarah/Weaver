package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Log levels. Deliberately two: a line is either the normal narrative of a task or
// the reason something went wrong, and anything finer is a filter nobody uses.
const (
	LogInfo  = "info"
	LogError = "error"
)

// defaultLogLimit caps how many lines a single read returns. A runaway handler can
// write an unbounded number of them, and neither the API nor a browser should be
// asked to carry all of it.
const defaultLogLimit = 500

// TaskLogLine is one recorded line.
type TaskLogLine struct {
	Attempt  int
	Level    string
	Message  string
	LoggedAt time.Time
}

// AppendTaskLog records one line against a task, outside any transaction. This is
// the path handlers take, through the worker's TaskLogger.
//
// Each line is its own INSERT rather than being buffered and flushed when the task
// finishes. That is chattier, and it is the right trade here: a buffered log is
// lost exactly when a worker is killed mid-task, which is the moment the log was
// worth keeping. It also means the UI can tail a running task instead of waiting
// for it to end.
func (s *Store) AppendTaskLog(ctx context.Context, taskID string, attempt int, level, message string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO task_logs (task_id, attempt, level, message)
		 VALUES ($1, $2, $3, $4)`,
		taskID, attempt, level, message,
	)
	if err != nil {
		return fmt.Errorf("append log for task %s: %w", taskID, err)
	}
	return nil
}

// appendTaskLogTx records a line inside the caller's transaction. The lifecycle
// lines use this so a line and the state change it describes commit together: a
// task can never be marked dead with no line saying why, and a rolled-back
// transition leaves no line claiming it happened.
func appendTaskLogTx(ctx context.Context, tx pgx.Tx, taskID string, attempt int, level, message string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO task_logs (task_id, attempt, level, message)
		 VALUES ($1, $2, $3, $4)`,
		taskID, attempt, level, message,
	)
	if err != nil {
		return fmt.Errorf("append log for task %s: %w", taskID, err)
	}
	return nil
}

// ListTaskLogs returns a task's lines oldest first, capped at defaultLogLimit.
//
// When a task has written more lines than the cap it is the most recent ones that
// matter (the failure is at the end, not the beginning), so the query takes the
// newest by id and the caller reverses them back into reading order. truncated
// reports whether anything was dropped, so the UI can say so rather than quietly
// presenting a partial log as complete.
func (s *Store) ListTaskLogs(ctx context.Context, taskID string) (lines []TaskLogLine, truncated bool, err error) {
	rows, err := s.pool.Query(ctx,
		`SELECT attempt, level, message, logged_at
		   FROM task_logs
		  WHERE task_id = $1
		  ORDER BY id DESC
		  LIMIT $2`,
		taskID, defaultLogLimit+1, // one extra row is how we detect there is more
	)
	if err != nil {
		return nil, false, fmt.Errorf("query task logs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var l TaskLogLine
		if err := rows.Scan(&l.Attempt, &l.Level, &l.Message, &l.LoggedAt); err != nil {
			return nil, false, fmt.Errorf("scan task log: %w", err)
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate task logs: %w", err)
	}

	// The extra row only ever existed to answer "is there more?". Drop it.
	if len(lines) > defaultLogLimit {
		lines = lines[:defaultLogLimit]
		truncated = true
	}

	// Flip newest-first back to oldest-first for reading.
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines, truncated, nil
}
