CREATE TABLE dependencies (
    run_id             UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    upstream_task_id   UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    downstream_task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,

    -- The edge itself is the identity: one row per dependency, no duplicates.
    PRIMARY KEY (upstream_task_id, downstream_task_id),

    -- A task cannot depend on itself. Cycles spanning several tasks are
    -- caught by Validate before a run is ever created.
    CONSTRAINT dependencies_no_self_loop CHECK (upstream_task_id <> downstream_task_id)
);

-- "What does this completed task unblock?" The hot query in Phase 5.
CREATE INDEX idx_dependencies_upstream ON dependencies (upstream_task_id);

-- "What is this task still waiting on?" Used to decide if it is ready.
CREATE INDEX idx_dependencies_downstream ON dependencies (downstream_task_id);