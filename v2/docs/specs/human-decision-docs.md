# Human decision docs (judgment tickets)

Status: Reviewed · Date: 2026-11-04 · Issue: #125 (related: #130 question options, #129 attribution, #124 local identity, #146 decide-by dates and escalation, tracking #141)

## Problem

A human `judgment` ticket is a title, some prose and a **Mark done…** button. In the canonical case, #34
*Confirm: north-star targets* (ticket numbers are illustrative), the owner cannot tell:

- what to decide, or what the options are: there is no structure for options, a recommended
  default or its reasoning (`v2/amp-api/internal/domain/performer.go`: the reason is the only
  structure);
- how the answer reaches the agents: `CompleteHumanTask` (`v2/amp-api/internal/service/human.go`)
  stores the note only as a run. It is not posted as a comment and dependents never receive it. A
  worker sees it only if it calls `amp_get_task` on the dependency with `include=["runs"]`, and
  nothing tells it to;
- who is waiting: My Work's Do cards have no `holding_up` (`service/waiting.go` fills it only for
  merge items), while MCP's `waiting_on_people` fills it for every kind, so the two disagree.

Agents can create a human ticket with only a name: the tool text says the brief is REQUIRED, but
nothing checks it.

The owner's goal: *"I'm struggling to see what this system is doing, what it needs from me, and
what it completed / was it verified."*

**How we will know it is fixed**
- On a project where judgment ticket #34 blocks #41, the owner records a decision on #34 from the
  drawer alone. #41's worker receives the decided text in its `amp_claim_task` and `amp_get_task`
  results with no extra call, and #41's drawer shows it.
- The owner edits #34's agent-drafted doc before recording. The lines the owner changed show as
  the owner's; the rest show as the agent's. The recorded version is the one #41 receives.
- `amp_check_plan` on a project with a judgment ticket that has no brief or no doc returns a
  finding naming that ticket.
- Every judgment ticket recorded through a doc has a frozen version, and every completed judgment
  ticket has a comment "Decision by <name>: …".
- My Work's Do cards show "blocks N · waiting Xd" and are sorted by it.

## Goals / Non-goals

- Goal: a recorded decision reaches every dependent, both the agent (MCP payload) and the person
  (drawer), with no extra call.
- Goal: a judgment ticket carries a structured **ask**: a question, 2 to 4 options with
  consequences, a recommended option with reasoning, and evidence links. Options use one shape,
  `domain.AskOption`, which #130 reuses for agent questions.
- Goal: a judgment ticket opens as a **decision doc** that an agent drafted. The owner edits it,
  agent text and owner text look different, and **Record decision** freezes the version that
  dependents receive.
- Goal: the agent can draft but never record the decision.
- Goal: My Work shows how many tickets wait on each Do item, and for how long.
- Non-goal: embedding or depending on Proof / proof-sdk. Only the interaction model is borrowed.
- Non-goal: real-time co-editing (CRDT/OT) or a WYSIWYG editor. The doc is markdown saved as
  artifact versions, edited in a textarea with a preview, one writer at a time with a
  stale-version check.
- Non-goal in this change (see Follow-ups): asking the agent inline, tracked suggestions,
  presence, a recorded checklist, decide-by dates and escalation (#146).
- Non-goal: per-reason templates. Only `judgment` gets the ask and the doc.

## Journeys

**J1. Owner records a decision (main path).**
Given judgment ticket #34, created by the planner with an ask (question, three options,
recommended B), so its decision doc v1 was drafted from that ask,
When the owner opens #34, the drawer shows the decision card with option B marked "Recommended"
and "blocks 5 · waiting 2d" in its header. The owner clicks **Edit doc**, tightens option B's
label in the option editor, saves (v2), picks B, types "Go with B; keep trial at 14 days" and clicks **Record
decision**,
Then v2 is frozen (artifact approved at v2, kept, never expires) and #34 is completed. A comment
"Decision by Jacob: B: Activation rate (40%, floor 30%) — Go with B; keep trial at 14 days" is
posted, using B's label as it is stored on v2. Dependents' drawers show "Decided by Jacob on #34: B — …",
and the next `amp_claim_task` on #41 returns it in `upstream_decisions`.

**J2. The agent tries to record the decision (refusal).**
Given the same ticket,
When the manager agent calls `amp_complete_task(task_id=34)`, `amp_review_artifact(approve=true)`
on its doc, or `amp_publish_artifact` to the doc,
Then the first two are refused with "Only a person records a decision, in AMP: <ticket link>", the
third with "Decision docs change in AMP, not by publish: <ticket link>", and nothing changes.

