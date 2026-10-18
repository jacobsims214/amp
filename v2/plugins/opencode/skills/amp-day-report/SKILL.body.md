# AMP Daily Report Skill

AMP builds a person's day from what actually happened in it: tickets they completed, have
in progress, are blocked on, started, created, and otherwise worked on, across all their
projects. You add the part only a person knows — a short summary — and save it. People in
the user's orgs read saved reports for status updates; agents read them to update other
trackers.

## Wrapping up the user's day

1. **Find their time zone** as an IANA name like `America/New_York` (not `EDT`, which
   `date +%Z` prints): `readlink /etc/localtime | sed 's#.*zoneinfo/##'` on macOS, or
   `timedatectl show -p Timezone --value` (or `cat /etc/timezone`) on Linux. It decides where
   "today" starts and ends. Pass it as `tz` every time; without it the report is in UTC.
2. **Draft:** `amp_day_report(tz=…)` (add `date="YYYY-MM-DD"` for another day). Nothing is
   saved. If a summary was already saved today, it's included — start from it.
3. **Summarise with the user.** Show them what AMP recorded (counts and the notable tickets),
   and write 3–6 lines in their voice:
   - what got done and why it matters (not a list of every ticket — the report has that),
   - what's blocked and on what,
   - what's next.
   Ask them to correct it; they know about work AMP didn't see.
4. **Save:** `amp_save_day_report(summary=…, tz=…)`. Saving again replaces the day's report,
   so it's fine to save early and update later.
5. **Hand back the copy-ready text:** the response's `report.markdown` is ready to paste into
   chat or email. The AMP UI (Daily reports) has the same with a copy button.

Don't invent work. If AMP shows little activity, say so and ask what else happened.

## Reading reports

- Someone's day or week: `amp_list_day_reports(user="name@company.com", from=…, to=…, tz=…)`.
  Anyone in the user's orgs can be read; tickets in projects the user can't see are left out
  (`hidden_items` says how many).
- Velocity: `amp_velocity(user=…, from=…, to=…, tz=…)` — tickets completed per day, by project.

## Syncing to another tracker (Jira, Odoo, …)

Each report has structured lists (`data.completed`, `data.in_progress`, `data.blocked`, …). Every
item carries its project, epic, story, and **effective metadata** — its own fields plus what it
inherits from its project, epic, and story. That's where the mapping lives:

- the project or epic says where things go (e.g. `jira_board: "PLAT"`, `odoo_project: 12`);
- a ticket may already be linked (e.g. `jira_key: "PLAT-123"`).

Update the other tracker from those fields. When you create something there for a ticket, write
the new key back onto the ticket (`amp_update_task(metadata={"jira_key": "PLAT-130"})`) so the next
sync finds it. To find work by its fields directly, use `amp_find_by_metadata`.
