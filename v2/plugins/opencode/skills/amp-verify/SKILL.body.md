# Recording a visual check

A `visual` check on a receipt is a **recording of the browser doing the thing**: agent-browser
opens the page, clicks and types through the journey, and the video is the evidence. AMP counts a
visual pass only when it cites that video. A screenshot, a test log, or "I looked at it" counts as
`cant_tell`.

One script next to this file does the driving, the recording and the encoding:

```bash
python3 <skill-dir>/scripts/record.py --task <task_id> --head-url http://localhost:5173
```

It needs `agent-browser`, `ffmpeg`, `ffprobe`, `git` and `python3`. If one is missing it says so and
exits 3: record the visual check as `cant_tell` with that reason. Don't fall back to screenshots.

## 1. Write the journeys

A journey is one thing a person does in the UI that an acceptance criterion describes. Write them
in `.amp/journeys/<task_id>.json` and **commit the file**: it is part of the work, and a reviewer
or verifier re-runs it.

```json
{
  "serve": {"command": "npm run dev -- --port $PORT --strictPort", "cwd": "ui", "setup": "npm ci", "ready": "/"},
  "viewport": [1280, 800],
  "journeys": [
    {"name": "Create a ticket from the board", "criterion_ref": "1",
     "steps": [["open", "/board"],
               ["click", "text=New ticket"],
               ["fill", "[name=title]", "Fix the login redirect"],
               ["click", "text=Create"],
               ["expect", "text", "Fix the login redirect"]]},
    {"name": "An empty title is refused", "criterion_ref": "2", "unhappy": true,
     "steps": [["open", "/board"],
               ["click", "text=New ticket"],
               ["click", "text=Create"],
               ["expect", "text", "Title is required"]]}
  ]
}
```

- **A step is one agent-browser command**, as a list of its arguments: `open`, `click`, `fill`,
  `type`, `press`, `hover`, `select`, `scroll`, `wait`, `find … click`, and the rest of
  `agent-browser --help`. An `open` path that starts with `/` is the served site. Find selectors
  with `agent-browser open <url>` then `agent-browser snapshot -i` before you write the file;
  prefer text, role, label and test-id selectors over `@e` refs, which change between runs.
- **`expect` is an assertion**: `["expect", "text", "Saved"]`, `["expect", "no-text", "Error"]`,
  `["expect", "visible", "#toast"]`, `["expect", "hidden", "#spinner"]`,
  `["expect", "url", "**/board"]`. Each waits up to 10 s (`AGENT_BROWSER_DEFAULT_TIMEOUT`, in ms).
- **A journey passes when every step succeeds**, and fails at the first one that doesn't. End
  every journey with an `expect` that shows the criterion is met: a journey that only clicks proves
  nothing.
- **At least one journey is an unhappy path** (`"unhappy": true`): the error, the empty state, the
  refused input. Without one the verdict is `CANT_TELL`.
- **`criterion_ref`** is the acceptance criterion's number, the same one the check uses.
- **Secrets**: write `"env:NAME"` as an argument (`["fill", "#password", "env:TEST_PASSWORD"]`). The
  value is read from the environment and the transcript shows only `env:TEST_PASSWORD`. The video
  still shows whatever the page shows, so only type secrets into password fields.
- **`serve`** (optional) is how to serve a checkout: `command` must listen on `$PORT`, `cwd` is
  relative to the repo root, `ready` is the path that answers when it is up (`timeout_s`, default
  120), and `setup` runs first in a fresh checkout (installing dependencies).

## 2. Commit, then record

The recording is bound to the commit's tree, so commit everything first, the journeys file
included. With uncommitted changes the recorder stops with `CANT_TELL`.

```bash
# against the dev server you already run on this checkout
python3 <skill-dir>/scripts/record.py --task <task_id> --head-url http://localhost:5173

# or let it serve the checkout itself with the file's "serve" (or --serve '<command>')
python3 <skill-dir>/scripts/record.py --task <task_id>

# and prove the journeys fail without your change: same journeys on a clean worktree of the base
python3 <skill-dir>/scripts/record.py --task <task_id> --base-rev "$(git merge-base HEAD origin/main)"
```

Give it a timeout like any long command (`timeout 900 python3 …`). Use `--base-rev` whenever the
project can be served from a fresh checkout (it needs `serve`): a journey that passes on head and
fails on base is the proof that your change is what made it pass. If every journey also passes on
the base, the verdict is `CANT_TELL`: the journeys don't show the change, so rewrite them.

It prints the verdict, each journey's result, the evidence directory and the arguments for
`amp_add_evidence`:

| Exit | Verdict | Means |
|---|---|---|
| 0 | `PASS` | every head journey passed, one is an unhappy path, and (with `--base-rev`) one fails on base |
| 1 | `FAIL` | a head journey failed: fix the work or the journey, commit, record again |
| 3 | `CANT_TELL` | it couldn't record or the journeys prove nothing; the reason is printed |
| 2 | none | a usage error: a bad flag or a malformed journeys file |

The evidence directory holds `video.mp4` (every head journey, in order), `poster.gif`,
`contact.png` (eight frames, so you can `read` it and see what was recorded), `transcript.txt`
(every command and its result) and `receipt.json` (the verdicts, and where each journey starts in
the video). Look at `contact.png` before you upload: if it shows a blank page, a login wall or an
error overlay, the recording isn't evidence, whatever the verdict says.

## 3. Upload and cite it

```
amp_add_evidence(receipt_id=<from amp_open_receipt>, dispatch_id="<your dispatch ID>",
  commit=…, tree_sha=…, files=[…])            ← the three values record.py printed, as they are
curl -fsS -T <evidence-dir>/video.mp4 "<url>"  ← each upload's url, once; prefix a bare /api/... with $AMP_URL
```

Upload every file it listed. Then cite **`video.mp4`'s key** in each visual check, one check per
journey's criterion, and pass the same `tree_sha` to `amp_complete_task`:

```
{criterion_ref: "1", kind: "visual", verdict: "pass", evidence_key: "<video.mp4's key>",
 note: "journey 1, Create a ticket from the board: passes on head, fails on base"}
```

- The verdict is the journey's own: `pass` for a head `PASS`, `fail` for a `FAIL` (upload the
  recording anyway: it shows the failure), `cant_tell` for anything else.
- Say in the note whether the base was run (`base not run` when it wasn't).
- If an upload fails, call `amp_add_evidence` again for a fresh URL. If it says evidence storage
  isn't configured, complete without evidence and record the visual checks as `cant_tell`.
- Not in git, no browser, or nothing to serve: the visual check is `cant_tell`, with the reason in
  its note. Never mark a visual check `pass` on screenshots or on a test run.
