---
title: Questions — when an agent needs a person
tags: [questions, blocked, answers]
---

# Questions

A worker can't wait for an answer. When it can't go on without a person (a decision, missing
information, access), it comments what it found, calls `amp_block_task` with the question as the
reason (and, for a choice, the `options` with the one it `recommended`), and stops. The ticket shows as a **question**:
- in its owner's **My work** tab, as an **Answer** item (`working/human-work.md`);
- on Mission, in the project's question count and under that person in **Waiting on people**;
- on the Board, and in `amp_list_tasks`' `questions` bucket (and in `waiting_on_people` with
  `kind: "answer"`).

A question waits on the ticket's **owner**. Questions on other people's tickets aren't counted as
waiting on you.

## Answering

Either:
- **In the UI.** In your My work tab (or the ticket panel) the question is quoted. When the
  agent gave options (`amp_block_task` `options` and `recommended`), they're one-tap choices with
  the recommended one marked (never preselected), plus **Other** for your own answer; the Board
  card lists them too. Choose one (and add a note if you like), then **Send answer**: the agent
  reads `Chose: <label>` and your note. A question without options shows suggested answers only
  when its text splits cleanly into choices ("HubSpot or Pipedrive?", or after a colon).
  **Unblock only** puts the ticket back without a comment, for when the answer is already on the
  ticket.
- **Through your agent.** Tell your manager the answer; it relays it with
  `amp_answer_question(task_id, answer)`, or `amp_answer_question(task_id, option=<index>,
  option_label=<its label>)` for one of the question's options. If the question changed since
  it was read (the label no longer matches), the answer is refused rather than given to an option
  you never saw; read it again and answer again. The UI does the same check. AMP shows it as "<your email> (relayed by agent)".

Both post the answer as a comment and put the ticket back to ready. The next worker reads the
comments first, finds the answer, and continues from where the last one stopped.

## Rules for managers

- Don't answer for your user unless the ticket, its comments, or what your user already said this
  session settles it, and then quote where the answer came from. Never answer a question that
  asks for an approval (a plan, an artifact, a sign-off): that's the person's click in the UI.
- Ask your user plainly: the question, the options, the worker's recommendation, the ticket ID.
  Batch several questions together.
- Keep everything else moving. Tickets that depend on the question wait; nothing else should.
- Don't use `amp_set_task_state` or a plain comment to answer; `amp_answer_question` is the one
  path.

## Not a question

A worker that stopped because it needs another ticket's output has found a **missing
dependency**. Add it with `amp_set_task_dependencies`; the ticket runs again when that work is
done.
