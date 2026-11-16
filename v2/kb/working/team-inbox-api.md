---
title: The team inbox API and MCP — routes, tools, codes and retry shapes
tags: [inbox, requests, api, mcp, rest, idempotency, attachments, go]
---

# The team inbox API and MCP: routes, tools, codes and retry shapes

This is the reference for calling the team inbox from a script, a connector or an agent. The
workflow it serves — who may do what, the lifecycle, delivery and retention — is
`working/team-inbox.md`.

REST and MCP are two adapters over **one service layer**, so every product rule is decided once and
a refusal is the same code on either surface. There is no MCP-only or REST-only behaviour below;
where a term's spelling differs (`state` versus `states`), that is the only difference.

## Routes

`{team id}` is a team's id. On a **write**, the `{request}` segment is the request's **serial**, and
a reference sent there is refused by name rather than searched for, so a caller is told which
spelling the route takes. The detail read and its three sub-reads (discussion, history, lineage)
accept the serial *or* the opaque reference, with `REQ-…` accepted and stripped — the same decoder,
so the two cannot disagree about what a reference may name. The attachment panel is serial-only.

Every route is registered behind the auth proxy, so a signed-in principal is always required — a
URL is never a substitute for signing in. A submit-only credential may reach the submission and
nothing else.

| Family | Routes |
|---|---|
| Intake | `POST /api/teams/{team}/requests` |
| Reads | `GET /api/teams/{team}/requests`, `…/requests/summary`, `…/requests/{request}`, `…/requests/{request}/comments`, `…/events`, `…/links` |
| Triage | `PATCH /api/teams/{team}/requests/{request}`, `POST …/comments`, `…/claim`, `…/release`, `…/assign` |
| Lifecycle | `POST …/transition`, `…/needs-info`, `…/reject`, `…/duplicate`, `…/reopen`, `…/deliver` |
| Lineage | `POST …/links`, `…/handoff` |
| Files | `GET`/`POST …/attachments`, `POST …/attachments/{id}/keep`, `DELETE …/attachments/{id}`, `POST …/attachments/{id}/download`, `GET /api/request-downloads/{token}` |
| Requester | `GET /api/requests`, `GET /api/requests/{reference}`, `POST /api/requests/{reference}/answer`, `…/accept`, `…/reopen` |
| Access | `GET`/`POST /api/teams/{team}/request-grants`, `DELETE …/{user}`, `GET`/`POST …/request-invites`, `DELETE …/{invite}` |

## Request bodies

Every write body is **strict**: an unknown field is refused, a second JSON value is refused, and an
over-size body is refused before it is read rather than after. A field that names a decision the
act must not make is refused **by name with the rule**, not with "unknown field", because a caller
that sent one believes it did something.

**Submit** — `subject` (200), `input_brief` (20 000), `metadata` (50 keys, 64-character keys,
1 000-character values). The idempotency key is a **header**, `Idempotency-Key`, never a body field.
Refused by name: the requester, the contact address, the org, the actor, the source, a project, a
story, a ticket, and a connector's `source_external_id`.

**Lifecycle** — `to` (the generic route only), `question`, `reject_reason` (2 000),
`canonical_request_id`, `outcome_brief` (20 000), `no_artifact`, and `expected_rev`. Each named
route knows its own destination; every evidence field exists on the body so the *policy* refuses one
its move does not read, rather than the decoder dropping it.

**Requester acts** — `answer`, `answer_to`, `expected_rev`. No state and no actor: an answer's
destination is the state the question interrupted, read from the request's history, and acceptance's
is fixed. Omitting `answer_to` answers the newest question.

**Links** — `link_kind`, `target_kind`, `target_id`, `supersede_route`.

**Handoff** — `handoff_key` (required, 64 characters, letters/digits/`-`/`_`) and `items[]`, each
with `ref` (64, letters/digits/`-`/`_`/`.`), `story_id`, `name`, and optionally `plan`,
`acceptance_criteria`, `priority`, `depends_on` (refs in the batch, or existing ticket ids as
`123`/`"123"`/`"#123"`). Refused by name: `project_id`, `epic_id`, `org_id`, `story_id` at the batch
level, `assigned_to`, `assignee`, `agent_id`, `owner`, `start_at`, `schedule`, `decide_by`, `ask`,
`performer`, `human_reason`, `metadata`, `approve`, `dispatch`, `actor`.

**Upload** — `file_name` and `content_base64` (standard base64, no line breaks). A location-shaped
key (`url`, `uri`, `path`) is refused rather than ignored: AMP never fetches a caller-supplied
address.

