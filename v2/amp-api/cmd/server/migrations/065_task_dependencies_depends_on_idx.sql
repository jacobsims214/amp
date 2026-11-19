-- Reverse lookups on task_dependencies (AMP #1574).
--
-- The primary key is (task_id, depends_on), so "which tasks wait on X"
-- (WHERE depends_on = $1) cannot use it and scans the whole table: 59k seq
-- scans reading 49.9M rows over 22 days in prod. The table is small, so a
-- plain CREATE INDEX (repo convention) is fine here.
CREATE INDEX IF NOT EXISTS idx_task_dependencies_depends_on ON task_dependencies (depends_on);
