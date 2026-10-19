---
title: Design systems — the layers
tags: [design, design-system, tokens, components, org, kb]
---

# Design systems: the layers

Design in AMP is layered, most general first. Each layer is a knowledge base, so every agent in
every harness reads the same thing.

| Layer | Where | What it holds | Who writes it |
|---|---|---|---|
| **Craft** | AMP's guide (`design/*`) | How to do UI/UX well: process, rules, critique | Ships with AMP |
| **Org design system** | Org KB, `design-system/*` | The org's brand, tokens, components, patterns, voice | Published from the org's Design System project (owners and admins) |
| **Project design system** | Project KB, `design-system/*` | Only what this product adds or changes | Published from the project itself |

Both are built the same way, with AMP's design system process (`amp_design_system_start`, then
`amp_design_system_publish`; see `design/building-a-design-system.md`). Nobody writes
`design-system/` docs by hand: they're published from approved artifacts, versioned, with a
changelog.
| **The work** | Artifacts and code | Mockups, screens, assets | Agents, reviewed by people |

**The more specific layer wins.** Project over org, org over the guide's defaults. A project
never restates the org's system; it records differences ("Storefront uses the org palette but a
warmer accent, `--accent: #c2410c`, for promotions") and anything new (a component only it has).

**Start every piece of design work with `amp_design_context(project_id)`.** It lists all three
layers for the project, includes the org's `design-system/README.md`, and says what's missing.

## Org design system: the files

Under `design-system/` in the org KB:

- `design-system/README.md`: what the system is, its principles (three to five, concrete), version and
  changelog, the source project (the org's Design System project id) and links to its approved
  artifacts (token sheet, component gallery).
- `design-system/tokens.md`: every token with its role and value. Color roles (background, surface, ink
  levels, line, accent, focus, success, warning, danger) for light and dark, contrast checked;
  type families and scale; spacing; radius; elevation; motion. Then the same as copy-paste code:
  a `:root { --… }` CSS block and a Tailwind `@theme` block.
- `design-system/components.md`: each core component's anatomy, variants, states, sizes, and do/don't.
- `design-system/patterns.md`: the org's decisions for layouts, navigation, forms, tables, settings, empty,
  loading and error states.
- `design-system/voice.md`: tone, words to use and avoid, capitalization, error-message style.
- `design-system/brand.md` (optional): logo usage, imagery, illustration, iconography set.

## Project design system: the files

Under `design-system/` in the project KB, only if the project differs: a `design-system/README.md` that says
which org design system version it extends and what it changes, plus `design-system/tokens.md` /
`design-system/components.md` with only the differences and additions.

## Using it

- Mockups put the resolved tokens on `:root` as CSS variables, so builders map them one to one.
- Builders use the system's components and tokens; a new one is a design-system change (ask),
  not a one-off.
- When there's no org design system yet, say so: the work follows the product's existing code
  tokens, and the artifact note says it isn't based on an org system. Suggest building one
  (`design/building-a-design-system.md`).