**Keep** — `{"keep": true}` or `{"keep": false}`. There is no default: `{}` would otherwise mean
unkeep, which is the destructive half.

## The preconditions

A request write is a compare-and-set on the request's revision (`rev`). Send it as `If-Match: 7` or
`"7"`, or as `expected_rev` in the body. Both together are allowed only when they agree.

- `If-Match: *` is **refused** — it means "any current representation", which is exactly the lost
  update the revision exists to prevent.
- A weak validator (`W/"7"`) is refused: `If-Match` is a strong comparison by definition.
- The three triage acts (claim, release, assign) take a revision when the caller has one; zero is
  the service's "whatever the row is now", which is what a replay-tolerant caller passes.
- An **edit** requires one. That is what closes the window between the authorization read and the
  write: a triage that starts in between is a conflict, not an edit that lands after it.

A conflict carries the fact you retry with: `rev` for a stale revision, `claim_user_id` for a lost
claim race. You never need a second read to retry.

## Codes

| Code | Status | Means |
|---|---|---|
| `request.invalid` | 400 | the submission or argument is wrong in a way the caller can fix |
| `request.not_found` | 404 | the request is not there **or** not yours to see — the same bytes, always |
| `request.conflict` | 409 | stale revision, lost claim, second different route, or a key reused with another payload |
| `request.rate_limited` | 429 | too many submissions; carries `retry_after_s` and a `Retry-After` header |
| `token.scope_denied` | 403 | a submit-only credential tried to read, triage or administer |

A 404 is deliberately not an access event and is not audited; a 403 is. A malformed path segment or
a body that is not JSON is an ordinary 400 — it never reached a decision, so it has no `request.*`
code.

**404 is a design decision, not an accident.** A request you may not see, a request that does not
exist, another team, and a team id that was never issued are one answer byte for byte, so ids cannot
be walked. The one exception is deliberate and narrow: an org member who is not on the inbox's team
and holds no grant gets **403** from the submission path, because they can already see the org.

## Idempotency and retries

**Submit.** The key is scoped to (org, team, source, submitter), so a browser's key and an API
client's key are different keys — they have different retry shapes. Same key and same payload →
the same request, nothing written, limiter unit refunded. Same key with a different payload → 409.

REST answers **201** when the call wrote the request and **200** when it answered a replay; the body
carries `created: true|false` either way. MCP answers with the request and `created`. With no key, a
retry submits a **second** request — `amp_submit_request` is deliberately not marked idempotent,
because without a key it isn't.

**Handoff.** Send the same `handoff_key` and the batch reconciles against what the previous attempt
created: existing items come back with `created: false`, and the answer reports `created` and
`already` counts so a client does not count the list. A **new** key is a deliberate re-plan. A 409
means another handoff of the same request is running — retry with the same key. A batch that stopped
part-way says how many tickets it made and is safe to retry as it is.

**Comment.** Post the id an earlier attempt returned as `retry_of_comment_id`; the retry answers
with that same row, unchanged, with its original attribution (`wrote: false`).

**Claim, release, assign.** Idempotent by the service's own semantics: a claim you already hold, a
release with no claim in force and an assignee already set all answer `changed: false` and write
nothing. A claim someone else holds is a 409 naming them; there is no take-over.

**Attach.** Re-sending the same relation writes nothing (`changed: false`) and answers with the row
that already exists, even if its target has since been deleted. The relation itself is the key.

## Bounds

| Bound | Value |
|---|---|
| Subject | 200 characters |
| Input brief, outcome brief, comment body, question | 20 000 characters |
| Reject reason | 2 000 characters |
| Contact snapshot (server-set) | 500 characters |
| Metadata | 50 keys, 64-character keys, 1 000-character values |
| Idempotency key | 255 bytes, scoped (org, team, source, submitter) |
| Submit limiter | 30 per key per fixed one-minute window, **per replica** |
| Page size | 50 default, clamped at 200 (queue, own list, comments, events) |
| Handoff batch | 1–100 items; `handoff_key` and each `ref` 64 characters |
| Attachment | 25 MiB per file, 5 files and 60 MiB per request, name 255 characters |
| Attachment retention | 90 days after the request closes, unless kept |
| Download grant | 5 minutes, single use, bound to the person who minted it |
| Public reference | 16–64 characters, letters/digits/`-`/`_`, never a plain number |

The limiter is a burst guard against a runaway loop or a retrying client — not a quota, not a
security boundary, and not shared between replicas. Authorization is the boundary. A submission the
domain refuses spends no quota (it is validated before the limiter runs), and a replay is refunded
rather than exempted.

## MCP tools

