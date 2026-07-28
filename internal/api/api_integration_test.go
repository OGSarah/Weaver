package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"weaver/internal/store"
)

// These exercise the handlers against a real database, because the interesting
// failures here are the ones that only appear when a query actually runs: an id
// Postgres refuses to parse, a run that exists but has no tasks yet, a second
// cancel of an already-cancelled run.
func newTestAPI(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	s, err := store.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)
	return NewServer(s, t.TempDir()).Handler(), s
}

// registerWorkflow posts a definition and returns the new workflow's id.
func registerWorkflow(t *testing.T, h http.Handler, body string) string {
	t.Helper()
	rec := do(t, h, "POST", "/api/workflows", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("register: bad body: %v", err)
	}
	if resp.ID == "" || resp.Version < 1 {
		t.Fatalf("register: got id %q version %d", resp.ID, resp.Version)
	}
	return resp.ID
}

func uniqueName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("api-test-%s-%d", t.Name(), time.Now().UnixNano())
}

// An id Postgres cannot even parse as a UUID is a thing that does not exist, and
// has to read as one: a 404, not a 500 that logs an internal error and tells an
// operator something is broken when a client simply sent a bad URL.
func TestMalformedIdsAreNotFound(t *testing.T) {
	h, _ := newTestAPI(t)

	cases := []struct{ method, path string }{
		{"GET", "/api/workflows/not-a-uuid"},
		{"POST", "/api/workflows/not-a-uuid/runs"},
		{"GET", "/api/runs/not-a-uuid"},
		{"GET", "/api/runs/not-a-uuid/tasks/also-not-a-uuid"},
		{"POST", "/api/runs/not-a-uuid/cancel"},
		// A valid UUID in the run position with a malformed task id: the pair is
		// what is looked up, so either half being unparseable is the same answer.
		{"GET", "/api/runs/00000000-0000-0000-0000-000000000000/tasks/nope"},
		// Shapes that are almost a UUID, which is what a truncated copy-paste or a
		// trailing character produces.
		{"GET", "/api/runs/00000000-0000-0000-0000-00000000000"},
		{"GET", "/api/runs/00000000-0000-0000-0000-000000000000x"},
	}

	for _, tc := range cases {
		rec := do(t, h, tc.method, tc.path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (body: %s)",
				tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	// An id that is only whitespace is the one malformed shape the handler catches
	// itself, before any query: it is a missing id rather than an unknown one, and
	// says so.
	if rec := do(t, h, "GET", "/api/workflows/%20", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("whitespace id = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}

	// History is a list rather than a lookup, so an id that cannot exist answers
	// the same way an id that merely does not exist does: an empty page, not an
	// error. Both forms have to agree, or a client has two cases to handle for
	// what is the same situation.
	for _, id := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		rec := do(t, h, "GET", "/api/workflows/"+id+"/runs", "")
		if rec.Code != http.StatusOK {
			t.Errorf("history for %q = %d, want 200 (body: %s)", id, rec.Code, rec.Body.String())
			continue
		}
		var runs []runHistoryResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
			t.Errorf("history for %q: bad body: %v", id, err)
		}
		if len(runs) != 0 {
			t.Errorf("history for %q returned %d runs", id, len(runs))
		}
	}
}

// The full path a UI takes: register, read back, trigger, poll the run, read one
// task, cancel. Each step's response has to carry what the next one needs.
func TestWorkflowRunRoundTrip(t *testing.T) {
	h, _ := newTestAPI(t)
	name := uniqueName(t)

	body := fmt.Sprintf(`{"name":%q,"tasks":[
		{"id":"extract","handler":"demoTask"},
		{"id":"transform","handler":"demoTask","dependsOn":["extract"]},
		{"id":"load","handler":"demoTask","dependsOn":["transform"],"retries":2,"timeoutSeconds":30}
	]}`, name)
	workflowID := registerWorkflow(t, h, body)

	// Read back: the definition comes back whole, because the UI draws the DAG
	// from it before any run exists.
	rec := do(t, h, "GET", "/api/workflows/"+workflowID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get workflow: %d (%s)", rec.Code, rec.Body.String())
	}
	var detail workflowDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("get workflow: bad body: %v", err)
	}
	if len(detail.Tasks) != 3 {
		t.Fatalf("want 3 tasks, got %d", len(detail.Tasks))
	}

	// Trigger.
	rec = do(t, h, "POST", "/api/workflows/"+workflowID+"/runs", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("trigger: %d (%s)", rec.Code, rec.Body.String())
	}
	var triggered map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &triggered); err != nil {
		t.Fatalf("trigger: bad body: %v", err)
	}
	runID := triggered["runId"]
	if runID == "" {
		t.Fatal("trigger returned no run id")
	}

	// The run view is self-contained: statuses plus the edges, so one poll is
	// enough to draw and colour the graph.
	rec = do(t, h, "GET", "/api/runs/"+runID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get run: %d (%s)", rec.Code, rec.Body.String())
	}
	var run runResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("get run: bad body: %v", err)
	}
	if len(run.Tasks) != 3 {
		t.Fatalf("want 3 tasks in the run, got %d", len(run.Tasks))
	}
	byName := map[string]taskResponse{}
	for _, task := range run.Tasks {
		byName[task.Name] = task
	}
	if got := byName["extract"].Status; got != "ready" {
		t.Errorf("root task status = %q, want ready", got)
	}
	if got := byName["transform"].Status; got != "pending" {
		t.Errorf("dependent task status = %q, want pending", got)
	}
	if got := byName["transform"].DependsOn; len(got) != 1 || got[0] != "extract" {
		t.Errorf("transform dependsOn = %v, want [extract]", got)
	}
	if got := byName["extract"].DependsOn; len(got) != 0 {
		t.Errorf("a root task should list no upstreams, got %v", got)
	}
	// Retries becomes max_attempts = retries + 1, and an unset one is 1.
	if got := byName["load"].MaxAttempts; got != 3 {
		t.Errorf("load maxAttempts = %d, want 3", got)
	}
	if got := byName["extract"].MaxAttempts; got != 1 {
		t.Errorf("extract maxAttempts = %d, want 1", got)
	}
	// A task that has not run yet must not claim timings it does not have.
	if byName["extract"].StartedAt != "" || byName["extract"].FinishedAt != "" {
		t.Errorf("unstarted task carried timings: %+v", byName["extract"])
	}

	// The per-task view adds the timeout and the log, which is empty rather than
	// null so a client can iterate it without a nil check.
	rec = do(t, h, "GET", "/api/runs/"+runID+"/tasks/"+byName["load"].ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get task: %d (%s)", rec.Code, rec.Body.String())
	}
	var task taskDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("get task: bad body: %v", err)
	}
	if task.TimeoutSeconds != 30 {
		t.Errorf("timeoutSeconds = %d, want 30", task.TimeoutSeconds)
	}
	if task.Logs == nil {
		t.Error("logs came back null; want an empty array")
	}
	if task.RunID != runID {
		t.Errorf("task reports run %q, want %q", task.RunID, runID)
	}

	// Cancel, then cancel again: the second is a conflict rather than a silent
	// success, so a client can tell "I cancelled it" from "it was already over".
	if rec := do(t, h, "POST", "/api/runs/"+runID+"/cancel", ""); rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, "POST", "/api/runs/"+runID+"/cancel", ""); rec.Code != http.StatusConflict {
		t.Errorf("second cancel: %d, want 409 (%s)", rec.Code, rec.Body.String())
	}

	// A task id from this run cannot be read through another run's URL, so a
	// leaked id is not a way into someone else's run.
	other := do(t, h, "POST", "/api/workflows/"+workflowID+"/runs", "")
	var otherRun map[string]string
	if err := json.Unmarshal(other.Body.Bytes(), &otherRun); err != nil {
		t.Fatalf("second trigger: bad body: %v", err)
	}
	rec = do(t, h, "GET", "/api/runs/"+otherRun["runId"]+"/tasks/"+byName["load"].ID, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-run task read = %d, want 404", rec.Code)
	}
}

