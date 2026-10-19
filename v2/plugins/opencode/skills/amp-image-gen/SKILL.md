---
name: amp-image-gen
description: Generate or edit raster images (illustrations, hero art, icons, textures, photo edits) through OpenRouter and save them to files. Use when a ticket needs an image that can't reasonably be drawn as SVG or CSS.
---

# Image generation

`scripts/gen_image.py` (next to this file) sends a prompt, and optionally reference images, to an
image model on OpenRouter and writes the result to disk. It prints the saved path(s) and the cost.

```bash
python3 <skill-dir>/scripts/gen_image.py "flat retro mission patch, ringed planet, three stars" \
  -o docs/images/patch.png --aspect 1:1
# edit or restyle an existing image
python3 <skill-dir>/scripts/gen_image.py "same layout, dark theme, orange accents" \
  --input docs/images/hero.png -o docs/images/hero-dark.png
```

Options: `--input` (repeatable) for reference or source images, `--aspect` (`1:1`, `16:9`,
`9:16`, `4:3`, …), `--model` to pick the model.

## Models

| Model | Use | Rough cost |
|---|---|---|
| `google/gemini-3.1-flash-image` (default) | drafts, icons, quick edits | ~$0.07 per image |
| `google/gemini-nano-banana-2.1` | finals, text in images, precise edits | ~2–3× the draft |

There's no open-weight image generator on OpenRouter today, so these are Google models. Check
`https://openrouter.ai/api/v1/models` (models whose output includes `image`) before assuming the
list is current.

## Rules

- Draft cheap, show the draft, then make the final. Don't loop on regenerations: if two tries
  miss, rewrite the prompt (subject, style, composition, colors, what to leave out).
- Prefer SVG/CSS for logos, icons and anything geometric.
- Don't put real people, brands or logos you don't own in images.
- Record the prompt and model next to the output (a short `.md` beside it, or in your ticket
  comment) so the image can be regenerated.
- The key comes from `OPENROUTER_API_KEY`, or opencode's stored OpenRouter key. If neither
  exists, block the ticket with a `QUESTION` asking for one; don't hunt for keys elsewhere.
