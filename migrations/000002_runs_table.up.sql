CREATE TABLE runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_id     UUID        NOT NULL REFERENCES workflows(id),
    status          TEXT        NOT NULL DEFAULT 'pending',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,

    CONSTRAINT  runs_status_valid CHECK (
        status IN('pending', 'running', 'succeeded', 'failed', 'cancelled')
    )
);

--  The scheduler and UI both ask "what is still in flight?"
CREATE INDEX idx_runs_status ON runs (status);

-- Run history for a given worklow, newest first.
CREATE INDEX idx_runs_workflow_created ON runs (workflow_id, created_at DESC);