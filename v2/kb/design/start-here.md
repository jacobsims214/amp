---
title: Design work — start here
tags: [design, ui, ux, frontend, mockups, artifacts, onboarding]
---

# Design work — start here

Read this before any UI, UX or visual work: a mockup, a new screen, a redesign, a landing page,
a diagram. Then read the pages it points to. Agents: this is not optional; good design is a
process, and skipping steps is how you get generic, forgettable screens.

## The loop

1. **Understand the job.** Who uses this screen, what are they trying to get done, and what
   happens right before and after? Write it in one sentence in your first ticket comment. If
   you can't, ask (`amp_block_task` with a question) before drawing anything.
2. **Load the design layers.** Call `amp_design_context(project_id)`: it lists this guide, the
   org's design system (org KB) and the project's additions (project KB), and what's missing
   (`design/design-systems.md`). Read them, then look at the product itself: its tokens in
   code, its components, two or three existing screens (screenshot them). New work should look
   like it belongs. Only invent a visual language when there is none, and then make it
   deliberate (`design/visual-craft.md`), and say so.
3. **Decide the hierarchy before the pixels.** List what's on the screen in order of
   importance. The top one or two things get the size, weight, color and position; everything
   else steps back. Most bad screens give everything equal weight.
4. **Design every state**, not just the happy path: empty, loading, filled, long content,
   error, success, disabled, no permission. See `design/ux-patterns.md`.
5. **Build it as a real artifact.** For mockups: one self-contained HTML file with real-looking
   content (`design/mockups.md`). Desktop and phone.
6. **Look at it, critique it, fix it.** Screenshot at 1440 and 390 px, in every state. Go
   through `design/critique.md` honestly and fix what fails. Do this at least twice. The first
   draft is never the one to show.
7. **Show it and ask one or two real questions.** Publish with `amp_publish_artifact`, block
   the ticket asking for review, and name the decisions you want ("Danger zone on this page or
   its own?"), not "thoughts?".
8. **Iterate on feedback** with new versions of the same artifact, saying what changed.

## The pages

- `design/design-systems.md`: the layers (craft → org design system → project) and their files.
- `design/building-a-design-system.md`: the playbook for an org's Design System project.
- `design/visual-craft.md`: typography, spacing, color, layout, surfaces, motion. How to make it
  look intentional and not generic.
- `design/ux-patterns.md`: forms, settings, navigation, tables, states, destructive actions, copy.
- `design/accessibility-and-responsive.md`: the checklist every screen must pass.
- `design/mockups.md`: how to build a mockup artifact that reviews well (with a template).
- `design/critique.md`: the rubric to critique your own work (and others') before showing it.

## When to show before building

Plan a design ticket first (and make the build depend on it) for new screens or flows,
redesigns, landing pages, anything where "what should it look like?" has more than one good
answer, and diagrams people must agree on. Skip it for bug fixes and changes with one obvious
answer. See `working/artifacts.md`.
