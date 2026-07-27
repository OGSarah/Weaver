DROP INDEX IF EXISTS idx_runs_scheduled_slot;
ALTER TABLE runs DROP COLUMN IF EXISTS scheduled_for;
