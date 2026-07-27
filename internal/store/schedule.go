package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"weaver/internal/workflow"
)

// ScheduledWorkflow is the current version of a workflow that carries a cron
// schedule, along with the definition the scheduler needs to materialize a run.
type ScheduledWorkflow struct {
	ID        string
	Name      string
	Schedule  string
	CreatedAt time.Time
	Def       workflow.WorkflowDef
}

// ListScheduledWorkflows returns the current version of every workflow that has a
// non-empty schedule. Older versions of a name are excluded, so re-registering a
// workflow with a new schedule cleanly supersedes the old one and only the latest
// definition is ever scheduled.
func (s *Store) ListScheduledWorkflows(ctx context.Context) ([]ScheduledWorkflow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (name) id, name, schedule, created_at, definition
		   FROM workflows
		  WHERE schedule IS NOT NULL
		  ORDER BY name, version DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query scheduled workflows: %w", err)
	}
	defer rows.Close()

	var out []ScheduledWorkflow
	for rows.Next() {
		var w ScheduledWorkflow
		var raw []byte
		if err := rows.Scan(&w.ID, &w.Name, &w.Schedule, &w.CreatedAt, &raw); err != nil {
			return nil, fmt.Errorf("scan scheduled workflow: %w", err)
		}
		if err := json.Unmarshal(raw, &w.Def); err != nil {
			return nil, fmt.Errorf("decode definition for %q: %w", w.Name, err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate scheduled workflows: %w", err)
	}
	return out, nil
}

// LastScheduledSlot returns the most recent cron slot a run was already created
// for, for this workflow, or nil if none exist yet. The scheduler uses it as the
// baseline for computing the next due slot: the next activation strictly after
// this time, so a slot already turned into a run is never scheduled twice.
func (s *Store) LastScheduledSlot(ctx context.Context, workflowID string) (*time.Time, error) {
	var slot *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT max(scheduled_for) FROM runs WHERE workflow_id = $1`,
		workflowID,
	).Scan(&slot)
	if err != nil {
		return nil, fmt.Errorf("query last scheduled slot: %w", err)
	}
	return slot, nil
}
