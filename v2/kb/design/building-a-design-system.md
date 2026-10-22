---
title: Building an org design system
tags: [design, design-system, playbook, planning, org]
---

# Building an org design system

An org's design system is built (and later updated) in its own AMP project, so every decision,
artifact and review has a home you can come back to. Its output is the `design-system/*` docs in
the org KB (`design/design-systems.md`). Managers: use this playbook when your user asks to build
or update the org's design system.

## The process (built into AMP)

The process is the same for every org and every project, and AMP enforces it:

- **`amp_design_system_start`** with `scope: "org"` (org owners and admins) finds or creates the
  org's **Design System** project (metadata `purpose: design-system`) and opens the next version
  as an epic of phase tickets from AMP's template, chained by dependencies and assigned to the
  designer. Run it again and you get the version already being built, never a duplicate.
- Each phase ends in an **artifact the user approves** (`working/artifacts.md`).
- **`amp_design_system_publish`** is the only way `design-system/` docs are written. It refuses
  until every phase has an approved artifact, and for the org only an owner or admin can run
  it. It stamps the README with the version, the layer, the source project, links to the
  approved artifacts and the changelog, and keeps those artifacts.
- Direct writes to `design-system/` (`amp_kb_write`, the KB editor) are refused.

As a manager: run `amp_design_system_start`, run `amp_check_plan`, present the plan, give your
user the `approve_url` and wait until they've approved it in the web UI, then dispatch each ready
phase ticket to the designer. Every phase stops for the user's review: they approve the artifact
in AMP (you may relay a "request changes" note), then re-dispatch, and move on when it's
approved.

## The phases (org)

1. **Audit.** Inventory what exists: screenshots of each product's key screens, the colors,
   fonts, spacing and radii actually used (from the CSS/Tailwind config), the components in use,
   and the inconsistencies. Artifact: an audit (markdown, or an HTML contact sheet). Ask the
   user for brand assets (logo, brand colors, fonts) and products they admire.
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
   empty/loading/error states, destructive actions, and copy tone, each with a short example.
6. **Publish.** The org owner's or admin's ticket: write `design-system/README.md`,
   `design-system/tokens.md`, `design-system/components.md`, `design-system/patterns.md` and
   `design-system/voice.md` from the approved artifacts (`amp_get_artifact` with the approved
   version) and call `amp_design_system_publish` with a one-line changelog.

## Updating it

Run `amp_design_system_start` again: it opens the next version in the same Design System
project, with the current docs as the starting point. Skip what doesn't change by saying so in
that phase's artifact (approval still applies). Publishing bumps the version and the changelog;
every project picks it up the next time anyone calls `amp_design_context`.

## A project's own layer

Once the org has published, a product project builds its layer with
`amp_design_system_start` `scope: "project"`: three tickets in that project (the delta from the
org system, the additions and overrides as an artifact, publish), each approved, then
`amp_design_system_publish` writes the project KB's `design-system/` docs with only the
differences. If an addition turns out to be useful elsewhere, propose it for the org's next
version.