**J3. Legacy ticket, or a person's judgment ticket, without a doc.**
Given a judgment ticket with no doc (created before this change, or by a person in the UI, which
has no ask editor),
When the owner opens it,
Then the drawer shows the brief and a required **Your decision** field. Mark done posts the
decision comment and fills `upstream_decisions` the same way as J1. `amp_check_plan` warns about
it (`judgment_without_doc`, severity warn).

**J4. A changed mind.**
Given #34 is recorded,
When the owner wants a different answer,
Then the doc is read-only. The owner (or the planner) creates a new judgment ticket that depends
on #34 and says what it supersedes. There is no reopen.

## Design

### Overview

```
 planner agent ──amp_create_tasks{performer:human, human_reason:judgment, ask:{…}}──▶ AMP API
                                     │ validates ask, renders doc v1 (artifact role=decision, author_kind=agent)
 owner (drawer) ─ edit (new version, author_kind=person) ──────▶ artifact_versions
                ─ Record decision (UI only) ───────────────────▶ decision_docs.decided_* + artifact approved+kept
                                                                 + ticket completed + comment
 dependents     ◀─ upstream_decisions (amp_get_task, amp_claim_task, GET /api/tasks/:id/decisions)
 every write    ─▶ hub event decision.updated (SSE) ─▶ open drawers refresh
```

**One source for each fact.**

| Fact | Stored in | Not stored in |
|---|---|---|
| Question, options, recommended index | the doc version's `decision_ask` metadata (`AskOption[]`), beside its markdown | not in `tasks` (no `ask` column), not parsed from markdown |
| Context, trade-offs, why the recommendation | the doc version's markdown body | not in the metadata |
| What "decided" means | the ticket's acceptance criteria | not in the doc markdown, not in the ask |
| The decision | `decision_docs` (doc tickets) or the human run note (J3) | — |

### 1. Decision write-back (Phase 1, no migration)

- `CompleteHumanTask` posts the note as a ticket comment after recording the run: `Decision by
  <name>: <note>` for `judgment` and `approval`, `Done by <name>: <note>` for other reasons, and
  nothing when the note is empty. `<name>` is the user's `display_name`, falling back to the email.
  It uses the existing `s.comment` helper.
- For `judgment` and `approval` the note is required on the server too: "a decision ticket needs
  your decision: say what you decided".
- `Service.UpstreamDecisions(ctx, p, taskID)` (new `service/decisions.go`, needs project
  membership) reads the ticket's direct dependencies that are completed human tickets. For each it
  returns the `decision_docs` record once it is claimed (§3), otherwise the non-empty note of its
  latest human run. A dependency with neither is left out.

  ```go
  type UpstreamDecision struct {
      TaskID   int       `json:"task_id"`
      Name     string    `json:"name"`
      Reason   string    `json:"human_reason"`
      Decision string    `json:"decision"`         // the note, or option + decided text
      Option   string    `json:"option,omitempty"` // chosen option label (doc tickets)
      DocURL   string    `json:"doc_url,omitempty"`
      Version  int       `json:"version,omitempty"`
      By       *int      `json:"decided_by"`
      ByName   string    `json:"decided_by_name"`
      At       time.Time `json:"decided_at"`
  }
  ```
- Agents get it without asking: `amp_get_task` and `amp_claim_task` add `upstream_decisions` when
  non-empty, and `claimNext` adds "Decisions people made that this ticket depends on are in
  upstream_decisions; follow them."
- People get it from `GET /api/tasks/:id/decisions` → `{upstream: UpstreamDecision[], own:
  DecisionDoc|null}`. The drawer shows a "Decided upstream" strip on any ticket that has one:
  "Decided by Jacob on #34: B — …", linking to #34. The strip says "Done by" for non-decision
  reasons.

### 2. Brief rule and the ask (Phase 2)

`domain/decision.go` (create):

```go
type AskOption struct {
    Label       string `json:"label"`       // "B: Activation rate (40%, floor 30%)"
    Consequence string `json:"consequence"` // trade-offs, cost
}
type Ask struct {
    Question    string      `json:"question"`
    Options     []AskOption `json:"options"`     // 2..4
    Recommended int         `json:"recommended"` // index into Options
    Reason      string      `json:"recommendation_reason"`
    Evidence    []string    `json:"evidence,omitempty"` // URLs, "#N", "artifact #N"
}
func (a *Ask) Validate() error
```

