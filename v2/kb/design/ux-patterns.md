---
title: UX patterns — forms, settings, states, copy
tags: [design, ux, forms, settings, empty-states, copy, patterns]
---

# UX patterns

Proven answers to the problems most screens have. Deviate when you have a reason, and say it.

## Forms

- Labels above inputs, always visible (placeholders are examples, never labels). Mark the
  *optional* fields, not the required ones, when most are required.
- Group related fields with a heading; one column on mobile, at most two on desktop for short
  related pairs (first/last name, city/postcode).
- Input width hints at the answer length (postcode short, address long).
- Validate on blur and on submit, not on every keystroke. Error text below the field says how
  to fix it ("Enter a full email address, like name@example.com"), not just "Invalid".
  On submit with errors, show a summary at the top and move focus to the first error.
- Buttons say what they do: "Save profile", "Update password", not "Submit" / "OK".
- Disable the primary button only while saving (with a spinner and "Saving…"). Otherwise let
  people click and show what's missing.

## Settings pages

- Side navigation of sections on desktop (tabs or a select on mobile); each section titled with
  one line explaining what it controls.
- Save per section when sections are independent; show unsaved changes (a sticky bar
  "You have unsaved changes · Discard · Save") rather than silent per-field saves, unless it's a
  toggle, which applies immediately with a small confirmation.
- Sensitive changes (email, password) re-confirm identity and explain the consequence
  ("We'll send a link to the new address").
- Destructive settings (close account, delete workspace) go last, visually separated, in a
  "Danger zone" with plain consequences.

## States every screen needs

- **Empty**: say what this is, why it's empty, and the one action that fills it. A first-run
  empty state is a chance to teach.
- **Loading**: skeletons in the shape of the content for anything over ~300 ms; keep layout
  stable (no jumping).
- **Error**: what happened in plain words, whether anything was lost, and what to do (retry,
  contact). Keep what the person typed.
- **Success**: brief confirmation near where they acted (inline, or a toast for background
  actions). Don't block with a modal.
- **Partial/long content**: long names truncate with a tooltip, long lists paginate or
  virtualize, numbers format with locale.
- **No permission**: say who can do it, instead of hiding the control entirely when people need
  to know it exists.

## Destructive actions

- Confirm with the consequence and the object's name ("Delete 'Q3 plan'? Its 12 tickets go to
  the trash for 30 days"). For irreversible, high-stakes deletes, type-to-confirm.
- Prefer undo over confirm for reversible actions.
- Danger buttons are red only at the point of confirming; the entry point is a quiet button.

## Navigation and layout

- People should always know where they are (active nav item, page title, breadcrumbs for
  depth > 2) and how to get back.
- Primary navigation stays in one place across screens. Don't hide core actions in menus.
- Put the page's main action at top right (desktop) and in a sticky bottom bar on mobile when
  it's the point of the page.

## Tables and lists

- Left-align text, right-align numbers, consistent row height, subtle row separators, sticky
  header for long tables. The first column identifies the row.
- Row actions on hover plus an always-visible "…" menu for touch and keyboard.
- Filters and search above the table; show the active filters and a count.
- On mobile, tables become stacked cards or a list with the two most important fields.

## Copy

- Sentence case for everything except proper nouns. Short, concrete, active verbs.
- Say what the person gets, not how the system works. Avoid jargon and internal names.
- Numbers and dates the way people say them ("Oct 18, 2:30 PM", "3 days ago").
- Every message that can be wrong is a support ticket: be exact.
