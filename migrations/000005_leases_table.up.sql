CREATE TABLE leases (
    -- One lease per task, enforced by making task_id the primary key.
    -- Two workers cannot hold the same task at once.
    task_id     UUID        PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,

    -- Which worker holds it. Opaque string set by the worker at startup.
    worker_id   TEXT        NOT NULL,

    -- When the claim goes stale. Extended by heartbeats while the task runs;
    -- if the worker dies the heartbeats stop and this time passes.
    expires_at  TIMESTAMPTZ NOT NULL,

    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The reaper's query in Phase 6: find leases that have expired.
CREATE INDEX idx_leases_expires_at ON leases (expires_at);

-- Useful for observability: which tasks is a given worker holding?
CREATE INDEX idx_leases_worker ON leases (worker_id);