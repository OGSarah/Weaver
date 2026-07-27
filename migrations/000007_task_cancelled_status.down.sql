-- Reverting requires no task to be sitting in the state we are about to forbid.
ALTER TABLE tasks DROP CONSTRAINT tasks_status_valid;
ALTER TABLE tasks ADD CONSTRAINT tasks_status_valid CHECK (
    status IN ('pending', 'ready', 'running', 'succeeded', 'failed', 'dead')
);
