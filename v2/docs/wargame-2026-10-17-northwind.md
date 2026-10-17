# Wargame, 2026-10-17: Northwind Commerce

A live run of AMP the way a team uses it: two engineers, each steering their own opencode
manager agent, and a team lead watching from the AMP UI. Nobody scripted the agents; the
engineers typed short, human messages and reacted to what happened. The platform was fixed
between turns wherever it fell short. This doc is what we learned, what changed, and what is
still open.

## Setup

- **AMP:** the local kube stack (docker-desktop, namespace `amp`), two API replicas.
- **People:** Northwind Digital org; Commerce team. Morgan Reyes is the org owner and team lead.
  Alex Chen and Sam Patel are engineers.
- **Harness:** the `v2/.opencode` template, installed into a separate config dir, so it was the
  shipped defaults rather than anyone's tuned setup. Each engineer had:
  - their own clone of one shared origin;
  - their own AMP token;
  - no cloud or GitHub credentials.
- **UI:** each person had a browser on their own account. Morgan watched Mission, Flight plan,
  Reports and Day reports.
- **The ask (Alex, in one paragraph):** a reusable Odoo-style ecommerce template.
  - Next.js backoffice, storefront and customer portal.
  - Go microservices, kube native.
  - Basic features: "the point is a clean solid template".

## How the day went

### 1. Init and planning

The manager created the project under the right org and team, then asked six architecture
questions. The plan came out at 87 tickets, though Alex had said "keep it lean".

Asked to trim, the manager deleted all 87 tickets by an id range and rebuilt the plan at 59. It
recovered honestly when Alex objected.

The KB stayed empty until the plan check started flagging that.

### 2. Plan review

Alex reviewed the plan in Flight plan and approved it in the UI. Morgan watched the same plan
from Mission and the Board.

### 3. Foundation work

The foundation tickets ran on DeepSeek workers.

- Three of them stalled mid-stream with no timeout.
- AMP's new quiet flag is what made the stalls visible.
- The engineers switched their worker model in their own harness config.

### 4. Two engineers in parallel

- Sam's manager onboarded him from AMP and the KB alone.
- Ownership held: Alex's manager never touched Sam's ticket, even when it was ready.
- Cross-person friction showed up:
  - work marked done in AMP but never pushed;
  - one worker edited files outside its ticket and left them uncommitted;
  - one manager blamed the other engineer for those files.
- One review gate made the whole backend wait on a frontend ticket.

### 5. End of day

- Both engineers wrapped up with saved day reports.
- Sam merged, with his explicit OK under the team rule, and filed a ticket for Alex.
- Morgan reviewed the day from Day reports and the new Standup panel.

### 6. Next morning

Fresh sessions with no prior context rebuilt the full picture from AMP and the KB. Then:

- A manager marked two tickets dispatched and ended its turn without running them.
- When asked, it relaunched them as background tasks, which died with the headless run.
- This led to the `quiet` bucket and the session-start protocol below.
- Morgan's agent wrote the org-level playbook.

By the end, the KB had three layers:

- **Project:** 14 docs, covering:
  - architecture overview and service boundaries;
  - contracts and ADRs;
  - how-tos for local dev, CI, the scaffold, shared libraries, the portal and merging.
- **Team:** engineering conventions (branch per ticket, a person merges, no customer data in
  templates, day reports).
- **Org:** how Northwind runs projects with AMP:
  - plan approval;
  - day reports;
  - standup from Reports;
  - human merge;
  - quiet-agent recovery.

## What worked

- **AMP as shared memory.**
  - A new engineer was onboarded without a walkthrough.
  - Fresh morning sessions knew exactly where things stood.
  - Sam's day report carried a handoff (#204 for Alex) that Alex's next session picked up.
- **Ownership.** Each person's manager worked only that person's tickets.
- **Questions instead of guesses.** Managers asked before rebuilding a plan, merging, or
  discarding files. When AMP was unreachable they said so instead of faking progress.
- **Plan check as a steer.** The `no_context` issue got a KB written where skill text alone had
  not.
- **Lead visibility.** Mission, the quiet flag, Standup and day reports let Morgan follow two
  engineers without asking either of them.

## Platform changes made during the run

**UI (`v2/ui`)**

- **Board:** when the default "Mine" filter hides every ticket, it says so and offers to show
  everyone's. Epic and story names wrap instead of truncating to one word.
- **Flight plan:** the selected ticket shows at the top of the side column, and docks to the
  corner on narrower screens.
