package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"weaver/internal/store"
)

// timeFormat is how timestamps are rendered in every response: RFC3339 in UTC, so
// clients get one unambiguous, sortable format.
//
// The Nano variant, because plain RFC3339 truncates to whole seconds. Tasks here
// run in hundreds of milliseconds and log lines land within the same second as each
// other, so second precision would render every duration as 0s and every log line
// as the same instant. Fractional seconds are still valid RFC3339, so this is a
// strictly more precise version of the same format rather than a different one.
const timeFormat = time.RFC3339Nano

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

// logLineResponse is one recorded log line. The attempt is included on every line
// rather than grouping server-side, so the client can group, filter, or flatten it
// without the shape of the response deciding for it.
type logLineResponse struct {
	Attempt  int    `json:"attempt"`
	Level    string `json:"level"`
	Message  string `json:"message"`
	LoggedAt string `json:"loggedAt"`
}

// taskDetailResponse is the per-task view, adding timing, timeout, the result
// payload the run view omits, and the task's logs.
//
// The logs ride along with the detail rather than living behind their own endpoint
// because the panel that shows one always wants the other, and one request keeps
// them consistent: fetched separately they could come from either side of a state
// change and show a succeeded task with its previous attempt's log.
type taskDetailResponse struct {
	taskResponse
	RunID          string            `json:"runId"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	CreatedAt      string            `json:"createdAt,omitempty"`
	Result         json.RawMessage   `json:"result,omitempty"`
	Logs           []logLineResponse `json:"logs"`
	// True when the task wrote more lines than one response will carry, so the UI
	// can say the log is partial instead of implying it is whole.
	LogsTruncated bool `json:"logsTruncated,omitempty"`
}

// runHistoryResponse is one entry in a workflow's run history: enough to render a
// list without fetching each run's tasks.
type runHistoryResponse struct {
	ID              string `json:"id"`
	WorkflowVersion int    `json:"workflowVersion"`
	Status          string `json:"status"`
	CreatedAt       string `json:"createdAt,omitempty"`
	StartedAt       string `json:"startedAt,omitempty"`
	FinishedAt      string `json:"finishedAt,omitempty"`
	TaskCount       int    `json:"taskCount"`
	// Status -> count, omitting statuses no task is in. The client derives whatever
	// summary it wants from this rather than the API deciding that "2 of 4
	// succeeded" is the interesting fact and discarding what the other two are.
	TaskCounts map[string]int `json:"taskCounts"`
}

// handleListRuns returns recent runs for a workflow, newest first. The optional
// limit query parameter narrows the page; the store caps it either way.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "workflow id is required")
		return
	}

	// A malformed limit is a client bug worth reporting, not something to silently
	// round to a default that hides the typo.
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}

	runs, err := s.store.ListRunHistory(r.Context(), id, limit)
	if err != nil {
		writeStoreError(w, "list runs", err)
		return
	}

	out := make([]runHistoryResponse, 0, len(runs))
	for _, run := range runs {
		out = append(out, runHistoryResponse{
			ID:              run.ID,
			WorkflowVersion: run.WorkflowVersion,
			Status:          run.Status,
			CreatedAt:       fmtTime(&run.CreatedAt),
			StartedAt:       fmtTime(run.StartedAt),
			FinishedAt:      fmtTime(run.FinishedAt),
			TaskCount:       run.TaskCount,
			TaskCounts:      run.TaskCounts,
		})
	}
	writeJSON(w, http.StatusOK, out)
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

	lines, truncated, err := s.store.ListTaskLogs(r.Context(), d.ID)
	if err != nil {
		writeStoreError(w, "list task logs", err)
		return
	}

	// Encode an empty log as [] rather than null, so the client can map over it.
	logs := make([]logLineResponse, 0, len(lines))
	for _, l := range lines {
		logs = append(logs, logLineResponse{
			Attempt:  l.Attempt,
			Level:    l.Level,
			Message:  l.Message,
			LoggedAt: fmtTime(&l.LoggedAt),
		})
	}

	resp := taskDetailResponse{
		taskResponse:   toTaskResponse(d.TaskState),
		RunID:          d.RunID,
		TimeoutSeconds: d.TimeoutSeconds,
		CreatedAt:      fmtTime(&d.CreatedAt),
		Result:         d.Result,
		Logs:           logs,
		LogsTruncated:  truncated,
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
