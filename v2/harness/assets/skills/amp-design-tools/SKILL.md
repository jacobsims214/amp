---
name: amp-design-tools
description: Fast tools for design work. Screenshot every state of a mockup in one call (capture.mjs), and publish a file as an AMP artifact without re-typing it (publish_file.py). Use them instead of screenshotting state by state with chrome-devtools, and instead of pasting a whole file into amp_publish_artifact.
---

# Design tools

Two scripts next to this file. Both run in seconds; doing the same by hand takes minutes of
tool calls and output tokens.

## capture.mjs — every state, every width, one call

```bash
node <skill-dir>/scripts/capture.mjs mockup.html --out shots
```

It opens the page in headless Chrome and screenshots, full-page, every combination of
width (default `1440,390`; widths under 600 are a 2x phone viewport), theme and state, as
`shots/<width>-<theme>-<state>.png`. States and themes come from the mockup's switcher:
buttons with `data-setstate="…"` and `data-settheme="…"` (the convention in
`design/mockups.md`). Override with `--states a,b`, `--themes light,dark`, `--widths 1440`.
Then `read` the PNGs you need to look at. It needs Chrome (`$CHROME` if it isn't in the usual
place) and Node 22+.

## publish_file.py — publish a file without re-typing it

```bash
python3 <skill-dir>/scripts/publish_file.py mockup.html --task 222 --title "Settings mockup" --note "first version"
python3 <skill-dir>/scripts/publish_file.py mockup.html --artifact 1 --note "v3: Save stays primary"
```

It reads the file and calls AMP's `amp_publish_artifact` itself (kind from the extension;
images and PDFs are sent as base64). It needs `AMP_MCP_URL` and `AMP_TOKEN` in the
environment. If they aren't set, publish with
`amp_publish_artifact` and the content.

After publishing, block the ticket for review as usual (`amp_block_task` with the url).