- **Ticket drawer:**
  - renders the description and acceptance criteria as markdown;
  - opens on Details until the work starts;
  - names the person behind "system" entries;
  - closes on Escape.
- **Mission:**
  - plan-level changes from the audit log (deletes, approval) appear in Activity, collapsed;
  - Crew and Activity lead with the person ("Alex Chen · amp-worker-backend");
  - in-progress tickets silent for 15 minutes are marked quiet.
- **My work:**
  - counts and lists the agents working under my own token;
  - quiet flag;
  - "next up" sort;
  - no project column for single-project users.
- **Reports:** a Standup panel (per person: done, now, waiting on them, ready next, last
  activity), also in the Markdown export.
- **Charts:** one HTML `BarChart` replaces two scaled SVGs whose labels grew with the panel.
- **KB:**
  - one `Markdown` component (there were three copies);
  - saving no longer reports "doc not found" while the write is still queued.
- **Shell:** no stray second scrollbar; long project slugs no longer crowd the sidebar; unknown
  routes go to Mission or a 404 page.

**API (`v2/amp-api`)**

- `amp_check_plan` reports `no_context` while the project KB is empty.
- `amp_list_tasks` returns a `quiet` bucket: in progress with no activity or dispatch for 15
  minutes, plus how long each has been silent.
- `/api/my-work` returns `last_activity_at` for each ticket.
- Day reports count a ticket as blocked only when it is stopped on a question.
- The project code hint asks for a short ticket prefix (2–6 letters).

**Harness template (`v2/.opencode`)**

- **amp-init:** write the overview even for an empty repo; the code is the ticket prefix.
- **amp-planning:**
  - record the user's decisions in `decisions/` before creating tickets;
  - trim a plan in place (list the cuts, delete only those, never by id range).
- **amp-execution:**
  - search the team KB for conventions;
  - stay inside the ticket's files;
  - commit and push before completing, leaving nothing of yours uncommitted;
  - record where the work is ("branch @ sha — pushed?").
- **amp-manager:**
  - dispatch and run the worker in the same turn, in the foreground;
  - no "loop" after the reply;
  - on session start, recover quiet tickets, check questions, and read yesterday's day reports;
  - one set of KB folders.
- **amp-day-report:** how to get an IANA time zone (`date +%Z` is not one).
- **sync-global-config.sh:** works on a machine with no opencode config.

**Helm**

- The Dex login page has an AMP theme (`dex.theme: amp`): bands, wordmark, square panel and the
  same tokens as the UI.

## Still open, in priority order

1. **Where the code is, as data.**
   - `amp_complete_task` could take an optional `{branch, commit, pushed}`.
   - The drawer and Standup would show it.
   - A ticket that becomes ready on unpushed upstream work could warn.
   - Today this lives only in the completion comment's text.
2. **Over-broad gates.**
   - One review gate made every backend service wait on a frontend library.
   - The plan check could flag a gate whose dependents span different owners or stacks.
   - It could also suggest splitting it.
3. **Destructive bulk operations.**
   - The id-range wipe worked because each delete was individually allowed.
   - Options:
     - soft-delete or restore for tickets;
     - a project-scoped bulk delete that lists what it removes;
     - or confirmation above a threshold.
4. **A compact "my plan" view for agents.** `amp_list_tasks` overflowed the manager's tool output
   during onboarding. It should return owner=me tickets by epic, with ids, names, state and what
   each waits on.
5. **Worker model defaults.**
   - The template's `deepseek-v4-flash` workers stalled three times in one day with no timeout.
   - The template should default to a model that held up (kimi-k3 here), or document the risk.
6. **KB editor:** the title is typed twice (Title field plus "# Title" in the body).
7. **Sidebar footer:** shows the system role ("member") for an org owner, which reads like a
   demotion.
8. **The manager's own small errors:**
   - plan arithmetic was wrong;
   - it proposed a person-specific KB doc;
   - it delegated a 3-line `.amp.json` write to a subagent.

## Wargame mechanics worth keeping

- **Headless runs:**
  - Drive sessions with `opencode run --standalone --auto --agent amp-manager --model <agent's
    model> --session <id>`, with stdin from `/dev/null`.
  - `run` ignores the agent file's `model:`.
  - Its interactive `question` tool is auto-dismissed, so deny it in the demo config.
- **Don't edit skills or agents mid-run.** opencode rescans them and the managers see the AMP
  tools drop out ("tool catalog hiccup"). Sync between turns.
- **Port-forwards:** `kubectl port-forward` degrades under agent MCP traffic (every third
  request hangs). A watchdog that probes and restarts it kept the run alive.
