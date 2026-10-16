-- completed_at was only ever set in memory, so completed tasks lost it
-- whenever their actor restarted (and the Report page lost them). Recover
-- it from the activity log: the last time each completed task completed,
-- or its last update if the log has no completion.
UPDATE tasks t SET completed_at = COALESCE(
    (SELECT MAX(l.created_at) FROM activity_log l
      WHERE l.task_id = t.id AND (l.action = 'completed' OR (l.action = 'state_change' AND l.to_state = 'completed'))),
    t.updated_at)
WHERE t.state = 'completed' AND t.completed_at IS NULL;

-- A task that isn't completed has no completion time.
UPDATE tasks SET completed_at = NULL WHERE state <> 'completed' AND completed_at IS NOT NULL;
