-- Per-task log lines. Until now the only record of what a task did was its error
-- string and its result payload; anything a handler printed went to the worker
-- process's stdout, which belongs to whichever container happened to run it and is
-- gone the moment that container is replaced. Putting the lines in Postgres makes
-- them survive the worker, which is the whole point on a system where workers are
-- expected to die.
CREATE TABLE task_logs (
    -- A monotonic id, not just a timestamp. Two lines written in the same
    -- microsecond order arbitrarily under logged_at alone, and a log that reorders
    -- itself between two reads is worse than useless. Ordering by id is stable.
    id        BIGSERIAL   PRIMARY KEY,

    task_id   UUID        NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,

    -- Which attempt produced this line. A retried task writes several attempts'
    -- worth of logs against one task row, and "what did attempt 2 do differently"
    -- is exactly the question you open the panel to answer.
    attempt   INT         NOT NULL,

    level     TEXT        NOT NULL DEFAULT 'info',
    message   TEXT        NOT NULL,
    logged_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT task_logs_level_valid CHECK (level IN ('info', 'error'))
);

-- The only read there is: every line for one task, in order. Including id in the
-- index means that read is satisfied by an index scan with no sort step.
CREATE INDEX idx_task_logs_task ON task_logs (task_id, id);