// Registering a name that exists adds a version rather than overwriting, and both
// versions stay independently addressable: the run history is what stitches them
// back together.
func TestRegisterSameNameAddsAVersion(t *testing.T) {
	h, _ := newTestAPI(t)
	name := uniqueName(t)

	body := fmt.Sprintf(`{"name":%q,"tasks":[{"id":"a","handler":"demoTask"}]}`, name)
	first := registerWorkflow(t, h, body)

	rec := do(t, h, "POST", "/api/workflows", fmt.Sprintf(
		`{"name":%q,"tasks":[{"id":"a","handler":"demoTask"},{"id":"b","handler":"demoTask"}]}`, name))
	if rec.Code != http.StatusCreated {
		t.Fatalf("second register: %d (%s)", rec.Code, rec.Body.String())
	}
	var second workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("second register: bad body: %v", err)
	}
	if second.Version != 2 {
		t.Errorf("version = %d, want 2", second.Version)
	}
	if second.ID == first {
		t.Error("a new version reused the previous version's id")
	}

	// The old version still resolves, and still has its own definition: a run
	// already in flight against it must stay readable.
	rec = do(t, h, "GET", "/api/workflows/"+first, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get v1: %d", rec.Code)
	}
	var v1 workflowDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &v1); err != nil {
		t.Fatalf("get v1: bad body: %v", err)
	}
	if len(v1.Tasks) != 1 {
		t.Errorf("v1 should still have 1 task, got %d", len(v1.Tasks))
	}
}

