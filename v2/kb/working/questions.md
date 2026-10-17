---
title: Questions — when an agent needs a person
tags: [questions, blocked, answers]
---

# Questions

A worker can't wait for an answer. When it can't go on without a person (a decision, missing
information, access), it comments what it found, calls `amp_block_task` with the question as the
reason, and stops. The ticket shows as a **question**: in `amp_list_tasks`' `questions` bucket,
in My work, in Mission and on the Board.

## Answering

Either:
- **In the UI.** Open the ticket and answer the question.
- **Through your agent.** Tell your manager the answer; it records it with
  `amp_answer_question(task_id, answer)`.

Both post the answer as a comment and put the ticket back to ready. The next worker reads the
comments first, finds the answer, and continues from where the last one stopped.

## Rules for managers

- Don't answer for your user unless the ticket, its comments, or what your user already said this
  session settles it, and then quote where the answer came from.
- Ask your user plainly: the question, the options, the worker's recommendation, the ticket ID.
  Batch several questions together.
- Keep everything else moving. Tickets that depend on the question wait; nothing else should.
- Don't use `amp_set_task_state` or a plain comment to answer; `amp_answer_question` is the one
  path.

## Not a question

A worker that stopped because it needs another ticket's output has found a **missing
dependency**. Add it with `amp_set_task_dependencies`; the ticket runs again when that work is
done.
