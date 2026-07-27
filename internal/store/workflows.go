package store

import (
	"context"
	"encoding/json"
	"fmt"

	"weaver/internal/workflow"
)

// CreateWorkflow stores a workflow definition and returns its new id. The whole
// definition is kept as JSONB so a run can be materialized from exactly the
// document that was registered.
func (s *Store) CreateWorkflow(ctx context.Context, def workflow.WorkflowDef) (string, error) {
	raw, err := json.Marshal(def)
	if err != nil {
		return "", fmt.Errorf("marshal definition: %w", err)
	}

	// schedule is stored as its own column for the scheduler to query without
	// having to parse the JSON; an empty schedule becomes SQL NULL.
	var schedule *string
	if def.Schedule != "" {
		schedule = &def.Schedule
	}

	var id string
	err = s.pool.QueryRow(ctx,
		`INSERT INTO workflows (name, definition, schedule)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		def.Name, raw, schedule,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert workflow %q: %w", def.Name, err)
	}
	return id, nil
}
