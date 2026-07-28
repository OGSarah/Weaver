-- Any task currently waiting out a backoff has to go back to 'ready' first. The
-- older claim query only looks at 'ready', so leaving them in 'failed' would strand
-- them mid-retry: never claimed, never dead, and holding their run open forever.
UPDATE tasks SET status = 'ready' WHERE status = 'failed';

DROP INDEX idx_tasks_claimable;

CREATE INDEX idx_tasks_claimable ON tasks (status, scheduled_at)
    WHERE status = 'ready';
