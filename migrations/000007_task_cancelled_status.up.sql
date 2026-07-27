-- Cancelling an in-flight run needs a terminal state for tasks that had not yet
-- started, so they are never claimed after the run is cancelled. Extend the task
-- status check to allow 'cancelled' alongside the existing states.
ALTER TABLE tasks DROP CONSTRAINT tasks_status_valid;
ALTER TABLE tasks ADD CONSTRAINT tasks_status_valid CHECK (
    status IN ('pending', 'ready', 'running', 'succeeded', 'failed', 'dead', 'cancelled')
);