// The list endpoint is what the UI's workflow menu is built from: one entry per
// name, showing the version a trigger would actually run. Listing every version
// would fill the menu with history nobody picked.
func TestListWorkflowsShowsOnlyTheCurrentVersion(t *testing.T) {
	h, _ := newTestAPI(t)
	name := uniqueName(t)

	registerWorkflow(t, h, fmt.Sprintf(`{"name":%q,"tasks":[{"id":"a","handler":"demoTask"}]}`, name))
	latest := registerWorkflow(t, h, fmt.Sprintf(
		`{"name":%q,"schedule":"0 6 * * *","tasks":[{"id":"a","handler":"demoTask"}]}`, name))

	rec := do(t, h, "GET", "/api/workflows", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d (%s)", rec.Code, rec.Body.String())
	}
	// An empty list encodes as [] rather than null, so the UI can map over it.
	if strings.TrimSpace(rec.Body.String()) == "null" {
		t.Fatal("list returned null; want an array")
	}
	var listed []workflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list: bad body: %v", err)
	}

	var mine []workflowResponse
	for _, wf := range listed {
		if wf.Name == name {
			mine = append(mine, wf)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("%q appears %d times in the list, want once", name, len(mine))
	}
	if mine[0].ID != latest {
		t.Errorf("listed id = %s, want the newest version %s", mine[0].ID, latest)
	}
	if mine[0].Version != 2 {
		t.Errorf("listed version = %d, want 2", mine[0].Version)
	}
	// The schedule came with the second version, and the list is where the UI
	// learns a workflow is scheduled at all.
	if mine[0].Schedule != "0 6 * * *" {
		t.Errorf("listed schedule = %q, want the current version's", mine[0].Schedule)
	}
	if mine[0].CreatedAt == "" {
		t.Error("listed workflow carried no createdAt")
	}
}

// History is what the run list is built from, so the limit has to be honoured and
// the per-status counts have to add up to the task count. A client renders "2 of 4
// succeeded" from these two fields, and they cannot disagree.
func TestRunHistoryLimitAndCounts(t *testing.T) {
	h, _ := newTestAPI(t)
	name := uniqueName(t)

	workflowID := registerWorkflow(t, h, fmt.Sprintf(
		`{"name":%q,"tasks":[{"id":"a","handler":"demoTask"},{"id":"b","handler":"demoTask","dependsOn":["a"]}]}`, name))

	for i := 0; i < 3; i++ {
		if rec := do(t, h, "POST", "/api/workflows/"+workflowID+"/runs", ""); rec.Code != http.StatusCreated {
			t.Fatalf("trigger %d: %d", i, rec.Code)
		}
	}

	get := func(query string) []runHistoryResponse {
		t.Helper()
		rec := do(t, h, "GET", "/api/workflows/"+workflowID+"/runs"+query, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("history%s: %d (%s)", query, rec.Code, rec.Body.String())
		}
		var runs []runHistoryResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
			t.Fatalf("history%s: bad body: %v", query, err)
		}
		return runs
	}

	if got := len(get("")); got != 3 {
		t.Errorf("unlimited history returned %d runs, want 3", got)
	}
	if got := len(get("?limit=2")); got != 2 {
		t.Errorf("limit=2 returned %d runs", got)
	}
	// Above the store's ceiling the request still succeeds, clamped rather than
	// refused: a client asking for more than exists is not an error.
	if got := len(get("?limit=100000")); got != 3 {
		t.Errorf("limit=100000 returned %d runs, want 3", got)
	}

	runs := get("")
	// Newest first, which is what makes the first entry the one a UI selects by
	// default after triggering.
	for i := 1; i < len(runs); i++ {
		if runs[i-1].CreatedAt < runs[i].CreatedAt {
			t.Errorf("history is not newest-first: %s before %s", runs[i-1].CreatedAt, runs[i].CreatedAt)
		}
	}
	for _, run := range runs {
		if run.TaskCount != 2 {
			t.Errorf("run %s: taskCount = %d, want 2", run.ID, run.TaskCount)
		}
		total := 0
		for status, n := range run.TaskCounts {
			if n <= 0 {
				t.Errorf("run %s: status %q present with count %d", run.ID, status, n)
			}
			total += n
		}
		if total != run.TaskCount {
			t.Errorf("run %s: counts sum to %d but taskCount is %d", run.ID, total, run.TaskCount)
		}
		if run.WorkflowVersion < 1 {
			t.Errorf("run %s: workflowVersion = %d", run.ID, run.WorkflowVersion)
		}
	}
}
