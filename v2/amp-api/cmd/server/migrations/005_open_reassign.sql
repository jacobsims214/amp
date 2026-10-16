-- Project setting: any project member may take or reassign a ticket that
-- nobody is working on (not in progress). On by default, including for
-- existing projects; project owners can turn it off.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS open_reassign BOOLEAN NOT NULL DEFAULT TRUE;
