---
title: Accessibility and responsive — the checklist
tags: [design, accessibility, a11y, responsive, mobile, wcag]
---

# Accessibility and responsive

Every screen passes this before it's shown or shipped. Mockups too: reviewers judge what they
see, and a mockup that ignores this sets the build up to ignore it.

## Accessibility (WCAG 2.2 AA)

- **Contrast**: text 4.5:1 (large text 3:1); meaningful UI borders and icons 3:1. Check
  secondary gray text and text on colored buttons.
- **Keyboard**: everything clickable is reachable with Tab in a sensible order and works with
  Enter/Space; no keyboard traps; Escape closes dialogs and menus.
- **Focus**: a visible focus ring on every interactive element (2 px, offset, accent color).
  Never `outline: none` without a replacement.
- **Semantics**: real `<button>`, `<a>`, `<label for>`, `<fieldset>/<legend>` for groups,
  headings in order (one `h1`). Landmarks: `header`, `nav`, `main`.
- **Names**: icon-only buttons have `aria-label`; images have `alt` (empty for decorative).
- **Forms**: errors linked with `aria-describedby`, `aria-invalid` on bad fields, focus moves
  to the first error on submit.
- **Color is never the only signal**: pair status colors with text or icons.
- **Motion**: honor `prefers-reduced-motion`; nothing flashes more than 3 times a second.
- **Targets**: at least 24×24 px (44×44 on touch for primary actions).

## Responsive

- Design mobile and desktop on purpose; check 390 px (phone), 768–820 px (tablet), 1440 px.
- Mobile: one column, side navigation becomes a horizontal scroller, select, or top tabs;
  primary action reachable at the bottom; no horizontal page scroll; 16 px side gutters.
- Inputs at least 16 px font on mobile (prevents iOS zoom), full width.
- Tables become lists or cards; long values wrap or truncate deliberately.
- Images and media scale (`max-width: 100%`); nothing has a fixed pixel width wider than the
  phone.
- Test with long content: a 40-character name, a 6-digit price, an empty value.
