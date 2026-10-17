# Wargame round 2, 2026-10-17: running Northwind like an enterprise team

Round 1 (`wargame-2026-10-17-northwind.md`) proved the core loop: plan, approve, dispatch, review,
day reports, start of day. Round 2 added the things a team inside a large company actually deals
with:
- scope agreed in Slack, not in AMP;
- work moving between people;
- finance and Jira tagging;
- what the agents cost, and whether that spend can be capitalized;
- a standup;
- tracing a code change back to how it came to be.

The cast is the same: Alex and Sam (engineers, each driving their own opencode manager), Morgan
(team lead, in the UI and her own agent). New voices arrive through a simulated `#commerce` Slack
channel: Priya (Product) and Dana (Finance).

## What happened

1. **Finance and Jira tagging.**
   - Morgan turned on required plan approval and set `jira_board` and `cost_center` on the
     project in Settings.
   - She asked her agent to tag every epic `capitalizable` and `jira_epic`.
   - It did, wrote the rule into `decisions/`, and flagged its one disputable call.
2. **Scope creep from Slack.**
   - A customer signed and needed B2B quotes. Sam relayed it to his manager in his own words.
   - The manager planned a 10-ticket epic, reassigned Sam's portal tickets to Alex as agreed in
     Slack, tagged the epic, and wrote a decision doc.
   - It tagged the epic `jira_key` where every other epic uses `jira_epic`.
3. **Self-approval.** Sam approved his own agent's plan. Owner-team membership made him a
   project Owner, which outranks Lead.
4. **Token reporting.** The quotes contract ticket recorded its run: kimi-k3, about 42k in /
   18k out, $0.43, branch `211` @ `3e48284`, pushed.
5. **Tracing a commit.**
   - Morgan traced `3e48284` in the UI back to the ticket, its plan, and the run.
   - Her agent then answered "how did we get to this commit, and what does it cost?" from AMP
     alone: who asked (Slack via Sam), the decision, the worker's choices, the model, and the
     cost.
   - It was honest that most earlier work had no spend data.
6. **Standup and finance.**
   - Morgan wrote the standup from Reports.
   - She answered Finance (spend by Jira epic, capex vs opex) from Portfolio and Reports, with a
     "this is a floor" caveat.
7. **Sick day and handover.**
   - Sam went out sick. Morgan filtered the Board to Sam and the quotes epic and handed his 9
     open tickets to Alex, with the reason posted on each.
   - When Sam came back, his manager found the handover and its reason, the Monday demo, the
     merge queue and the open questions, and recommended leaving quotes with Alex.
8. **Hung workers.**
   - Workers kept going quiet for 20 minutes or more. AMP's quiet flag caught every case;
     managers recorded the abandoned runs and recovered from comments, runs and worktrees.
   - Root cause, found late: workers in sibling git worktrees (`../alex-159`) hit
     external-directory permission prompts that a headless subagent can never answer.

## Platform changes (26 commits)

**Runs: what each agent run used and where its work went**

- `task_runs` (migration 014) stores per run:
  - outcome, agent and model;
  - input, output and cache tokens, and cost;
  - branch, commit, PR, pushed, and (migration 016) merged.
- Recording and reading runs:
  - `amp_complete_task` and `amp_block_task` accept these fields.
  - `amp_report_run` records a run on its own (killed runs, usage a manager learns afterwards, a
    PR opened later).
  - `amp_get_task_runs` lists them.
  - `amp_get_ticket_history` now includes comments and runs, so one call tells the whole story.
- Runs survive deleting the ticket.
- **opencode plugin** `.opencode/plugin/amp-usage.js`: an `amp_session_usage` tool returns the
  agent's own model, tokens and cost from its session. The sync script installs it with its
  dependency.

**Spend and finance**

- **Project Reports:**
  - a Tokens readout and a tokens-by-model panel;
  - tokens and cost columns on the metadata breakdown (so `capitalizable` splits capex/opex),
    epics, people and the standup;
  - tokens per completed ticket by agent profile;
  - abandoned-run (rework) counts;
  - all of it in the Markdown export.
- **Portfolio** (`/api/orgs/{id}/portfolio`): every project in the org with progress, questions
  and spend, by model, with a CSV export. Tracker-agnostic: the columns are whatever metadata the
  projects carry (jira_board, linear_team, ado_area_path, cost_center…), and spend splits by any
  ticket metadata key the viewer picks (capitalizable, cost_type…).
  It warns when runs reported no usage, since spend is then a floor.

**Traceability**

- **Trace** (`amp_trace`, `/api/trace`, sidebar page): from a commit (7+ characters), branch or
  PR URL back to the tickets, their plan, comments, runs and state changes, in projects you can
  see.
