package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"weaver/internal/workflow"

	"github.com/jackc/pgx/v5"
)

// WorkflowSummary is one workflow's metadata without its full definition, for
// the list endpoint. Only the current (highest) version of each name is listed.
type WorkflowSummary struct {
	ID        string
	Name      string
	Schedule  string // empty when the workflow is not scheduled
	Version   int
	CreatedAt time.Time
}

// WorkflowRecord is a stored workflow plus its decoded definition, for triggering
// a run or inspecting exactly what was registered.
type WorkflowRecord struct {
	WorkflowSummary
	Def workflow.WorkflowDef
}

// CreateWorkflow stores a workflow definition and returns its new id and version.
// Registering a name that already exists adds the next version rather than
// overwriting, so history is preserved and the latest version wins. The whole
// definition is kept as JSONB so a run can be materialized from exactly the
// document that was registered.
func (s *Store) CreateWorkflow(ctx context.Context, def workflow.WorkflowDef) (id string, version int, err error) {
	raw, err := json.Marshal(def)
	if err != nil {
		return "", 0, fmt.Errorf("marshal definition: %w", err)
	}

	// schedule is stored as its own column for the scheduler to query without
	// having to parse the JSON; an empty schedule becomes SQL NULL.
	var schedule *string
	if def.Schedule != "" {
		schedule = &def.Schedule
	}

	// The version is one past the current max for this name, computed in the same
	// statement so a first registration lands at 1. Two concurrent registrations
	// of the same name can race to the same version; the (name, version) unique
	// constraint makes exactly one succeed and the other error, which the API
	// surfaces as a conflict.
	err = s.pool.QueryRow(ctx,
		`INSERT INTO workflows (name, definition, schedule, version)
		 VALUES ($1, $2, $3,
		         (SELECT COALESCE(MAX(version), 0) + 1 FROM workflows WHERE name = $1))
		 RETURNING id, version`,
		def.Name, raw, schedule,
	).Scan(&id, &version)
	if err != nil {
		return "", 0, fmt.Errorf("insert workflow %q: %w", def.Name, err)
	}
	return id, version, nil
}

// ListWorkflows returns the current version of every registered workflow, newest
// name first. Older versions of a name are hidden: the latest is the one the API
// triggers and the scheduler schedules.
func (s *Store) ListWorkflows(ctx context.Context) ([]WorkflowSummary, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (name) id, name, schedule, version, created_at
		   FROM workflows
		  ORDER BY name, version DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query workflows: %w", err)
	}
	defer rows.Close()

	var out []WorkflowSummary
	for rows.Next() {
		var w WorkflowSummary
		var schedule *string
		if err := rows.Scan(&w.ID, &w.Name, &schedule, &w.Version, &w.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan workflow: %w", err)
		}
		if schedule != nil {
			w.Schedule = *schedule
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflows: %w", err)
	}
	return out, nil
}

// GetWorkflow loads a single workflow row by id, decoding its stored definition so
// a run can be materialized from it. Returns ErrNotFound if no such workflow.
func (s *Store) GetWorkflow(ctx context.Context, id string) (*WorkflowRecord, error) {
	var w WorkflowRecord
	var raw []byte
	var schedule *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, schedule, version, created_at, definition
		   FROM workflows
		  WHERE id = $1`,
		id,
	).Scan(&w.ID, &w.Name, &schedule, &w.Version, &w.CreatedAt, &raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isMalformedID(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query workflow: %w", err)
	}
	if schedule != nil {
		w.Schedule = *schedule
	}
	if err := json.Unmarshal(raw, &w.Def); err != nil {
		return nil, fmt.Errorf("decode definition: %w", err)
	}
	return &w, nil
}
