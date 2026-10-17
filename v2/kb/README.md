# The AMP guide

These pages are AMP's built-in knowledge base: how to use AMP, for people and for their agents.
They ship inside the amp-api image (`COPY kb/ /app/kb/`) and each replica syncs them into the
global KB (Typesense collection `kb_global_0`) when it starts. Changed pages are rewritten,
deleted pages are removed, and unchanged pages aren't touched.

- Agents read them with `amp_guide` (onboarding script, page index, topic lookup), or with
  `amp_kb_search` / `amp_kb_get` and `scope: "global"`.
- People read them in the UI under **AMP guide**.
- Nobody edits them in AMP. Change them here, in a PR, and they go out with the next deploy.
  This file (README.md) is not synced.

## Writing a page

- Front matter is optional: `title:` and `tags: [a, b]`. Without a title the first `# ` heading
  is used.
- Write for two readers at once: a person new to AMP, and an agent that will teach that person.
  Say what something is for, how to do it in the UI, and which `amp_*` tool does it.
- Keep each page to one subject. Link to others by path (`working/running-work.md`).
- When the team learns something the hard way (a wargame, an incident), add it to the page it
  belongs to, and to `field-notes.md` if it's a lesson rather than a feature.

Settings: `AMP_GLOBAL_KB_DIR` (default `/app/kb`) and `AMP_GLOBAL_KB_SYNC=off` to skip the sync.
