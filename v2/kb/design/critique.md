---
title: Design critique — the rubric
tags: [design, critique, review, checklist, quality]
---

# Design critique

Use this on your own work before you publish it (at least twice), and when reviewing someone
else's. Score each line pass / fix. Anything that fails gets fixed before review; if you can't,
say so in the version note.

## The rubric

**Purpose**
- Can someone tell in 5 seconds what this screen is for and what to do first?
- Does it match the job written in the ticket (not a generic version of it)?

**Hierarchy**
- Is there one clear primary element and one primary action?
- Squint test: blur your eyes (or the screenshot). Do the important things still stand out,
  and do groups read as groups?

**Consistency**
- Every size, gap, radius and color from the scale/tokens? Same things look the same?
- Does it look like it belongs to this product?

**Craft**
- Alignment: do edges line up? Is spacing even and grouped by proximity?
- Typography: real scale, comfortable line length, no more than three emphasis levels?
- Any tell from "Avoid the generic look" in `design/visual-craft.md`?

**Completeness**
- Empty, loading, error, success, long content, no permission: shown or deliberately N/A?
- Phone and desktop both designed, not just squeezed?

**Content**
- Real-looking names, numbers, and copy? Buttons say what they do? Errors say how to fix it?

**Accessibility**
- Contrast, focus, keyboard, labels, color-not-alone (`design/accessibility-and-responsive.md`)?

## How to critique honestly

- Look at screenshots, not the code. Compare side by side with an existing screen of the
  product, or a best-in-class reference for the same pattern.
- Find at least three things to improve on every pass. "Looks fine" means you haven't looked.
- Fix the biggest issue first (usually hierarchy or spacing), then re-screenshot.

## Giving feedback (reviewers and people)

Be specific and actionable: what, where, why. "The Save button competes with Cancel; make
Cancel a ghost button" beats "buttons feel off". Separate must-fix from taste.
