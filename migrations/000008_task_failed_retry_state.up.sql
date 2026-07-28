-- A task whose attempt failed but still has retries left used to go straight back
-- to 'ready', so "queued, waiting for a free worker" and "just failed, waiting out
-- a backoff" were the same state. It now waits in 'failed' until its scheduled_at
-- passes, which makes a retry visible in the table and in the UI instead of being
-- inferable only from the attempt counter.
--
-- 'failed' is therefore a claimable state, not a terminal one, and the claim index
-- has to cover it or every retry would fall back to a sequential scan. It stays a
-- partial index: the two live statuses are a small slice of a large tasks table,
-- and the terminal rows (succeeded, dead, cancelled) never need to be scanned.
DROP INDEX idx_tasks_claimable;

CREATE INDEX idx_tasks_claimable ON tasks (status, scheduled_at)
    WHERE status IN ('ready', 'failed');
