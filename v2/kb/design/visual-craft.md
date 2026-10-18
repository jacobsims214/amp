---
title: Visual craft — making it look intentional
tags: [design, ui, typography, color, spacing, layout, visual]
---

# Visual craft

What separates a screen that looks designed from one that looks assembled. Use the product's
existing tokens when there are any; these rules tell you how to use them well, and what to do
when you must choose.

## Hierarchy first

- Every screen has one primary thing (a title, a number, a call to action). Make it obviously
  the most important by size, weight, and position. Then one or two secondary things. The rest
  is quiet: smaller, lighter, gray.
- Use at most three levels of text emphasis on a screen (e.g. ink, ink-2, ink-3). Emphasize by
  de-emphasizing the rest, not by making more things bold.
- One primary button per view. Secondary actions are outlined or ghost; tertiary are links.

## Typography

- One family for UI (a good grotesk or humanist sans: Inter, IBM Plex Sans, Geist, Source Sans),
  optionally a second for display or numbers (a serif or a mono) used sparingly and on purpose.
- A real type scale, not ad-hoc sizes: e.g. 12 / 13 / 14 / 16 / 20 / 24 / 32 / 40. Body 14–16 px.
- Line height: ~1.5 for body, ~1.2 for headings. Tighten letter-spacing slightly on large
  headings (−0.01 to −0.02em); loosen small caps labels (+0.06 to +0.12em).
- Line length 55–75 characters for reading text. Never full-width paragraphs on desktop.
- Weights: regular for text, medium/semibold for labels and headings. Avoid bold body text.
- Tabular numbers (`font-variant-numeric: tabular-nums`) in tables, prices and metrics.

## Spacing and layout

- A 4/8 px spacing scale (4, 8, 12, 16, 24, 32, 48, 64). Every gap comes from it.
- Proximity shows grouping: space *inside* a group is smaller than space *between* groups.
  If a label is as far from its input as from the previous field, the form reads as a list of
  unrelated things.
- Align to a grid. Left edges line up. Pick a content max-width (often 960–1200 px) and keep it.
- Generous whitespace reads as confidence; cramming reads as cheap. But settings and data
  screens can be denser than marketing pages: match density to the task.
- Prefer one clear column of content with a side navigation over many floating cards. Not
  everything needs a card; a card is for things that are separate objects.

## Color

- Mostly neutrals. One brand/accent color for primary actions, links, and the current item.
  Semantic colors (success, warning, danger) only for meaning, never for decoration.
- Build neutrals as a scale (50–950) with a slight warm or cool tint that matches the brand;
  pure gray (#888) looks dead.
- Contrast: text at least 4.5:1 (3:1 for large text and UI borders that carry meaning). Check
  the gray-on-white secondary text especially.
- Backgrounds: page background a step off white (or off black in dark mode); surfaces on top.
  Separate with a 1 px border *or* a subtle shadow, not both heavily.
- Dark mode is not inverted light mode: lower the contrast of surfaces, desaturate the accent
  a little, and never use pure black text on pure white or vice versa for long reading.

## Surfaces, borders, radius, elevation

- Pick one radius scale (e.g. 6 / 10 / 16) and use it consistently: inputs and buttons share
  one, cards another.
- Borders: 1 px, low contrast (e.g. neutral-200). Shadows: soft and small (`0 1px 2px` for
  resting, larger only for things that float: menus, dialogs).
- Sticky bars (headers, save bars) get a border or shadow so they read as above the content.

## Icons and imagery

- One icon set (Lucide, Heroicons, Phosphor), one stroke width, sized to the text (16–20 px).
  Icons support labels; they rarely replace them.
- No emoji as UI icons. No clip art. Avatars: real initials or photos, consistent shape.
- Illustrations only when they add meaning (an empty state can have one); keep them in the
  product's palette.

## Motion

- Fast and functional: 120–200 ms ease-out for hovers and small transitions, 200–300 ms for
  panels. Nothing bounces unless the brand is playful. Respect `prefers-reduced-motion`.

## Avoid the generic look

These are the tells of a screen nobody designed. Don't ship them unless the product's own
design system uses them:

- Default indigo/purple primary (#4f46e5 / #6366f1) with Inter and a purple gradient.
- Every section in an identical white card with the same shadow, stacked down the page.
- Everything centered; hero text over a blurry gradient blob.
- Lorem ipsum, "John Doe", "Item 1, Item 2", fake round numbers.
- Equal visual weight everywhere: same size headings, all buttons primary.
- Gratuitous glassmorphism, neon glows, or drop shadows on text.

Instead: commit to a point of view. A distinct (but accessible) accent, a considered type
pairing, a layout with rhythm and contrast between dense and open areas, real content.
