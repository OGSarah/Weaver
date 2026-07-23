# Weaver build checklist

## Phase 0: Environment and project setup

- [x] Install Go (`brew install go`) and confirm with `go version`.
- [x] Install a container runtime. Already had Docker Desktop.
- [x] Install a Postgres client for poking at the database. I installed TablePlus
- [x] Install VS Code and the official Go extension (adds gopls, debugging, and format-on-save).
- [x] Create the repo: `go mod init github.com/<you>/weaver`.
- [x] Set up the folder layout: `cmd/api`, `cmd/scheduler`, `cmd/worker` for the three binaries, and `internal/` for shared code.
- [x] Add a `docker-compose.yml` that starts a Postgres container. Get it running with `docker compose up`.
- [x] Commit this as your first checkpoint.

You should understand: why this project ships as three separate binaries (api, scheduler, worker) that all share one database, rather than one big process.

---

## Phase 1: The DAG, in memory

Start with the pure logic, no database and no network. This is the conceptual heart of the project and the easiest part to unit-test.

- [x] Define Go structs for a workflow: a set of tasks and the dependency edges between them.
- [x] Write a function that, given a workflow, returns the root tasks (those with no upstream dependencies).
- [x] Write a function that, given a completed task, returns the tasks it unblocks (its downstream tasks).
- [x] Write a cycle-detection function using depth-first search. It should return an error if the workflow contains a cycle.
- [x] Write a topological sort that returns a valid execution order, or an error if the graph is cyclic.
- [x] Write unit tests: a valid diamond DAG, a simple cycle, a self-loop, and a disconnected graph. Confirm cycles are rejected.

You should understand: how depth-first search detects a cycle (the "currently visiting" versus "fully visited" node states), and why a topological sort is impossible on a cyclic graph.

---

## Phase 2: The database schema

Now make state durable. Postgres is both your store and your queue, so the schema is the backbone of the whole system.

- [x] Choose a migration tool (golang-migrate is a common choice) and wire it in.
- [x] Create the `workflows` table: id, name, definition (the DAG as JSON), schedule, version.
- [x] Create the `runs` table: id, workflow_id, status, created_at, started_at, finished_at.
- [x] Create the `tasks` table: id, run_id, name, handler, status, attempt count, max attempts, timeout, scheduled_at, timings, result/error.
- [x] Create the `dependencies` table (or store edges in the task rows): which tasks block which within a run.
- [x] Create the `leases` table: task_id, worker_id, expires_at.
- [ ] Add indexes you will actually query on: tasks by (status, scheduled_at), leases by expires_at.
- [ ] Run the migrations against your Compose Postgres and inspect the tables with your client.

You should understand: why keeping the queue inside Postgres (rather than adding Redis or a broker) gives you transactional state transitions and a single source of truth, and what you trade away for that simplicity.

---

## Phase 3: Triggering a run

Turn a stored workflow definition into a concrete run with task rows ready to execute.

- [ ] Write the code that, given a workflow, creates a `runs` row and one `tasks` row per task, all starting in a Pending state.
- [ ] In the same database transaction, mark the root tasks as Ready.
- [ ] Confirm the state transition rules in code: a task becomes Ready only when all of its upstream tasks have Succeeded.
- [ ] Write a query that, given a run, returns its full current state (every task and its status).
- [ ] Test by triggering a run manually and inspecting the rows. The roots should be Ready; everything else Pending.

You should understand: why creating the run and marking its roots Ready must happen in a single transaction (what breaks if a crash lands between those two steps).

---

## Phase 4: The worker claim loop

This is the concurrency core. Multiple workers will poll the same table, and no task may ever run twice at once.

- [ ] Write the claim query using `SELECT ... FOR UPDATE SKIP LOCKED` to grab one Ready task whose scheduled_at has passed.
- [ ] In the same transaction, flip the task to Running and write a lease row with an expiry a short time in the future.
- [ ] Build the worker's main loop: poll for a task, if one is claimed run its handler, otherwise sleep briefly and poll again.
- [ ] Create a simple handler registry: a map from handler name to a Go function, so tasks know what code to run.
- [ ] Start two workers at once (in Compose, scale the worker service) and confirm they never grab the same task.