- **The ask is input, not storage.** `amp_create_task`, `amp_create_tasks` items and
  `amp_update_task` take an `ask` object. The server validates it and renders doc v1 from it (§3).
  It is not saved on the ticket. Reads (`amp_get_task` → `decision`, `GET
  /api/tasks/:id/decision`) return the current version's stored options. `amp_update_task(ask=…)`
  works only while a judgment ticket has no doc (it creates one); after that it is refused: "This
  ticket's options are in its decision doc; the owner edits them in AMP: <ticket link>". #130's
  agent questions use the same `AskOption` but stay a separate object (D7).
- Example `ask` (the ticket also has its description and acceptance criteria):

  ```json
  {"question":"Which north-star metric do we commit to for the next 90 days?",
   "options":[{"label":"A: Weekly active teams","consequence":"Easy to measure; rewards seats over value"},
              {"label":"B: Activation rate (40%, floor 30%)","consequence":"Needs the onboarding event (#41)"},
              {"label":"C: Net revenue retention","consequence":"Lags 90 days; can't steer this quarter"}],
   "recommended":1,"recommendation_reason":"B is the only one we can move and measure inside the quarter.",
   "evidence":["artifact #1","#3"]}
  ```
  `question`, `options` and `recommended` become the version's metadata;
  `recommendation_reason` and `evidence` are written into the markdown.
- **Brief rule.** A human ticket of any reason, on any channel, needs a non-empty description and
  acceptance criteria. A `judgment` ticket created over MCP or an API token (`!p.InUI()`) also
  needs a valid `ask`. The UI has no ask editor, so a person's judgment ticket without one follows
  J3. One check, `domain.HumanBriefMissing(desc, criteria, reason string, ask *Ask) []string`, is
  called from `CreateTask`, `CreateTasks` (the error names the ref and the missing fields) and
  `UpdateTask`. `UpdateTask` runs it only when the update makes a ticket human or judgment, or
  clears a brief field, so legacy tickets stay editable. Agent tickets are unchanged. The UI's
  new-ticket and edit forms mark both fields required for a person's ticket.
- `recommended` only marks a badge; choosing is always a person's click (D8). MCP field help says
  a human ticket's brief is "what the person must do, written for them (required)", and the
  human-ticket examples drop "review and merge a PR" (merges are Merge needed).
- **Plan check** (`service/plan.go`, beside `human_without_owner`): `human_without_brief` for open
  human tickets with an empty description or criteria, and `judgment_without_doc` for open
  judgment tickets with no doc, both `warn`. `PlanCheckModal.tsx` gets titles for both.

### 3. The decision doc

**Storage (Phase 2, migration `046_decision_docs.sql`).** It reuses artifacts: kind `markdown`, a
new nullable `artifacts.role` (`'decision'`), one doc per ticket.

```sql
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS role TEXT;
ALTER TABLE artifact_versions ADD COLUMN IF NOT EXISTS author_kind TEXT NOT NULL DEFAULT 'person'
    CHECK (author_kind IN ('person','agent'));
ALTER TABLE artifact_versions ADD COLUMN IF NOT EXISTS decision_ask JSONB; -- {question, options: AskOption[], recommended}; decision docs only
CREATE TABLE IF NOT EXISTS decision_docs (
    task_id              INT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    artifact_id          INT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    decided_version      INT,
    chosen_option        INT,          -- index into vN's decision_ask.options; NULL = "something else"
    decision_text        TEXT,
    decided_by           INT REFERENCES users(id) ON DELETE SET NULL,
    recording_started_at TIMESTAMPTZ,  -- Record decision's claim
    run_id               INT REFERENCES runs(id) ON DELETE SET NULL,
    decided_at           TIMESTAMPTZ
);
```

**Creation (Phase 2).** When a judgment ticket gets a valid ask, v1 (`author_kind='agent'`) is
written in two parts:

- **metadata** `decision_ask`: the question, the options (`AskOption[]`) and the recommended index,
  copied from the ask;
- **markdown body** (prose only): `## Context` (epic › story, evidence links, "Waiting on this:
  #41 #42"), `## Trade-offs` (a paragraph per option), and `## Why we recommend <label>` (the
  reason).

"Decided means" is in neither: the card renders the acceptance criteria (§5).
`Repo.CreateDecisionDoc` writes the artifact (`role='decision'`, `keep=true`, no expiry), v1 and
the `decision_docs` row in one transaction. If it fails, the ticket is still created, with the
warning "decision doc not created; set ask again with amp_update_task". Since no doc exists, that
retry is allowed.

