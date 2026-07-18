# Weaver

A learning project, for me to gain a better understanding of these technologies.

A DAG-based job scheduler and workflow orchestrator. Weaver lets you define workflows as directed acyclic graphs of tasks, schedule them, execute them across a pool of workers, and recover automatically when things fall. Think of it as small, readable, from-scratch take on the ideas behind Airflow and Temporal.

The name is based on a loom. A workflow is a set of threads (tasks) woven together into something coherent, with each pass depending on the ones before it.

## Why this exists

Weaver is built to exercise the harder, more interesting problems that show up once you taken execution reliability seriously. They are:

- At-lease-once execution with idempotency, so a retried task does not corrupt state.
- Dead worker detection via hearrbeats and lease expiry, so a crashed worker does not strand its work.
- Dependency resolution across a DAG, so tasks only run once their upsteams succeed.
- Retries with exponential backoff and timeouts, so transient failures self-heal.
- A queue that survives restarts, back by Postgres rather than in-memory state.
