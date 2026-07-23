CREATE TABLE tasks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          UUID        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,

    -- Snapshot of the definition, copied from the workflow at run creation
    -- so a task executes what it was created with.
    name            TEXT        NOT NULL,
    handler         TEXT        NOT NULL,
    max_attempts    INT         NOT NULL DEFAULT 1,
    timeout_seconds INT         NOT NULL DEFAULT 300,

    -- Runtime state.
    status          TEXT        NOT NULL DEFAULT 'pending',
    attempt         INT         NOT NULL DEFAULT 0,

    -- When this task next becomes eligible to claim. Drives retry backoff:
    -- a failed task is pushed into the future rather than retried immediately.
    scheduled_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,

    result          JSONB,
    error           TEXT,

    -- Task names are unique within a run, so DependsOn edges resolve
    -- unambiguously and a run cannot contain two tasks called the same thing.
    UNIQUE (run_id, name),

    CONSTRAINT tasks_status_valid CHECK (
        status IN ('pending', 'ready', 'running', 'succeeded', 'failed', 'dead')
    )
);

-- The claim query in Phase 4: find the next eligible task. This index is the
-- one that matters most for throughput, since every worker runs this on a loop.
CREATE INDEX idx_tasks_claimable ON tasks (status, scheduled_at)
    WHERE status = 'ready';

-- Fetching every task for a run, for the UI and for unblocking downstream work.
CREATE INDEX idx_tasks_run ON tasks (run_id);