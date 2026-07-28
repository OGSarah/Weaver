package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"weaver/internal/store"
)

// timeFormat is how timestamps are rendered in every response: RFC3339 in UTC, so
// clients get one unambiguous, sortable format.
const timeFormat = time.RFC3339

// taskResponse is one task's state in a run view.
type taskResponse struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Handler     string  `json:"handler"`
	Status      string  `json:"status"`
	Attempt     int     `json:"attempt"`
	MaxAttempts int     `json:"maxAttempts"`
	ScheduledAt string  `json:"scheduledAt,omitempty"`
	StartedAt   string  `json:"startedAt,omitempty"`
	FinishedAt  string  `json:"finishedAt,omitempty"`
	Error       *string `json:"error,omitempty"`
	// Upstream task names. Together with the task list this makes a run response
	// self-contained: the UI can draw the DAG and colour it from one poll, with no
	// second request for the workflow definition.
	DependsOn []string `json:"dependsOn,omitempty"`
}

// runResponse is a run plus every task in it, the shape GET /runs/{id} returns.
type runResponse struct {
	ID         string         `json:"id"`
	WorkflowID string         `json:"workflowId"`
	Status     string         `json:"status"`
	CreatedAt  string         `json:"createdAt,omitempty"`
	StartedAt  string         `json:"startedAt,omitempty"`
	FinishedAt string         `json:"finishedAt,omitempty"`
	Tasks      []taskResponse `json:"tasks"`
}

// taskDetailResponse is the per-task view, adding timing, timeout, and the result
// payload the run view omits.
type taskDetailResponse struct {
	taskResponse
	RunID          string          `json:"runId"`
	TimeoutSeconds int             `json:"timeoutSeconds"`
	CreatedAt      string          `json:"createdAt,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
}

// handleGetRun returns a run and all of its task states.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "run id is required")
		return
	}

	state, err := s.store.GetRunState(r.Context(), id)
	if err != nil {
		writeStoreError(w, "get run", err)
		return
	}

	resp := runResponse{
		ID:         state.ID,
		WorkflowID: state.WorkflowID,
		Status:     state.Status,
		CreatedAt:  fmtTime(&state.CreatedAt),
		StartedAt:  fmtTime(state.StartedAt),
		FinishedAt: fmtTime(state.FinishedAt),
		Tasks:      make([]taskResponse, 0, len(state.Tasks)),
	}
	for _, t := range state.Tasks {
		resp.Tasks = append(resp.Tasks, toTaskResponse(t))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetTask returns one task within a run, including its result and error.
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("id"))
	taskID := strings.TrimSpace(r.PathValue("tid"))
	if runID == "" || taskID == "" {
		writeError(w, http.StatusBadRequest, "run id and task id are required")
		return
	}

	d, err := s.store.GetTask(r.Context(), runID, taskID)
	if err != nil {
		writeStoreError(w, "get task", err)
		return
	}

	resp := taskDetailResponse{
		taskResponse:   toTaskResponse(d.TaskState),
		RunID:          d.RunID,
		TimeoutSeconds: d.TimeoutSeconds,
		CreatedAt:      fmtTime(&d.CreatedAt),
		Result:         d.Result,
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCancelRun cancels an in-flight run.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "run id is required")
		return
	}

	if err := s.store.CancelRun(r.Context(), id); err != nil {
		writeStoreError(w, "cancel run", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// toTaskResponse maps a stored task state to its JSON shape.
func toTaskResponse(t store.TaskState) taskResponse {
	return taskResponse{
		ID:          t.ID,
		Name:        t.Name,
		Handler:     t.Handler,
		Status:      t.Status,
		Attempt:     t.Attempt,
		MaxAttempts: t.MaxAttempts,
		ScheduledAt: fmtTime(&t.ScheduledAt),
		StartedAt:   fmtTime(t.StartedAt),
		FinishedAt:  fmtTime(t.FinishedAt),
		Error:       t.Error,
		DependsOn:   t.DependsOn,
	}
}

// fmtTime renders a possibly-nil timestamp, yielding "" (omitted) when unset.
func fmtTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(timeFormat)
}
