---
name: amp-design-tools
description: Fast tools for design work, and the rule for where screenshots go. Screenshot every state of a mockup in one call (capture.mjs), publish a file as an AMP artifact without re-typing it (an upload URL, or publish_file.py), and put screenshots and visual evidence on the ticket as one self-contained HTML artifact. Use them instead of screenshotting state by state with chrome-devtools, and instead of pasting a whole file into amp_publish_artifact.
---

# Design tools

Two scripts next to this file, and one rule. The scripts run in seconds; doing the same by
hand takes minutes of tool calls and output tokens.

## The rule: screenshots and visual evidence go on the ticket

Anything visual you made or checked (a mockup, a prototype, screenshots of the UI you built or
reviewed) goes on your ticket as **one html artifact**. Never leave screenshots on disk for
someone to find, never commit them, and never hand a reviewer a screenshot in place of the
mockup itself. Keep a copy in the repo only when the ticket asks for one.

- **One self-contained file.** Inline CSS and JS; CDN scripts and fonts are fine; images go in
  as `data:` URIs. 10 MB at most. Nothing else is reachable from the sandbox, so a multi-file
  prototype (a Vite or React app, a page with an `assets/` folder) must be bundled into one file
  before you publish it: `vite-plugin-singlefile`, or a build step that inlines every asset.
  Multi-file artifacts aren't supported.
- **Switchers in CSS.** State, width and theme each switch from buttons that set a data
  attribute (`data-setstate="empty"`, `data-setwidth="390"`, `data-settheme="dark"`) with CSS
  keyed off it, so a reviewer sees every state, both widths and both themes without running
  anything (`design/mockups.md` has the pattern). A responsive mockup can leave width to AMP's
  viewport switcher. A page of screenshots of a real app needs all three: one
  `<img src="data:image/png;base64,…">` per state, width and theme, and the CSS shows the one
  selected.
- **Publish the file, don't paste it.** Use the command for your harness (below).

## Publishing a file

- **Claude Code, or any harness with a shell**: ask `amp_publish_artifact` for an upload URL,
  then upload the file with `curl`. Get the size and hash first:

  ```bash
  wc -c < mockup.html | tr -d ' '                 # bytes
  shasum -a 256 mockup.html | cut -d' ' -f1       # sha256
  ```

  Call `amp_publish_artifact(task_id=<your ticket>, title="Settings mockup", kind="html",
  bytes=<bytes>, sha256="<sha256>")` with no `content`. For a new version, pass
  `artifact_id=<id>` and `note="what changed"` instead of `task_id` and `title`. The result's
  `upload.url` is good once, for 15 minutes:

  ```bash
  curl -fsS -T mockup.html '<upload.url>'         # prefix a bare /api/... with AMP's address
  ```

  The response holds the artifact's `url` and `version`. Nothing is published until the upload
  lands. A 422 means the bytes or sha256 didn't match the file: fix them and upload again to the
  same URL.
- **A small file** can still go inline: `amp_publish_artifact(..., content=...)` (or
  `content_base64` for an image or PDF). Don't re-type a large file this way.

After publishing, block the ticket for review as usual (`amp_block_task` with the url).

## Approved artifacts: keep them

An artifact nobody keeps expires a while after its last version (`expires_at`). When a person
approves a design that builders will work from, it should outlive the review. Keeping is the
person's call: if they asked for it (in the approval or a comment), call
`amp_keep_artifact(artifact_id=<id>, keep=true)`. Otherwise name the artifact, its approved
version and its `expires_at` in your completion comment, and say it needs keeping if builders
will rely on it.

## capture.mjs — every state, every width, one call

```bash
node <skill-dir>/scripts/capture.mjs mockup.html --out shots
```

It opens the page in headless Chrome and screenshots, full-page, every combination of
width (default `1440,390`; widths under 600 are a 2x phone viewport), theme and state, as
`shots/<width>-<theme>-<state>.png`. States and themes come from the mockup's switcher:
buttons with `data-setstate="…"` and `data-settheme="…"` (the convention in
`design/mockups.md`). Override with `--states a,b`, `--themes light,dark`, `--widths 1440`.
Then `Read` the PNGs you need to look at. It needs Chrome (`$CHROME` if it isn't in the usual
place) and Node 22+. The PNGs are for you to look at: what reviewers see is the published html.

## publish_file.py — publish with an API token

```bash
python3 <skill-dir>/scripts/publish_file.py mockup.html --task 222 --title "Settings mockup" --note "first version"
python3 <skill-dir>/scripts/publish_file.py mockup.html --artifact 1 --note "v3: Save stays primary"
```

It reads the file and calls AMP's `amp_publish_artifact` itself (kind from the extension;
images and PDFs are sent as base64). It needs `AMP_MCP_URL` and `AMP_TOKEN` in the
environment. If they aren't set, use the upload URL above.
