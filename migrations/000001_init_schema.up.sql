CREATE TABLE workflows (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT        NOT NULL,
    definition  JSONB       NOT NULL,
    schedule    TEXT,
    version     INT         NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),

    -- One row per name+version, so updating a workflow adds a version
    -- rather than overwriting history.
    UNIQUE (name, version)
);

-- Fast lookup of the current definition for a given workflow name.
CREATE INDEX idx_workflows_name_version ON workflows (name, version DESC);