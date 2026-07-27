package api

import (
	"errors"
	"net/http"
	"strings"

	"weaver/internal/workflow"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/robfig/cron/v3"
)

// cronParser matches the scheduler's: standard 5-field specs. Validating the
// schedule here means a workflow with a bad cron is rejected at registration
// rather than silently never firing.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// workflowResponse is the metadata returned for a registered or listed workflow.
type workflowResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Schedule  string `json:"schedule,omitempty"`
	Version   int    `json:"version"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// handleRegisterWorkflow validates a workflow definition and stores it as a new
// version. It rejects a malformed DAG (including cycles) and a bad cron schedule
// with a 400 before anything is persisted.
func (s *Server) handleRegisterWorkflow(w http.ResponseWriter, r *http.Request) {
	var def workflow.WorkflowDef
	if err := decodeJSON(r, &def); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Structural + acyclic validation (the Phase 1 cycle check).
	if err := workflow.ValidateDef(def); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A schedule, if present, must be a parseable cron spec.
	if def.Schedule != "" {
		if _, err := cronParser.Parse(def.Schedule); err != nil {
			writeError(w, http.StatusBadRequest, "invalid schedule: "+err.Error())
			return
		}
	}

	id, version, err := s.store.CreateWorkflow(r.Context(), def)
	if err != nil {
		// A duplicate (name, version) means two registrations of the same name
		// raced; the loser should retry. Surface it as a conflict, not a 500.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "workflow was updated concurrently; retry")
			return
		}
		writeStoreError(w, "register workflow", err)
		return
	}

	writeJSON(w, http.StatusCreated, workflowResponse{ID: id, Name: def.Name, Schedule: def.Schedule, Version: version})
}

// handleListWorkflows returns the current version of every workflow.
func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	wfs, err := s.store.ListWorkflows(r.Context())
	if err != nil {
		writeStoreError(w, "list workflows", err)
		return
	}
	// Encode an empty slice as [] rather than null.
	out := make([]workflowResponse, 0, len(wfs))
	for _, wf := range wfs {
		out = append(out, workflowResponse{
			ID:        wf.ID,
			Name:      wf.Name,
			Schedule:  wf.Schedule,
			Version:   wf.Version,
			CreatedAt: wf.CreatedAt.UTC().Format(timeFormat),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTriggerRun materializes a new run from a stored workflow definition and
// returns its id. The workflow id in the path selects an exact version.
func (s *Server) handleTriggerRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "workflow id is required")
		return
	}

	wf, err := s.store.GetWorkflow(r.Context(), id)
	if err != nil {
		writeStoreError(w, "get workflow", err)
		return
	}

	runID, err := s.store.CreateRun(r.Context(), wf.ID, wf.Def)
	if err != nil {
		writeStoreError(w, "create run", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"runId": runID})
}