You should understand: exactly what `SKIP LOCKED` does, and why it lets many workers share one queue table without an external lock service.

---

## Phase 5: Completion, unblocking, and failure

Make a run actually flow from start to finish, including when tasks fail.

- [ ] On successful handler return: mark the task Succeeded and delete its lease, in one transaction.
- [ ] In that same transaction, check each downstream task and mark it Ready if all of its upstreams have now Succeeded.
- [ ] When the last task in a run finishes, mark the run itself as complete.
- [ ] On handler error or panic: mark the task Failed and record the error message.
- [ ] Implement retries: if attempts remain, schedule the task to become Ready again after an exponential backoff delay (add jitter). If attempts are exhausted, mark it Dead.
- [ ] Implement timeouts: if a handler runs longer than the task's timeout, treat it as a failure.
- [ ] Test a full happy-path run end to end, then a run where one task fails and retries, then one where a task exhausts its retries.

You should understand: at-least-once execution and why it forces your handlers to be idempotent (what happens if the same task runs twice because of a retry).

---

## Phase 6: Dead worker recovery

The senior-level payoff. Prove that a run resumes when a worker dies mid-task.

- [ ] Add heartbeats: while a handler runs, periodically extend the lease's expires_at.
- [ ] Write the reaper: a routine that finds Running tasks whose lease has expired and returns them to Ready.
- [ ] Run the reaper on a timer inside the scheduler binary.
- [ ] Chaos test: start a run, then hard-kill a worker (not a graceful shutdown) while it holds a task. Confirm the reaper reclaims the task and another worker finishes it.
- [ ] Confirm the reclaimed task respects its attempt count so a genuinely stuck task cannot loop forever.

You should understand: how leases plus heartbeats distinguish "a worker is still working" from "a worker has died", and why this is more reliable than asking workers to report their own death.

---

## Phase 7: Scheduling and the API

Wrap the engine in the two things a user touches: scheduled triggers and an HTTP API.

- [ ] Add cron-style scheduling: the scheduler parses each workflow's schedule and creates runs when they come due.
- [ ] Make sure a due schedule creates exactly one run even with multiple scheduler instances (guard it with the database).
- [ ] Build the REST API: register/update a workflow, list workflows, trigger a run, fetch run status, fetch a single task with its logs, cancel a run.
- [ ] Validate incoming workflow definitions with your Phase 1 cycle check. Reject cyclic DAGs with a clear error.
- [ ] Test the endpoints with curl or a REST client.

You should understand: why "create exactly one run when a schedule is due" is a distributed-systems problem the moment you run more than one scheduler.

---

## Phase 8: The UI and polish

Make it visible and pleasant, then reach for the stretch goals.

- [ ] Scaffold a React app (Vite is the quickest) that talks to your API.
- [ ] Render a workflow as a DAG using a graph library, so dependencies are visible.
- [ ] Show a run's live status: color each task node by state (Pending, Ready, Running, Succeeded, Failed, Dead) and poll for updates.
- [ ] Add a run history view and a per-task log/detail panel.
- [ ] Write a README section documenting how to run the whole thing locally.
- [ ] Stretch: a metrics endpoint exposing queue depth, run latency, and worker liveness.
- [ ] Stretch: per-workflow concurrency limits or task priorities.
- [ ] Stretch: dynamic fan-out, where one task generates several downstream tasks at runtime.

You should understand: how the UI stays a thin read layer over the database, and why the interesting reliability guarantees all live in the backend, not the frontend.

---

## Definition of done

- [ ] A workflow can be defined, validated, scheduled, and triggered.
- [ ] Multiple workers execute tasks concurrently with no double execution.
- [ ] Failed tasks retry with backoff and eventually land in Dead.
- [ ] Killing a worker mid-task does not lose the work: it resumes.
- [ ] The UI shows the DAG and live run status.
- [ ] You can explain, out loud, every "you should understand" line above.
