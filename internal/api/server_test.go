package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Everything in this file exercises the request-handling layer alone: the routes,
// the decoder, and the validation that runs before the store is ever touched. The
// server is built with a nil store on purpose -- if a change lets one of these
// requests reach the database, the nil pointer says so immediately rather than the
// test quietly starting to depend on Postgres.
func testServer(t *testing.T) http.Handler {
	t.Helper()
	return NewServer(nil, t.TempDir()).Handler()
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A rejected registration must say what was wrong with the document, in the one
// error field every endpoint uses, and must never be a 500: these are all client
// mistakes.
func TestRegisterWorkflowRejectsBadDocuments(t *testing.T) {
	h := testServer(t)

	cases := []struct {
		name string
		body string
		want string // substring of the error message
	}{
		{"empty body", ``, "invalid JSON"},
		{"not JSON at all", `not json`, "invalid JSON"},
		{"truncated JSON", `{"name": "w", "tasks": [`, "invalid JSON"},
		{"JSON null", `null`, "name is required"},
		{"a JSON array", `[]`, "invalid JSON"},
		{"a bare string", `"workflow"`, "invalid JSON"},
		// DisallowUnknownFields: a typo must be an error, not silently dropped
		// state the caller believes was stored.
		{"unknown top-level field", `{"name":"w","tasks":[{"id":"a","handler":"h"}],"scheudle":"* * * * *"}`, "invalid JSON"},
		{"unknown task field", `{"name":"w","tasks":[{"id":"a","handler":"h","retires":3}]}`, "invalid JSON"},
		{"wrong type for tasks", `{"name":"w","tasks":{}}`, "invalid JSON"},
		{"wrong type for retries", `{"name":"w","tasks":[{"id":"a","handler":"h","retries":"three"}]}`, "invalid JSON"},
		{"no name", `{"tasks":[{"id":"a","handler":"h"}]}`, "name is required"},
		{"no tasks", `{"name":"w","tasks":[]}`, "no tasks"},
		{"duplicate ids", `{"name":"w","tasks":[{"id":"a","handler":"h"},{"id":"a","handler":"h"}]}`, "duplicate task id"},
		{"missing handler", `{"name":"w","tasks":[{"id":"a"}]}`, "missing its handler"},
		{"unknown dependency", `{"name":"w","tasks":[{"id":"a","handler":"h","dependsOn":["ghost"]}]}`, "unknown task"},
		{"cycle", `{"name":"w","tasks":[{"id":"a","handler":"h","dependsOn":["b"]},{"id":"b","handler":"h","dependsOn":["a"]}]}`, "cycle detected"},
		{"negative retries", `{"name":"w","tasks":[{"id":"a","handler":"h","retries":-1}]}`, "retries cannot be negative"},
		{"negative timeout", `{"name":"w","tasks":[{"id":"a","handler":"h","timeoutSeconds":-30}]}`, "timeoutSeconds cannot be negative"},
		{"duplicate edge", `{"name":"w","tasks":[{"id":"a","handler":"h"},{"id":"b","handler":"h","dependsOn":["a","a"]}]}`, "more than once"},
		{"bad cron", `{"name":"w","schedule":"not a cron","tasks":[{"id":"a","handler":"h"}]}`, "invalid schedule"},
		{"cron with too few fields", `{"name":"w","schedule":"* * *","tasks":[{"id":"a","handler":"h"}]}`, "invalid schedule"},
		// 6-field (seconds) specs are a different dialect from the 5-field one the
		// scheduler parses. Accepting it here would store a workflow that the
		// scheduler then refuses to run every tick.
		{"cron with a seconds field", `{"name":"w","schedule":"0 0 6 * * *","tasks":[{"id":"a","handler":"h"}]}`, "invalid schedule"},
		{"cron field out of range", `{"name":"w","schedule":"99 * * * *","tasks":[{"id":"a","handler":"h"}]}`, "invalid schedule"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, "POST", "/api/workflows", tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			var body errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not JSON: %v (%s)", err, rec.Body.String())
			}
			if !strings.Contains(body.Error, tc.want) {
				t.Errorf("error %q does not mention %q", body.Error, tc.want)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

// The /api/ catch-all is what keeps the API JSON-only. Anything under the prefix
// that matches no route -- an unknown path, a known path with the wrong verb, a
// path with extra segments -- has to come back as a JSON 404 rather than falling
// through to the file server and answering a fetch() with an HTML body it will try
// to parse. The catch-all is matched on the prefix alone, so it takes these before
// ServeMux's own method handling can turn any of them into a 405.
func TestUnroutableAPIRequestsAnswerJSON404(t *testing.T) {
	h := testServer(t)

	cases := []struct{ method, path string }{
		{"GET", "/api/"},
		{"GET", "/api/nope"},
		{"GET", "/api/workflows/id/runs/extra/segments"},
		{"GET", "/api/runs/id/tasks"},       // the task route needs a task id
		{"GET", "/api/runs/some-id/cancel"}, // cancel is POST
		{"DELETE", "/api/workflows"},        // no delete route exists
		{"PUT", "/api/workflows/id"},        // registration is a POST to the collection
		{"POST", "/api/runs/id"},            // run fetch is GET only
		{"PATCH", "/api/workflows/id/runs"}, // trigger is POST, list is GET
		{"GET", "/api/workflows/"},          // trailing slash matches no wildcard route
	}

	for _, tc := range cases {
		rec := do(t, h, tc.method, tc.path, "")

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s: Content-Type = %q, want application/json", tc.method, tc.path, ct)
		}
		var body errorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s: body is not JSON: %s", tc.method, tc.path, rec.Body.String())
		} else if body.Error == "" {
			t.Errorf("%s %s: 404 body carried no error message", tc.method, tc.path)
		}
	}
}

func TestHealthz(t *testing.T) {
	rec := do(t, testServer(t), "GET", "/healthz", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

// A malformed limit is reported rather than rounded to a default, so a typo in a
// client's query string is visible instead of silently returning a different page
// than the one asked for. These are all rejected before the store is consulted.
func TestListRunsRejectsBadLimits(t *testing.T) {
	h := testServer(t)

	// %20 rather than a literal space: a raw space is not a legal request target,
	// so it never reaches a handler in the first place.
	for _, raw := range []string{"0", "-1", "abc", "1.5", "1e3", "%20", "9999999999999999999999", "0x10"} {
		rec := do(t, h, "GET", "/api/workflows/some-id/runs?limit="+raw, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("limit=%q gave %d, want 400", raw, rec.Code)
		}
	}
}

// fmtTime is what decides whether a timestamp field appears at all, so the nil
// case (a task that has not started, a run that has not finished) has to render as
// empty rather than as the zero time.
func TestFmtTime(t *testing.T) {
	if got := fmtTime(nil); got != "" {
		t.Errorf("fmtTime(nil) = %q, want empty", got)
	}

	// Non-UTC input must come back as UTC, so clients get one comparable format.
	loc := time.FixedZone("UTC+5", 5*60*60)
	ts := time.Date(2026, 7, 28, 12, 0, 0, 500_000_000, loc)
	got := fmtTime(&ts)
	if want := "2026-07-28T07:00:00.5Z"; got != want {
		t.Errorf("fmtTime = %q, want %q", got, want)
	}

	// Sub-second precision is the reason for the Nano variant: tasks here finish
	// in milliseconds, and whole-second formatting would render every duration
	// as zero.
	if !strings.Contains(got, ".5") {
		t.Errorf("fmtTime dropped fractional seconds: %q", got)
	}
}
