-- scheduled_for records which cron slot a run was created for. Manual and
-- API-triggered runs leave it NULL; the scheduler stamps it with the exact
-- activation time of the schedule that produced the run.
ALTER TABLE runs ADD COLUMN scheduled_for TIMESTAMPTZ;

-- This is the whole distributed-scheduling guard. When several scheduler
-- instances all notice the same slot came due, each tries to insert a run for
-- (workflow_id, scheduled_for); the unique index lets exactly one win and turns
-- the rest into a no-op (ON CONFLICT DO NOTHING). Postgres treats NULLs as
-- distinct, so manual runs (scheduled_for IS NULL) are never constrained by it.
CREATE UNIQUE INDEX idx_runs_scheduled_slot ON runs (workflow_id, scheduled_for);