`amp_submit_request`, `amp_list_requests`, `amp_get_request`, `amp_update_request`,
`amp_comment_request`, `amp_claim_request`, `amp_release_request`, `amp_assign_request`,
`amp_attach_request`, `amp_handoff_request`, `amp_upload_request_file` — that is the whole list.
There is **no** tool for the lifecycle, delivery, acceptance, keep/delete or download; those are
REST and the web app.

What the tools map to:

| Tool | Service method |
|---|---|
| `amp_submit_request` | `SubmitRequest` |
| `amp_list_requests` / `amp_get_request` | `ListRequests` / `GetRequest` |
| `amp_update_request` / `amp_comment_request` | `EditRequest` / `CommentOnRequest` |
| `amp_claim_request` / `amp_release_request` / `amp_assign_request` | `ClaimRequest` / `ReleaseRequestClaim` / `AssignRequest` |
| `amp_attach_request` / `amp_handoff_request` | `AttachRequest` / `HandoffRequest` |
| `amp_upload_request_file` | `AddRequestAttachment` |

Two argument differences from the REST spelling, both because MCP tool arguments are typed rather
than strings: `states` is an array of state names (REST: repeated or comma-joined `state`), and
`requester_user_id` is the submitter filter (REST: `submitter`, which also accepts `me`). A
wrong-typed `input_brief` or `idempotency_key` is **refused**, not coerced: a silently dropped key
turns the caller's next retry into a second request.

**Which view you get.** `amp_get_request` answers `{view, request}`: with `team_id` + `request_id`
you get the team's whole record plus its outcome; with `public_ref` you get the requester's
whitelist projection. The `view` field says which, and exactly one of the two shapes is present —
there is no third state and no partially-filled one.

**Authorization is not a project level.** Every request tool runs behind `open`, never `guard`. An
inbox is reached by naming a team, and requiring a project `Level` would be exactly the capability a
submission must not need — a customer, a connector and a submit-only credential all reach a team's
inbox holding zero projects.

## What the credential scopes do

A **submit-only** API token is an attenuation of its account: it may call `POST
/api/teams/{id}/requests` and `amp_submit_request` for the one team it is bound to, and nothing
else. Everything else answers `token.scope_denied`, and the tool list it sees is trimmed to the one
tool — a hidden tool stays callable in MCP, so the refusal does not depend on the client having read
the list. The capability layer narrows the same credential independently (no project level, no org
or team role, no inbox read, an empty visible-project set), so neither layer depends on the other.

Minting one is `POST /api/users/{id}/tokens` with `{"scope": "submit", "scope_team_id": <team>}`,
and the secret is returned exactly once. The **issuer** must already be able to submit to that
inbox: a token is an attenuation of its account, so minting one for a team the account could not
submit to would be an expansion by another name. There is no second mint route — a second definition
of who may hold a narrowed credential is the thing this avoids.

## Two negative rules worth repeating

- **Nothing on this surface approves or dispatches.** A handoff creates tickets that await a
  person's approval; nothing in the attach path, the handoff or any tool calls an approval or a
  dispatch. The requester's text cannot decide an owner, a schedule, a performer or an approval —
  and the adapters refuse an argument that tries, rather than dropping it.
- **Acceptance is unreachable from a credential.** `accept` and `answer` refuse anything that is
  not a person in the web UI with **403**, before the reference is resolved — so a token learns
  nothing about which references exist. There is no on-behalf path. No MCP tool reaches either, and
  no API token can act as the requester.
- **The requester's `reopen` is decided differently, on purpose.** It is not person-only in the
  policy (a team reopens too), so it resolves the reference first and then asks the capability; a
  requester's credential resolves to no capability and gets the surface's **404**. The same rule,
  one step later, and a different code — do not branch on "all requester acts are 403".

## Gotchas

- **`If-Match: *` looks helpful and is refused.** It means "whatever is current", which cannot be
  distinguished from the lost update the revision prevents.
- **The limiter is per replica.** With N replicas a caller can reach N times the limit in one
  window. Do not build a quota on it.
- **The idempotency key lives in a header on REST and an argument on MCP**, and its scope includes
  the **source**: a call retried over the other surface is a different key, by design.
- **An empty aggregate can encode as `null`.** The delivery aggregate's `items` is a nil slice when
  a request has no delivery links, and Go marshals that as `null` — read it as "no rows", not
  "not served".
- **A requester's read of `comments` is absent, not empty.** The requester's arm carries
  `shared_comments` and omits `comments` entirely, so a client can tell "this view has no such list"
  from "the list is empty".
- **The history and the lineage are team-only.** Both answer a requester the surface's single 404:
  they are internal facts with no requester-shaped projection.