- **Waiting to merge:**
  - `amp_list_unmerged` and the Mission panel list done-but-unmerged work, with a Merged button.
  - CI clears the queue with `POST /api/trace/merged`; `docs/ci-merge-reporting.md` has a GitHub
    Actions step.

**People and plans**

- **Handover:** `amp_handover_tasks` and the Board's "Hand over …" move someone's open tickets
  (respecting the Board's filters) with the reason posted on each. Leads may hand over anyone's
  work; anyone may hand over their own.
- **Plan approval** is the engineer's "go" on the plan their agent drafted: any person on the
  project may approve (agents can't). Flight plan shows who approved. (Briefly restricted to
  leads during this round; that was a mistake and is reverted.)
- **Plan check warnings** (WARN, not NO-GO):
  - `coupled_gate`: one ticket joins different people's work.
  - `metadata_drift`: an epic lacks keys its siblings carry, naming the near-miss key it has
    instead.
- The metadata editor suggests keys already in use.
- **Standup:**
  - The Reports panel gains "Blocked by": whose unfinished tickets each person waits on.
  - A **day-report standup sheet** (`amp_project_day_reports`) shows everyone's report for a day
    on the project, and who hasn't written one.
- **Trash:** deleted tickets are snapshotted (migration 015) and can be restored with comments,
  history and links: `amp_list_deleted_tasks`, `amp_restore_tasks`, or the Settings → Trash
  section.
- **amp_list_tasks:**
  - a `quiet` bucket of in-progress tickets silent for 15 minutes;
  - `format=compact` for orientation.
  - My work shows quiet tickets too, and its questions query no longer drops your own past 100
    org-wide.

**Harness template**

- **Execution:**
  - Workers record model, tokens (never estimated), branch, commit, push status and PR.
  - They put the ticket ref in commit messages.
  - They stay inside their ticket's files and inside the project directory.
- **Manager rules:**
  - Dispatch and run the worker in the foreground, at most 4 at a time.
  - Use worktrees inside the repo.
  - Propose only your own user's tickets.
  - Recover quiet tickets at session start.
  - Read the standup sheet.
  - Reflect decisions made outside AMP (scope, handovers, Jira, capitalizable) as tickets,
    assignments, metadata and `decisions/` docs.
  - Record merges.
  - Stop when the AMP tools are missing instead of working around them.
- **Reviewers** wire fix tickets into the next gate.
- **Config:**
  - Workers default to kimi-k3.
  - Provider stream timeouts and a 60-second AMP MCP timeout.
  - The manager gets 60 steps.

After the permission fix, Alex's tickets completed one after another (#161, #210, #159, #154,
#155), each with a full run record: model, tokens, cost, branch, commit, pushed. The abandoned
runs before them show the rework, for example 5 abandoned runs on #161.

## What the round showed

- **Visibility held where it matters to a lead.** Morgan found the stalls (quiet), the merge
  queue, the cross-person blocking and the spend from AMP alone. She ran a standup and answered
  Finance without asking anyone.
- **Traceability works end to end when runs are recorded.** Commit → ticket → plan → decision doc
  → run (model, tokens, cost) → merged. Work done before run recording existed has no spend data
  and won't trace by commit, and the reports say so.
- **Off-platform decisions land in AMP when the human says them to their agent.** The agent put
  them in tickets, assignments, metadata and the KB. The one slip (`jira_key`) is now caught by
  the plan check.
- **Most "agent hangs" were harness configuration, not models.** Headless subagents can't answer
  permission prompts, so anything that triggers one freezes them silently. Keep agents inside
  the project.

## Still open

1. Agents can't learn their usage in every harness. Claude Code reports a subagent's tokens to
   the manager, and the manager prompt says to record them. Other harnesses may need their own
   plugin.
2. The plugin uses opencode's v2 plugin API (`setup` + zod), which isn't in the published plugin
   types yet; it may need updating when opencode documents it.
3. The quiet signal is 15 minutes of no AMP activity. A worker running a long build without
   commenting looks quiet. Workers comment at each step, but a heartbeat could make the signal
   exact.
4. Restore covers tickets, not deleted stories or epics; restoring a ticket whose story is gone
   is refused.
5. Agents can reach the host's Docker: the local-dev-stack worker ran `docker compose up` on the
   host, and the extra containers added to the load when Docker Desktop's VM crashed mid-run. (It
   was restarted, and AMP and the sessions recovered with no data lost.) Templates should run
   dev stacks inside the project's own sandbox or tell agents not to start host services.
6. The demo agents can read files outside their checkout (they found AMP's source). Sandboxing is
   the harness's job, but the template could ship tighter defaults.
