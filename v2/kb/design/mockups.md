---
title: Building mockup artifacts
tags: [design, mockups, prototypes, html, artifacts, review]
---

# Building mockup artifacts

How to build a mockup that people can judge, and that a builder can implement. Read
`design/start-here.md` first.

## What a good mockup is

- **One self-contained HTML file**: inline CSS and JS. Fonts from Google Fonts and libraries
  from a CDN are fine; nothing else is reachable inside AMP's sandbox.
- **Uses the product's tokens.** Put them as CSS variables on `:root` (colors, radius, font,
  spacing), copied from the real design system, so the builder can map one to one.
- **Looks finished**: real copy, realistic data, real states. People react to what they see; a
  rough mockup gets rough feedback.
- **Covers the states** with a small switcher bar at the top (Filled, Empty, Errors, Loading,
  …) so reviewers can flip through without you publishing several files.
- **Responsive**: reviewers switch Desktop / Tablet / Phone in AMP. It must hold up at 390 px.
- **Interactive where it matters**: tabs switch, toggles toggle, a dialog opens. Not a full app.

## The skeleton

```html
<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Settings — mockup</title>
<link href="https://fonts.googleapis.com/css2?family=…&display=swap" rel="stylesheet">
<style>
  :root { /* the product's tokens */ --bg:…; --surface:…; --ink:…; --ink-2:…; --line:…;
          --accent:…; --danger:…; --radius:…; --font:…; }
  /* base, layout, components … */
  .states { position: sticky; top: 0; z-index: 10; /* reviewer-only state switcher */ }
  [data-state] [data-show] { display: none; }
  [data-state="filled"] [data-show~="filled"], [data-state="empty"] [data-show~="empty"] { display: revert; }
</style></head>
<body data-state="filled">
  <div class="states" role="toolbar" aria-label="Mockup state">…buttons that set body.dataset.state…</div>
  <!-- the screen -->
</body></html>
```

## Process

1. Read the product's tokens and screenshot two existing screens. Note what you'll reuse.
2. Write the content first (headings, labels, sample data, error messages). Content decides
   the layout, not the other way round.
3. Build the layout and the primary state. Screenshot at 1440 and 390. With chrome-devtools:
   `new_page` on the `file://` URL; desktop `resize_page` 1440×1000; phone `emulate` with
   `viewport: "390x844x2,mobile,touch"` (headless Chrome can't resize a window that narrow);
   `wait_for` text on the page; `take_screenshot` with `fullPage: true`; `read` the PNG.
4. Critique with `design/critique.md`; fix; screenshot again. Then the other states.
5. Write the file in parts if it's long (skeleton first, then sections) rather than one
   enormous write: long single outputs can stall.
6. Publish: `amp_publish_artifact(task_id, title, kind="html", content, note)`. The note says
   what's in it and how to use the state switcher.
7. Ask for review with `amp_block_task`: the url and one or two concrete decisions.
8. New version for each round of feedback (`artifact_id`), note says what changed.

## Other kinds

- **Diagrams**: SVG (`kind="svg"`), hand-written or from a tool; readable at 100%, labeled,
  in the product's colors. Mermaid is fine if you render it to SVG first.
- **Images**: `kind="image"` (PNG/JPEG/WebP/GIF, `content_base64`). Generated art via the
  `amp-image-gen` skill.
- **Specs**: `kind="markdown"`. **PDFs** for documents meant to be printed or signed.

## For the builder

The approved version is the spec. Note in the mockup (an HTML comment at the top) anything the
builder must know: which components it maps to, which tokens it uses, behavior that isn't
visible (validation rules, what happens on save).
