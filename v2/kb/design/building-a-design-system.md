---
title: Building an org design system
tags: [design, design-system, playbook, planning, org]
---

# Building an org design system

An org's design system is built (and later updated) in its own AMP project, so every decision,
artifact and review has a home you can come back to. Its output is the `design-system/*` docs in
the org KB (`design/design-systems.md`). Managers: use this playbook when your user asks to build
or update the org's design system.

## The project

- One per org: **"<Org> Design System"**, in the org, owned by the team responsible for brand or
  design (or the user's team). Project metadata `{"purpose": "design-system"}`.
- Look for it before creating one (`amp_list_projects`, `amp_find_by_metadata` purpose =
  design-system). Updating the system later means new tickets in this same project.
- Read the project's KB and the org KB's `design-system/` first: on an update, the current
  system is the starting point.

## The plan

One epic, a story per phase. Each phase ends with an artifact the user approves, and the next
phase depends on it (design tickets, `working/artifacts.md`). The designer specialist does the
design tickets; a researcher can do the audit.

1. **Audit.** Inventory what exists: screenshots of each product's key screens, the colors,
   fonts, spacing and radii actually used (from the CSS/Tailwind config), the components in use,
   and the inconsistencies. Artifact: an audit (markdown, plus an HTML contact sheet of the
   screenshots and swatches). Ask the user for brand assets (logo, brand colors, fonts) and
   products they admire.
2. **Direction.** Two or three distinct style tiles in one HTML artifact with a switcher: type
   pairing, palette, a button, an input, a card, a table row, and one real screen from the org's
   products in that style. The user picks one (or mixes) and says why.
3. **Foundations.** Tokens from the chosen direction: color roles for light and dark (contrast
   checked to AA), type scale, spacing, radius, elevation, motion, focus. Artifact: a live token
   sheet (HTML specimen of every token, light/dark toggle) plus the `:root` CSS and Tailwind
   `@theme` blocks.
4. **Components.** The core set, every state (default, hover, focus, active, disabled, loading,
   error) and size: button, link, input, textarea, select, checkbox, radio, switch, badge, avatar,
   card, table, tabs, navigation (sidebar and top bar), dialog, toast, tooltip, empty state,
   skeleton. Artifact: a component gallery (HTML) using only the tokens.
5. **Patterns and voice.** The org's decisions for page layouts, forms, settings, tables,
   empty/loading/error states, destructive actions, and copy tone, each with a short example
   screen. Artifact: markdown plus an HTML example page.
6. **Publish.** A ticket owned by an org owner or admin (only they can write the org KB): write
   `design-system/README.md`, `design-system/tokens.md`, `design-system/components.md`, `design-system/patterns.md`, `design-system/voice.md` to the org
   KB from the approved artifacts (`amp_kb_write` with scope org and a reason), with the version
   and changelog in the README. Ask the user to **keep** the approved artifacts (they're the
   visual reference; `amp_keep_artifact` when they say so). Optionally, follow-up tickets in
   product projects to adopt the tokens in code.

## Updating it

New tickets in the Design System project: what changes and why, an artifact showing the change
(before/after), approval, then update the org KB docs, bump the version in the README, and add a
changelog line. Product projects pick it up the next time anyone calls `amp_design_context`.

## A project's own layer

When a product needs something the org system doesn't have, that's a design ticket in the
product's project: a mockup of the addition, approval, then a `design-system/` doc in the
project KB recording only the difference. If it turns out to be useful elsewhere, propose moving
it into the org system (a ticket in the Design System project).