**Options are version metadata.** Every decision-doc version stores its `decision_ask`, validated
on every write (`Ask.Validate`: 2 to 4 options, each with a label and consequence, recommended in
range). A write that fails validation is refused with the reason, so an edit can never leave a
version with broken options. `chosen_option` indexes vN's options, and the recorded label is
`vN.decision_ask.options[i].label`. So an edited option is what gets recorded.

**Provenance (Phase 3).** Each version's `author_kind` is fixed by its write path: the rendered v1
is `agent`, and `PUT doc` (UI only) is `person`. The UI computes line blame. For each line of the
shown version it finds the version that introduced it, using `lineDiff` from
`ui/src/lib/textDiff.ts` across consecutive versions. Agent lines get a left rule in the accent
colour and an "Agent" tag on hover; owner lines have no mark. Options are compared the same way:
an option whose label or consequence the owner changed is the owner's. `GET /api/tasks/:id/decision`
returns `versions: [{version, author_kind, text, ask}]`, because version content is not in
`ArtifactVersion`'s JSON. `MaxArtifactVersions` (100) bounds the work.

**Who may act.** Edit and Record need `authz.TaskWork` (as Mark done) and `p.InUI()` (D17).
Reading needs `authz.TaskRead`.

**Editing (Phase 3).** `PUT /api/tasks/:id/decision/doc {base_version, content, ask}` writes both parts
as one version, in a transaction that locks the `decision_docs` row (`FOR UPDATE`). It refuses a
claimed or decided doc, an invalid `ask`, and a stale `base_version` (409; the UI keeps the draft
and says "The doc changed in another tab. Reload to see it"). At 100 versions it returns 409 "This
doc has 100 versions; record the decision or start a new judgment ticket".

**Guards (Phase 2, when docs first exist).** For a decision doc these checks run before
`refuseAgentAccount` and the `p.Agent` checks, so agent accounts get the same strings:

| Call | Result |
|---|---|
| `PublishArtifact` to the doc, any caller | "Decision docs change in AMP, not by publish: <ticket link>" |
| `amp_complete_task`, or `ReviewArtifact(approve)` by an agent | "Only a person records a decision, in AMP: <ticket link>" |
| `ReviewArtifact` or `KeepArtifact(false)` by a person | "Decision docs are approved and kept by Record decision: <ticket link>" |
| Mark done on a ticket with a doc (Phase 3 on) | "Record the decision on its doc: <ticket link>" |
| Any doc write after the record is claimed | "This decision is recorded and can't change: <ticket link>" |

`<ticket link>` is `s.TaskURL(project, task)`, and `<doc link vN>` is `/artifacts/<id>?v=N`.

**Record decision (Phase 3)** freezes vN, its markdown and its options together. `POST /api/tasks/:id/decision/record {version, chosen_option|null,
decision_text}`. `Service.RecordDecision`:

1. checks who may act, that `decision_text` is non-empty, and that `chosen_option` is in range of
   vN's options or null;
2. **claims** under the same row lock as edits: it stores version, option, text, `decided_by` and
   `recording_started_at`, but only if `version` is current (else 400), the doc is undecided, and
   it has no claim or one older than 1 minute (else 409 "already being recorded" / "decided").
   From the claim on, every doc write is refused, and dependents already read the claimed values;
3. calls `CompleteHumanTask` with note `"<option label> — <decision_text>\n<doc link vN>"`
   (without the label when the option is null). Only while `decision_docs.run_id` is NULL, it
   records the run, posts the comment and sets `run_id`. `CompleteHumanTask` refuses a judgment
   ticket that has a doc unless `RecordDecision` calls it;
4. in one transaction, approves the artifact at vN and sets `decided_at`.

If a step after the claim fails, the claim goes stale after a minute and the button reads
"Recording didn't finish. Retry"; a retry finishes with the **stored** values. After recording the
doc is read-only, with no reopen (J4).

**Live updates.** Every decision write (create, edit, record) publishes `decision.updated`
`{task_id, version}` from the REST and MCP handlers, like `artifact.updated`; it joins
`domain.AllEventTypes` and the UI's `SSEEvent` union, and open drawers reload through `useSSE`.

### 4. Who waits on a Do item (Phase 1)

