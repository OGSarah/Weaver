<p align="center">
  <img src="docs/branding/weaver-wordmark.png" alt="Weaver" width="396">
</p>

<p align="center">
  <img src="docs/screenshots/weaver-ui-safari-macbook.png" width="900"
       alt="The Weaver UI: a workflow drawn as a DAG with every task succeeded, a run history down the left, and a task detail panel on the right showing its timings and log">
</p>

A DAG-based job scheduler and workflow orchestrator. Weaver lets you define workflows as directed acyclic graphs of tasks, schedule them, execute them across a pool of workers, and recover automatically when things fail. Think of it as a small, readable, from-scratch take on the ideas behind Airflow and Temporal.

[![Go tests](https://github.com/OGSarah/Weaver/actions/workflows/go-test.yml/badge.svg)](https://github.com/OGSarah/Weaver/actions/workflows/go-test.yml)
[![Go lint](https://github.com/OGSarah/Weaver/actions/workflows/go-lint.yml/badge.svg)](https://github.com/OGSarah/Weaver/actions/workflows/go-lint.yml)
[![Web tests](https://github.com/OGSarah/Weaver/actions/workflows/web-test.yml/badge.svg)](https://github.com/OGSarah/Weaver/actions/workflows/web-test.yml)
[![Web lint](https://github.com/OGSarah/Weaver/actions/workflows/web-lint.yml/badge.svg)](https://github.com/OGSarah/Weaver/actions/workflows/web-lint.yml)

## Features

- Defines workflows as DAGs in JSON or via the API, with per-task dependencies.
- Cron-style scheduling plus manual and API-triggered runs.
- A worker pool that claims tasks using row-level locking (no double execution).
- Configurable retries, backoff, and per-task timeouts.
- Automatic recovery of tasks orphaned by dead workers.
- A React UI that renders the DAG, shows live run status, and exposes logs and run history.
- A REST API for triggering runs, inspecting state, and managing workflow definitions.

<details>
<summary><h2>Getting started</h2></summary>

Everything below assumes a clean machine and a clean database, and ends with the UI
open in a browser and real runs moving through it.

### Prerequisites

- **Docker** (Docker Desktop, OrbStack, or equivalent) for Postgres and the three services.
- **Go 1.22+** only if you want to run the binaries outside Docker. The routing uses
  the standard library's method-and-path patterns, which landed in 1.22.
- **Node 18+** only if you intend to change the frontend *and* run the API outside
  Docker. The image builds the bundle itself, so `docker compose up` renders the UI
  on a machine with no Node installed at all.

### 1. Clone

```bash
git clone <your-repo-url> weaver
cd weaver
```

### 2. Start everything

```bash
docker compose up --build --scale worker=2
```

That brings up five things: Postgres, a one-shot `migrate` container that applies
every migration in `migrations/` and exits, the API on port 8080, the scheduler
(which runs the cron loop and the reaper), and two workers. The workers wait for the
migration container to finish, so they never poll a schema that is not there yet.

Two workers rather than one because a single worker runs its tasks one at a time; with
two you can watch independent branches of a DAG execute in parallel.

Leave it running and open a second terminal for the next step.

### 3. Register two example workflows

Nothing is registered by default, so the UI starts empty. These two are enough to see
every state the system has. Both use the demo handlers wired up in
[`cmd/worker/main.go`](cmd/worker/main.go), which sleep for a random 200-1000ms and
log as they go.

A healthy pipeline, shaped as a diamond so two tasks run in parallel:

```bash
curl -s localhost:8080/api/workflows -H 'Content-Type: application/json' -d '{
  "name": "etl-pipeline",
  "tasks": [
    {"id": "extract",   "handler": "extractData"},
    {"id": "transform", "handler": "transformData", "dependsOn": ["extract"]},
    {"id": "validate",  "handler": "validateData",  "dependsOn": ["extract"]},
    {"id": "load",      "handler": "loadWarehouse", "dependsOn": ["transform", "validate"]},
    {"id": "notify",    "handler": "sendEmail",     "dependsOn": ["load"]}
  ]}'
```

And one that fails on purpose. `noSuchHandler` is not in the registry, so the task
fails every attempt: the cheapest way to watch the retry and backoff machinery
without waiting for a real flake.

```bash
curl -s localhost:8080/api/workflows -H 'Content-Type: application/json' -d '{
  "name": "flaky-pipeline",
  "tasks": [
    {"id": "extract",    "handler": "extractData"},
    {"id": "broken",     "handler": "noSuchHandler", "retries": 3, "dependsOn": ["extract"]},
    {"id": "never-runs", "handler": "sendEmail",     "dependsOn": ["broken"]}
  ]}'
```

Each returns the new workflow's id and version.

### 4. Open the UI

[http://localhost:8080](http://localhost:8080)

Pick a workflow on the left, press **Trigger run**, and watch. The next section is a
tour of what is worth looking at.

### Scaling and stopping

```bash
docker compose up --scale worker=4   # more workers, same queue
docker compose down                  # stop, keeping the database volume
docker compose down -v               # stop and delete all data
```

### What to try

The demo handlers finish in well under a second, so a run is over in a few seconds.
Trigger things more than once.

**Watch a run execute.** Pick `etl-pipeline` and press **Trigger run**. Nodes move
grey -> blue -> amber (pulsing) -> green as the workers pick them up. `transform` and
`validate` both depend only on `extract`, so with two workers they go amber together,
and `load` stays grey until both are green. That is the dependency resolution doing
its job, visible.

**Open a task while it is running.** Click any node. The panel on the right shows its
timings, its handler, and its log, and keeps up with it: log lines appear as the
handler writes them and Duration counts up as `755ms (running)` until the task lands.

**Watch a task retry and die.** Trigger `flaky-pipeline`. `broken` turns orange
(`FAILED`, with a `2/4` attempt counter) between attempts, then red (`DEAD`) once its
attempts are gone. Click it: the log is grouped by attempt, and each group records
the jittered backoff that followed it, something like `retrying in 582ms (attempt 2
of 4)`. Notice `never-runs` stays grey forever, because a downstream task of a dead
task is never unblocked.

**Read the run history.** Bottom left, newest first. The bar under each row is that
run's tasks broken down by state, so a run that half-finished is distinguishable from
one that failed immediately without opening either. Hover for exact counts, click to
reopen a run and inspect it exactly as if it were live.

**Cancel a run.** Trigger `etl-pipeline`, then, while it is still going:

```bash
curl -s -X POST localhost:8080/api/runs/<run-id>/cancel
```

Anything not yet started turns to dashed `CANCELLED` outlines. The UI never learns
about this directly; it just polls, which is the point of keeping the UI a read layer.

**Kill a worker mid-task.** The Phase 6 payoff, and the most interesting thing here:

```bash
docker compose kill --signal=SIGKILL worker
```

`SIGKILL` gives the worker no chance to release its lease. Its task sits in `RUNNING`
until the lease expires (30s), the reaper notices, and the task goes back to `FAILED`
with `worker lease expired; requeued by reaper` in its log. Bring workers back with
`docker compose up -d --scale worker=2` and another one finishes it, on the next
attempt number. Nothing is lost and nothing runs twice.

</details>

<details>
<summary><h2>Understanding DAGS</h2></summary>

DAG stands for Directed Acyclic Graph. It is the concept the entire project is built around, so it is worth taking the time to understand before doing anything else. Break the name into its three parts:

- Graph: A set of nodes connected by edges. In Weaver, each node is a task ("extract data", "send email") and each edge is a dependency between tasks.
- Directed: The edges have a direction. "Transform" depends on "extract", and that arrow only points one way. Extract has to finish before transform can start, never the reverse.
- Acyclic: There are no cycles. You can never follow the arrows and end up back where you started. This is a crucial property.

A valid DAG has arrows that only ever flow forward.

```mermaid
graph TD
Extract --> Transform
Extract --> Validate
Transform --> Load
Validate --> Load
```

A graph with a cycle is not a DAG, and a scheduler cannot run it. Below, A waits on C, C waits on B, and B waits on A. Nothing can ever start, because every task is blocked by another task that is itself blocked.

```mermaid
graph LR
A[Task A] --> B[Task B]
B --> C[Task C]
C --> A
```

### Why acyclic matters

The acyclic property is what makes the whole system computable. Because there are no cycles, two things are always true:

1. You can always find a valid order to run the tasks. This ordering is called a `topological sort`, and there can be more than one valid ordering. This is exactly what lets independent tasks (like "transform" and "validate" above) run in parallel.
2. You can always answer "what is ready to run right now?" by checking whether every task pointing into a given task has already succeeded.

The worker loop is essentially:
- Find tasks whose upstream dependencies are all done.
- Run the tasks.
- Mark the tasks as complete.
- Repeat. The algorithm only terminates because the graph is acyclic.

Because of this, one of the first things Weaver does when a workflow is submitted is validate that it is actually a DAG, rejecting any definition that contains a cycle before it ever tries to run. Cycle detection is a classic depth-first-search problem.

### Glossary

- `Node` (or vertex): A single task.
- `Edge`: A dependency arrow between two tasks.
- `Upstream`: The tasks that must finish before a given task can run ("extract" is upstream of "transform").
- `Downstream`: The tasks waiting on a given task to finish.
- `Root task`: A task with no upstream dependencies. These are what the scheduler kicks off first when a run starts.
- `Topological sort`: Any ordering of the tasks that respects all the dependency arrows.

</details>

<details>
<summary><h2>Architecture</h2></summary>

Weaver splits into four moving parts: 
1. An API server
2. A Postgres-backed store that doubles as the task queue
3. A pool of stateless workers
4. A scheduler that turns time into work. The React UI talks only to the API server.
 
```mermaid
graph TB
    subgraph Client
        UI[React UI]
    end
 
    subgraph Control Plane
        API[API Server]
        SCH[Scheduler]
    end
 
    subgraph State
        DB[(Postgres:<br/>workflows, runs,<br/>tasks, queue, leases)]
    end
 
    subgraph Execution
        W1[Worker 1]
        W2[Worker 2]
        W3[Worker N]
    end
 
    UI -->|REST| API
    API -->|read / write| DB
    SCH -->|enqueue due runs| DB
    W1 -->|claim / heartbeat / complete| DB
    W2 -->|claim / heartbeat / complete| DB
    W3 -->|claim / heartbeat / complete| DB
    SCH -->|reap expired leases| DB
```
 
### Task lifecycle
 
Every task moves through a small, explicit state machine. Keeping the states minimal is what makes recovery tractable: a reaper only has to look for leases that expired while a task was RUNNING.
 
```mermaid
stateDiagram-v2
    [*] --> Pending
    Pending --> Ready: all upstream tasks succeeded
    Ready --> Running: worker claims lease
    Running --> Succeeded: task returns ok
    Running --> Failed: error or timeout, retries remaining
    Failed --> Running: backoff elapsed, worker claims lease
    Running --> Dead: error or timeout, retries exhausted
    Running --> Failed: lease expired (worker died), retries remaining
    Running --> Dead: lease expired, retries exhausted
    Pending --> Cancelled: run cancelled
    Ready --> Cancelled: run cancelled
    Failed --> Cancelled: run cancelled
    Succeeded --> [*]
    Dead --> [*]
    Cancelled --> [*]
```
 
Two of these states are easy to misread. **Failed is a waiting state, not a terminal
one:** it means an attempt happened and did not finish, and another is coming. Dead
is the terminal one, reached when the attempts are spent. Keeping them apart is what
lets you tell a task that is retrying from one that has given up, and both from a
task that is merely queued behind a busy worker pool.

Every state has exactly one meaning, which is why a dead worker's task also lands in
Failed rather than going back to Ready. Ready means "never attempted since it was
unblocked"; Failed means "an attempt is behind it". A task's status alone tells you
which, with no need to read its error column to find out. The two paths into Failed
differ only in when the next attempt may start: a handler error backs off
exponentially, while a task reclaimed from a dead worker is claimable immediately,
since a worker dying is no evidence the task itself needs time to settle.
 
Because Failed is a waiting state, it is *claimable*. The claim query selects on
`status IN ('ready', 'failed') AND scheduled_at <= now()`, so a retry becomes
eligible the moment its delay passes, with no background sweep needed to promote it
back to Ready. It also means cancelling a run has to cancel Failed tasks alongside
Pending and Ready ones, or a cancelled run would still have work picked up when its
backoff elapsed.
 
### Task logs
 
A handler is given a logger alongside its context and task, and every line it writes
lands in the `task_logs` table against the task and attempt that produced it:
 
```go
reg.Register("extractData", func(ctx context.Context, task store.ClaimedTask, log *worker.TaskLogger) error {
    log.Printf("fetching %d rows", n)
    return nil
})
```
 
Two decisions are worth knowing about.
 
**Lines are written one at a time, as they happen,** rather than buffered and
flushed when the task ends. That is chattier, and it is the right trade here: a
buffered log is lost exactly when a worker is killed mid-task, which is the moment
the log was worth keeping. It also means the UI can tail a task that is still
running.
 
**The state transitions log themselves,** in the same transaction that performs
them. Claiming, succeeding, failing, retrying, being reaped, and being cancelled
each write their own line, so a task's log is a complete account of its life even if
its handler never logged anything, and a task can never be marked dead with nothing
saying why. The attempt number on each line is what keeps a retried task readable:
the UI groups by it, so "what did attempt 2 do differently" is answerable at a
glance.
 
### Execution flow for a single run
 
```mermaid
sequenceDiagram
    participant U as User / Scheduler
    participant A as API Server
    participant D as Postgres
    participant W as Worker
 
    U->>A: trigger workflow run
    A->>D: create run + task rows (Pending)
    A->>D: mark root tasks Ready
    loop poll for work
        W->>D: claim a Ready task (SELECT ... FOR UPDATE SKIP LOCKED)
        D-->>W: task + lease (Running)
        loop while executing
            W->>D: heartbeat (extend lease)
        end
        alt success
            W->>D: mark Succeeded
            W->>D: mark newly unblocked tasks Ready
        else failure
            W->>D: mark Failed (schedule retry or Dead)
        end
    end
```
 
</details>

<details>
<summary><h2>How the hard parts work</h2></summary>

### At-least-once, not exactly-once
 
Weaver guarantees a task will run at least once. Exactly-once is not achievable in a distributed system without cooperation from the task itself, so tasks are expected to be idempotent. Each task execution carries a stable run ID and task ID that handlers can use as an idempotency key.
 
### Claiming work without double execution
 
Workers claim tasks with `SELECT ... FOR UPDATE SKIP LOCKED`. This lets many workers poll the same table concurrently while guaranteeing that any given task row is handed to exactly one worker at a time. No external lock service is required.
 
### Dead worker detection
 
When a worker claims a task it also writes a lease with an expiry timestamp, and it renews that lease with periodic heartbeats while the task runs. If the worker crashes, the heartbeats stop and the lease expires. A reaper (run by the scheduler) sweeps for RUNNING tasks whose lease has passed, and returns them to READY so another worker can pick them up. This is how a run resumes after a worker dies mid-task.
 
### Retries and timeouts
 
Each task has a max attempt count and a base backoff. On failure, Weaver computes the next eligible run time using exponential backoff with jitter, and the task will not be claimable again until that time passes. A task that exceeds its timeout is treated as a failure and follows the same path.
 
</details>

<details>
<summary><h2>Data model</h2></summary>

The core tables (simplified):
 
- `workflows`: The DAG definition, versioned, stored as task nodes and edges.
- `runs`: One row per triggered execution of a workflow.
- `tasks`: One row per task per run, holding state, attempt count, timings, and result.
- `dependencies`: Upstream and downstream edges for tasks within a run.
- `leases`: worker ID, task ID, and expiry for in-flight work

Keeping the queue inside Postgres (rather than a separate broker) means one source of truth, transactional state transitions, and easy recovery. It trades some raw throughput for a much simpler correctness story, which is the right call for a system whose whole point is reliability.
 
</details>

<details>
<summary><h2>Tech stack</h2></summary>

The backend is written in Go, split into three binaries (`cmd/api`, `cmd/scheduler`, `cmd/worker`) that share one database. Go is a natural fit here: goroutines make the worker pool and heartbeat loops cheap and easy to reason about, and each binary compiles to a single static file that is trivial to run and deploy.
 
- Language: Go (1.22 or newer).
- Postgres driver: `pgx` (`github.com/jackc/pgx`), which exposes the row-level locking and `SELECT ... FOR UPDATE SKIP LOCKED` behavior the queue relies on.
- Migrations: `golang-migrate` for versioned schema changes.
- HTTP: the standard library `net/http`, optionally with a lightweight router like `chi`. No heavy framework is needed.
- Cron parsing: `robfig/cron` for interpreting workflow schedules.
- Store and queue: Postgres, serving as both the durable store and the task queue.
- Frontend: React, bundled with esbuild, plus a DAG rendering library such as React Flow for the graph view.
- Local dev: Docker Compose to bring up Postgres and one or more workers.

Deliberately not used: a separate message broker (Redis, RabbitMQ, Kafka) or an external lock service. Keeping the queue and locks inside Postgres is the whole point, since it gives transactional state transitions and one source of truth. Adding a broker later is a reasonable extension, not a starting requirement.

</details>

<details>
<summary><h2>Frontend development</h2></summary>

Skip this unless you are changing the UI. The Docker image builds the bundle itself,
so the steps above never need npm.

The UI is a React app under `web/`, bundled with esbuild (no Vite). The Go API serves
the built assets, so there is one origin and no CORS: API routes live under `/api`,
and every other path falls through to the file server. A browser call is just
`fetch('/api/workflows')`, with no host and no port.

The API reads the frontend from disk rather than embedding it, and looks in `./web`
by default (`WEB_DIR` overrides it; the image sets it to `/web`). So a rebuilt bundle
needs only a browser refresh, with no Go rebuild.

**Work on the frontend with the API on the host, not in Docker.** The image bakes the
bundle in at build time, so a change only reaches a container through
`docker compose up --build`, which is far too slow a loop to design against. Run
Postgres in Compose and everything else locally, as in [Running without
Docker](#running-without-docker), then:

```bash
cd web
npm install     # once
npm run dev     # rebuild on every save
npm run build   # one-shot build
npm test        # unit tests for the pure modules (node --test)
npm run lint    # eslint, the same check CI runs
```

`npm run dev` writes straight into `web/dist/`, which is where the host API is already
reading from, so the loop is: save, refresh.

Both commands bundle `src/main.jsx`, resolving imports and transforming JSX into
`dist/bundle.js`, which `index.html` loads. `npm run dev` only rebuilds the bundle; it
does not reload the browser, so refresh to see changes.

They also handle one non-JS import. The header wordmark is `import`ed straight from
`docs/branding/`, and the `--loader:.png=file` flag tells esbuild to copy that file
into `dist/` under a content-hashed name and resolve the import to its URL
(`--public-path=/dist`, which is where the Go file server exposes it). So the image
has one source in the repo, the same file the README shows, rather than a second copy
living under `web/`. Emitting assets like this is the other half of what a bundler
does, beyond resolving and transforming JavaScript.

Colors and spacing come from `web/src/theme.js`, and the task state palette from
`web/src/status.js`. Those two files are the whole design system; components import
from them rather than writing literal colors.

</details>

<details>
<summary><h2>Running without Docker</h2></summary>

Useful when iterating on Go code, since it skips an image rebuild each time, and the
only sensible way to work on the frontend.

This path needs Node, because nothing has built the UI for you. `web/dist/` is not in
the repo; under Docker the image builds it, and here you do. Skip the first two lines
if you only care about the API and are happy for the page to be blank.

```bash
# Build the UI once (or `npm run dev` to keep rebuilding on save)
cd web && npm install && npm run build && cd ..

# Postgres still comes from Compose
docker compose up -d postgres

export DATABASE_URL='postgres://weaver:weaver_dev_password@localhost:5432/weaver?sslmode=disable'
make migrate-up

go run ./cmd/api        # :8080, serves the API and web/ from disk
go run ./cmd/worker     # run this in two terminals for two workers
go run ./cmd/scheduler  # cron loop and reaper
```

`make migrate-up` needs the [golang-migrate](https://github.com/golang-migrate/migrate)
CLI on your PATH (`brew install golang-migrate`). Under Docker the `migrate` service
does this for you, so the CLI is only needed here.

### If the workflow list looks cluttered

The integration tests register throwaway workflows and leave them behind. To clear
them out:

```bash
docker compose exec postgres psql -U weaver -d weaver -c "
DELETE FROM runs WHERE workflow_id IN (
  SELECT id FROM workflows WHERE name LIKE 'claim-test-%' OR name LIKE 'demo-%');
DELETE FROM workflows WHERE name LIKE 'claim-test-%' OR name LIKE 'demo-%';"
```

</details>

<details>
<summary><h2>API sketch</h2></summary>

Every API route lives under `/api`. Everything outside that prefix is the UI, served
as static files by the same binary on the same origin.

```
POST   /api/workflows              register or update a workflow definition
GET    /api/workflows              list workflows (metadata only, no task lists)
GET    /api/workflows/:id          fetch one workflow with its full task list,
                                   including the dependency edges the UI draws
POST   /api/workflows/:id/runs     trigger a run
GET    /api/workflows/:id/runs     run history, newest first (?limit=N).
                                   Covers every version of the workflow's name,
                                   not just the version in the path
GET    /api/runs/:id               fetch run status, task states, and the
                                   dependency edges between them
GET    /api/runs/:id/tasks/:tid    fetch a single task: timings, result payload,
                                   and its log lines
POST   /api/runs/:id/cancel        cancel an in-flight run

GET    /healthz                    liveness probe (not under /api: it is an
                                   infrastructure concern, not part of the UI's API)
```

### Example workflow definition

```json
{
  "name": "daily-report",
  "schedule": "0 6 * * *",
  "tasks": [
    { "id": "extract", "handler": "extractData", "retries": 3, "timeoutSeconds": 120 },
    { "id": "transform", "handler": "transformData", "dependsOn": ["extract"] },
    { "id": "load", "handler": "loadWarehouse", "dependsOn": ["transform"] },
    { "id": "notify", "handler": "sendEmail", "dependsOn": ["load"], "retries": 5 }
  ]
}
```

</details>

<details>
<summary><h2>Trying the API</h2></summary>

With the stack up (`docker compose up --build`), the API is on `http://localhost:8080`. A definition is validated on registration: a cyclic DAG, an edge to an unknown task, or an unparseable schedule is rejected with a `400` before anything is stored. Registering a name that already exists stores a new version rather than overwriting it.

```bash
# Register a workflow (returns its id and version)
curl -s localhost:8080/api/workflows -d @workflow.json

# List the current version of every workflow
curl -s localhost:8080/api/workflows

# Trigger a run of a workflow by id (returns the new run id)
curl -s -X POST localhost:8080/api/workflows/<workflow-id>/runs

# Run history, newest first, across every version of this workflow's name
curl -s "localhost:8080/api/workflows/<workflow-id>/runs?limit=10"

# Fetch a run, the state of every task in it, and the edges between them
curl -s localhost:8080/api/runs/<run-id>

# Fetch a single task: timings, result, error, and its log lines grouped by attempt
curl -s localhost:8080/api/runs/<run-id>/tasks/<task-id>

# Cancel an in-flight run (stops unstarted tasks; running ones finish and are ignored)
curl -s -X POST localhost:8080/api/runs/<run-id>/cancel
```

Workflows that carry a `schedule` are picked up by the scheduler, which creates a run each time a cron slot comes due. If the scheduler was down across several slots, it backfills a run for each missed slot when it returns rather than skipping them. The run for a given slot is created exactly once even if several schedulers are running: the insert is guarded by a unique index on `(workflow_id, scheduled_for)`, so the losers of the race become no-ops rather than duplicate runs.

</details>

<details>
<summary><h2>Testing</h2></summary>

```bash

make test-db     # create weaver_test and apply the migrations (once, and after adding one)
make test        # everything, including the tests that need Postgres
make test-unit   # only the tests that need nothing; the rest skip themselves
make lint        # gofmt, go vet, eslint
cd web && npm test

```

The tests that touch a database point at `weaver_test`, not the `weaver` database
Compose uses. They share a queue with whatever else is running against the same
database, and the workers in Compose would claim the tasks a test just created.

### What each suite covers

| Suite | Tests | Coverage | Needs Postgres | What it holds down |
| --- | --- | --- | --- | --- |
| [internal/workflow](internal/workflow/) | 12 | 100% | no | Validation and the cycle check: every way a definition can be malformed, a 50k-deep chain, a 5k-wide fan-out, and a topological order that does not drift between runs |
| [internal/store](internal/store/) | 25 | 66% | yes | The transactions. No double claim under contention, lease ownership, retry backoff and its jitter, cancellation, the reaper's verdicts, log and history caps, and a cancel racing a claim |
| [internal/worker](internal/worker/) | 13 | 87% | yes | What a handler can do to a worker: panic, hang past its timeout, name a handler that does not exist, or finish work whose lease was reclaimed. Plus the poll loop and its shutdown |
| [internal/api](internal/api/) | 10 | 81% | partly | The request surface: malformed JSON, unknown fields, bad cron, bad ids, wrong verbs, limit parsing, and a full register → trigger → poll → cancel round trip |
| [internal/scheduler](internal/scheduler/) | 9 | 84% | yes | Cron slots becoming runs: backfilling every missed slot, never twice, never early, and a broken schedule not stalling the others |
| [web/src](web/src/) | 19 | — | no | The pure frontend modules: DAG geometry, edge routing, and the status palette, including graphs the API would reject |

Coverage is statement coverage from `make test`. The API row is "partly" because
the validation and routing tests run with no database at all -- the server is built
with a nil store, so a change that let one of those requests reach Postgres fails
immediately rather than quietly growing a dependency.

### Two things worth knowing

**Database-backed packages take turns.** `go test ./...` runs packages
concurrently, and the claim query is global by design: a worker takes the next
eligible task anywhere in the queue, not one scoped to a run. Two packages testing
at once therefore claim each other's tasks. Each such package's `TestMain` takes a
Postgres advisory lock ([internal/testsupport](internal/testsupport/)), so they
queue up however the suite is invoked, rather than relying on a `-p 1` nobody
remembers to pass.

**Integration tests are the point here, not a supplement.** Almost everything this
project claims is a property of a transaction -- a claim that cannot double-issue,
a completion that unblocks downstream work atomically, a reaper deciding a task's
fate. Those are only true against a real Postgres, so CI runs one as a service
container and applies the migrations before the suite, rather than skipping every
test that matters.

### Linting

| Check | Command | Runs in |
| --- | --- | --- |
| `gofmt` | `gofmt -l .` | [Go lint](.github/workflows/go-lint.yml) |
| `go vet` | `go vet ./...` | [Go lint](.github/workflows/go-lint.yml) |
| ESLint (flat config, `react-hooks`) | `cd web && npm run lint` | [Web lint](.github/workflows/web-lint.yml) |

CI runs the four workflows behind the badges at the top: Go tests (with a Postgres
service), Go lint, web tests, and web lint. Every one of them is a command above,
so a green badge means the same thing locally and on a branch.

</details>

## License

Released under the [MIT License](LICENSE). © 2026 SarahUniverse