`WaitingOn` (`service/waiting.go`) collects the ids of `do` and `answer` items and calls
`Repo.OpenDependents` once, together with the merge ids. `OpenDependents` becomes transitive (a
recursive query over open downstream tickets), the same count as MCP's `waiting_on_people`
(`mcp/human.go`), so the two views agree. Do items sort by `len(holding_up)` descending, then by
age. `ProjectMyWork.tsx` Do cards show "blocks N · waiting Xd" (or "Nothing waits on it ·
waiting Xd"). Deadlines and escalation are not part of this (#146).

### 5. The decision card (UI)

`ui/src/components/human/DecisionCard.tsx` is built **once**, in Phase 2. Its options section is
`OptionPicker`, fed by the current version's stored options. Phase 3 adds Edit doc,
provenance marks and Record decision inside the same card. It replaces nothing.

```
┌ You decide · Jacob does this · blocks 5 (#41 #42 #43 → #44 #45) · waiting 2d ─────┐
│ Product › North star · judgment · drafted by planner-agent · v2                    │
├────────────────────────────────────────────────────────────────────────────────────┤
│ QUESTION  Which north-star metric do we commit to for the next 90 days?            │
│ OPTIONS                                                                            │
│ ○ A  Weekly active teams — easy to measure; rewards seats over value               │
│ ◉ B  Activation rate (40%, floor 30%) — needs #41's event        [Recommended]     │
│ ○ C  Net revenue retention — lags 90 days                                          │
│ ○ Something else                                                                   │
│ WHY B  ▍"B is the only one we can move and measure inside the quarter."   Agent    │
│ EVIDENCE  artifact #1 North-star proposal v2 · #3                                  │
│ DECIDED MEANS  • Metric chosen  • Target and floor set  • Trial length confirmed   │
├────────────────────────────────────────────────────────────────────────────────────┤
│ [Edit doc]                                                                         │
│ Your decision: [Go with B; keep trial at 14 days_______________]                   │
│                         [Accept recommendation]   [Record decision]                │
└────────────────────────────────────────────────────────────────────────────────────┘
```

- **Edit doc** opens the markdown textarea and an option editor (2 to 4 rows of label and
  consequence, plus a recommended radio, Phase 3). Save writes both into one new version.
- **Accept recommendation** selects the recommended option and fills "Your decision" with its
  label. The owner still clicks Record decision.
- **Decided means** is the acceptance criteria, rendered read-only with `checklistItems`
  (`ui/src/lib/brief.ts`, shipped in #135). Ticking and recording the items is a follow-up.
- In Phase 2, before Record decision exists, the card's button is Mark done. It sends the note
  `"<option label> — <decision text>"` through `CompleteHumanTask`, the same as J3. Phase 3 swaps
  the button for Record decision. The card itself does not change.
- After recording, the card shows "Decided by Jacob · v2 · <date>" and the doc is read-only.
- The Artifacts page gets a "Decisions" filter (`role=decision`, approved). There is no separate
  decision log page.

## Alternatives

- **Embed proof-sdk.** A second document store, auth model and agent bridge; the owner decided
  against it (D3).
- **A text field and no doc.** Ships as Phase 1, but has no options, provenance or frozen version
  (D1).
- **Options as `tasks.ask`, or parsed from `###` headings in the markdown.** The first gives two
  sources once the owner edits; the second breaks silently when an edit changes a heading.
  Rejected for per-version metadata (D14).
- **Character-span provenance marks.** Need a structured editor to stay in sync.

## Risks

- **The new brief rule breaks planners and scripts that create bare human tickets.** Mitigation:
  the error names the ref and the missing fields, existing tickets only get a plan-check warning,
  agent tickets are untouched, and the planning skill and kb change in the same PR.
- **Lost owner edits on a version conflict.** Mitigation: the 409 keeps the draft in the textarea.
- **Agent impersonating the owner** with the person's token. Mitigation: edit and record are UI
  only, authorship comes from the channel, and the agent has no write path to the doc after v1.
- **Decision docs purged by artifact retention.** Mitigation: `keep=true` at creation (a ticket can
  wait longer than the 45-day default), and unkeeping is refused.
- **Decisions waiting too long.** Mitigation in v1: "blocks N · waiting Xd" and the sort order.
  Deadlines and escalation are in #146.
- **Migration number collision** with PRs in flight (#126). Mitigation: one additive file
  (`046_decision_docs.sql`), renumbered on rebase when another PR takes the number (it was 043 until #143 took 043–045).
- **Rollback.** New columns are nullable or defaulted, and old clients ignore new keys. The brief
  rule is one function and can be reverted alone.

## Decisions & Assumptions

| ID | Decision or assumption | Why | By |
|----|------------------------|-----|----|
| D1 | A judgment ticket becomes a collaborative decision doc: agent-drafted question, context, options with trade-offs, recommended default with reasoning; the owner edits; visible provenance; Record decision freezes a version dependents receive. Inline asks, suggestions and presence are the stated direction (Follow-ups) | Owner direction for #125 (H1) | human |
| D2 | The agent may draft, comment and suggest but can never record the decision | Owner direction (H1) | human |
| D3 | Proof is a concept reference only; build natively; no proof-sdk dependency | Owner: "we dont need to swap proof into it, just the concept as a feature ability" (H2) | human |
| D4 | The decision doc is built on AMP artifacts (versions, approval, keep) | Owner direction (H3) | human |
| D5 | Related changes ship in one PR, with commits that each leave the build green | Owner's process constraint | human |
| D6 | v1 is Phases 1–3. Ask-the-agent, tracked suggestions and presence, and the recorded checklist, move to follow-ups | Owner, 2026-11-04, accepting the zoom-out scope finding | human |
| D7 | The agent's question (#130) and the judgment ticket are two objects that share `AskOption` | Owner, 2026-11-04 (was Q1) | human |
| D8 | The agent recommends but never pre-selects; only a person's click chooses | Owner, 2026-11-04 (was Q2) | human |
| D9 | No due dates or auto-escalation in v1; show "blocks N · waiting Xd" and sort My Work by it. Decide-by dates and escalation are #146 | Owner, 2026-11-04 (was Q3) | human |
| D10 | Harness auto-pickup of decision asks is a follow-up, with the ask feature | Owner, 2026-11-04 (was Q4) | human |
| D11 | No reopen; a changed decision is a new judgment ticket that supersedes the old one | Owner, 2026-11-04 (was Q5); reopening would un-start work that consumed the decision | human |
| D12 | No decision log page; the Artifacts page gets a "Decisions" filter | Owner, 2026-11-04 (was Q6) | human |
| D13 | No recorded checklist for agent tickets; agent verification is #126 | Owner, 2026-11-04 (was Q7) | human |
| D14 | Options have one stored source: structured `AskOption[]` metadata (`artifact_versions.decision_ask`) on each decision-doc version, validated on every write, written by the card's option editor, and frozen with the version by Record. The markdown holds prose only. `ask` is tool input that seeds v1; there is no `tasks.ask` column and no parsing of headings | zoom-out finding (`tasks.ask` was a second source); coordinator review (parsing headings broke silently on owner edits) | agent |
| D15 | "Decided means" has one source, the acceptance criteria, rendered under the doc and not in its markdown | zoom-out finding: it was stored twice | agent |
| D16 | `DecisionCard` and `OptionPicker` are built once, in Phase 2; Phase 3 adds edit, provenance and record to the same card | zoom-out finding: Phase 3 replaced Phase 2's card | agent |
| D17 | Version authorship comes from the write path (rendered v1 = agent, `PUT doc` = person) and the channel (`p.InUI()`), never the user id | Managers use their person's token | agent |
| D18 | Provenance is line blame across stored versions | No structured editor | agent |
| D19 | The brief rule applies to every human reason on every channel; `ask` is required only for judgment and only off the UI; the update check runs only when a ticket becomes human or judgment, or a brief field is cleared | Legacy tickets stay editable; the UI has no ask editor | agent |
| D20 | `human_without_brief` and `judgment_without_doc` are always `warn` | Don't freeze existing plans on deploy | agent |
| D21 | Record decision claims the row first, under the edit lock; a stale claim (1 minute) is retried with the stored values; `run_id` keeps run and comment to one | Serializes records and edits | agent |
| D22 | Decision docs are kept from creation; review and unkeep are refused | A record must outlive retention | agent |
| D23 | `upstream_decisions` covers direct dependencies only | Small payload | agent |
| D24 | My Work's `holding_up` counts open dependents transitively, like `waiting_on_people` | The two views must agree | agent |
| D25 | `decision.updated` is published by the REST and MCP handlers | `Service` has no hub | agent |
| D26 | Code comments that cite the missing `decisions/human-work-ux.md` point to this spec and `v2/kb/working/human-work.md` | That file exists nowhere | agent |

## Open questions

None open. The owner answered all seven on 2026-11-04 (D7–D13). Deferred work is listed below and
in #146.

## Follow-ups (not in this change)

The direction stays as decided in D1–D4. Each item below is its own issue, linked from #125:

1. **Ask the agent, tracked suggestions, presence.** The owner selects text or writes in a thread
   and asks the agent. A connected agent replies with a comment or a tracked suggestion that the
   owner accepts or rejects, and an accepted suggestion becomes a version marked "Agent · accepted
   by <name>". A presence line shows "<agent> is drafting…" and goes stale after 2 minutes. The
   honest "No agent has picked this up" state covers the case where no agent is connected.
   Constraints carried over: built natively on the artifact versions above (D3, D4); agents only
   comment or suggest, and every agent change passes through the owner; the agent never records
   (D2); suggestions anchor on exact quotes. This adds `decision_notes`, `artifact_versions.accepted_by`
   and the `amp_decision_get / _comment / _suggest / _activity` tools.
2. **Harness auto-pickup of decision asks** (`v2/harness`), so that unanswered asks are rare. Comes
   with item 1.
3. **Checklist recorded on completion.** The person ticks the acceptance-criteria items in the
   drawer. Mark done and Record decision send `checked` and store it on the run
   (`runs.checked_items`), and dependents see what was confirmed. Human tickets only (D13).
4. **Decide-by dates and escalation when overdue:** #146.

## Implementation plan

One PR (D5), three commits, with the build green after each. Paths are relative to `v2/`.

### Phase 1: Decisions reach dependents (no migration)

- `amp-api/internal/domain/decision.go` (create): `UpstreamDecision`.
- `amp-api/internal/service/human.go`: require a note for judgment and approval; post the
  `Decision by` / `Done by` comment.
- `amp-api/internal/service/decisions.go` (create): `UpstreamDecisions`, from human run notes.
- `amp-api/internal/mcp/server.go`, `mcp/next.go`: `upstream_decisions` on get and claim; the
  claim sentence.
- `amp-api/internal/api/rest.go`: `GET /api/tasks/:id/decisions` → `{upstream, own: null}`.
- `amp-api/internal/service/waiting.go`, `repository/waiting.go`: transitive `holding_up` for
  do and answer items, plus the sort.
- `ui/src/api/client.ts`, `ui/src/types/index.ts`: `getTaskDecisions`, `UpstreamDecision`.
- `ui/src/components/human/HumanActions.tsx`: for judgment and approval, a required "Your
  decision" field, with the error "Write what you decided".
- `ui/src/components/TaskDrawer.tsx`: the "Decided upstream" strip.
- `ui/src/pages/ProjectMyWork.tsx`: "blocks N · waiting Xd" on Do cards.
- Code comments that cite `decisions/human-work-ux.md` are repointed (D26). Applied migration SQL
  is left untouched.
- `kb/working/human-work.md`: "Your decision" and how dependents receive it.

### Phase 2: Brief rule, structured ask, option picker

- `amp-api/cmd/server/migrations/046_decision_docs.sql` (create): §3 storage.
- `amp-api/internal/domain/decision.go`: `AskOption`, `Ask`, `Validate`, `HumanBriefMissing`,
  `DecisionDoc`.
- `amp-api/internal/domain/artifact.go`, `repository/artifacts.go`: `Role`, `AuthorKind`,
  `DecisionAsk` on versions.
- `amp-api/internal/repository/decisions.go` (create): `CreateDecisionDoc`, `GetDecisionDoc`.
- `amp-api/internal/service/decisions.go`: `renderDecisionDoc`, doc creation from `CreateTask(s)`
  and `amp_update_task(ask=…)` (with the warning on failure), and `GetDecision` (versions with
  their options). `UpstreamDecisions` prefers `decision_docs`.
- `amp-api/internal/service/plan.go`, `service/approval.go`: the brief rule and the two
  plan-check kinds; `UpdateTask` refuses `ask` once a doc exists.
- `amp-api/internal/service/artifacts.go`: the §3 guards for publish, review and unkeep.
- `amp-api/internal/mcp/server.go`, `mcp/instructions.go`: the `ask` property (input only),
  `decision` on `amp_get_task`, the field help, and dropping "review and merge a PR".
- `amp-api/internal/api/rest.go`: `GET /api/tasks/:id/decision`; `/decisions` fills `own`.
- `amp-api/internal/domain/models.go`, `ui/src/types/index.ts`: `EventDecisionUpdated` in
  `AllEventTypes` and in the `SSEEvent` union; `Ask`, `AskOption`, `DecisionDoc`.
- `amp-api/cmd/server/*_test.go` fixtures that create bare human tickets get a brief.
- `ui/src/components/human/OptionPicker.tsx`, `DecisionCard.tsx` (create): the §5 card, with Mark
  done as the action.
- `ui/src/components/human/HumanWork.tsx`: render `DecisionCard` when a doc exists.
- `ui/src/pages/Board.tsx`, `TaskDrawer.tsx`: brief fields required for a person's ticket.
- `ui/src/components/PlanCheckModal.tsx`: titles for the two kinds.
- `ui/src/pages/Artifacts.tsx`: the Decisions filter.
- `plugins/amp/skills/amp-planning/SKILL.md`, `kb/working/human-work.md`,
  `kb/working/planning-and-approval.md`: the brief rule, the ask with the #34 example, the
  findings.

### Phase 3: The decision doc and Record decision

- `amp-api/internal/repository/decisions.go`: `SaveDecisionVersion` (lock, base check, cap),
  `ClaimDecisionRecord`, `FinishDecisionRecord`.
- `amp-api/internal/service/decisions.go`: `SaveDecisionDoc`, `RecordDecision` (§3, with retry).
- `amp-api/internal/service/human.go`: `CompleteHumanTask` refuses a judgment ticket with a doc
  unless `RecordDecision` calls it.
- `amp-api/internal/api/rest.go`: `PUT /api/tasks/:id/decision/doc` and `POST
  /api/tasks/:id/decision/record`, each publishing `decision.updated`.
- `ui/src/lib/provenance.ts` (create): `blame(versions)` built on `lineDiff`.
- `ui/src/components/human/DecisionCard.tsx`: Edit doc (textarea, preview, option editor, 409 handling),
  provenance marks, Record decision in place of Mark done, the read-only state, and the
  `decision.updated` subscription.
- `kb/working/human-work.md`, `kb/working/artifacts.md`: decision docs and Record decision.

## Verification

Each phase:

```sh
cd v2/amp-api && go vet ./... && go test -race -count=1 ./...
cd v2/amp-api && TEST_DATABASE_URL=postgres://amp:amp@localhost:5432/amp_test?sslmode=disable go test -race ./cmd/server/ -count=1 -timeout 120s
cd v2/amp-api && TEST_CLUSTER=1 TEST_DATABASE_URL=… go test -race -count=1 ./cmd/server   # cross-replica, as CI does
cd v2/ui && npx tsc -b && npm run lint && npx vitest run
```

**Phase 1**
- `cmd/server/human_test.go`: a judgment note is posted as a comment; an empty one is a 400; a
  `physical` ticket with no note posts nothing; get and claim on a dependent return
  `upstream_decisions`, leaving out incomplete and empty-note dependencies.
- `cmd/server/waiting_on_test.go`: do and answer items get a transitive `holding_up` equal to
  `waiting_on_people`, and do items sort by it.
- `HumanActions.test.tsx`, `TaskDrawer.humanBrief.test.tsx`: the required "Your decision" field;
  the strip links #34.

**Phase 2**
- `domain/decision_test.go`: `Ask.Validate` (1 or 5 options, recommended out of range, missing
  consequence) and `HumanBriefMissing`.
- `cmd/server/plan_test.go`: the brief rule by channel and reason (MCP 400 naming the ref; agent
  ticket unchanged; UI judgment ticket without ask created; switching to human checked; legacy
  rename allowed); both plan-check kinds are `warn`.
- `cmd/server/decisions_test.go`: an ask creates v1 (`author_kind=agent`, `role=decision`,
  `keep=true`, `decision_ask` equal to the ask, prose-only markdown) and no stored ask on the
  ticket; `amp_update_task(ask=…)` creates a missing doc and is refused once one exists; a forced
  creation failure returns the warning; publish, review and unkeep are refused with the §3 strings,
  agent accounts included.
- `DecisionCard.test.tsx`: the badge, Accept recommendation, Something else, Mark done sending
  "<label> — <text>", and Decided means listing the criteria.

**Phase 3**
- `cmd/server/decisions_test.go`:
  - `PUT doc`: stale base → 409; MCP or token → refused; 100 versions → 409; one option → 400;
  - `record` at v2 approves v2, completes the ticket, posts the comment, and gives the dependent
    the label, text and `version: 2`; a renamed label records the new label;
  - stale version → 400; repeat or concurrent record → one 200 and one 409; `PUT doc` racing a
    record → exactly one succeeds; Mark done on a doc ticket → 409;
  - with the run write forced to fail, the dependent already sees the claim and a retry records
    the run and comment once;
  - each write emits one `decision.updated`.
- `provenance.test.ts`: per-line and per-option blame across an agent v1 and a person v2.
- `DecisionCard.test.tsx`: a 409 keeps the draft; Record needs decision text; read-only after.

**Manual check** (`docker compose up`, with the J1 setup created with `amp_create_tasks`): open
#34, edit option B, record B, then open #41. The strip shows the decision, and `amp_claim_task`
on #41 returns it.
