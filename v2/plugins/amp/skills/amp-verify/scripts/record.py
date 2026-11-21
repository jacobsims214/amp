#!/usr/bin/env python3
"""record.py: record a ticket's browser journeys as verification-receipt evidence.

  python3 record.py --task <task_id> [--head-url URL] [--serve CMD] [--base-rev REV]
                    [--journeys FILE] [--out DIR]

It drives agent-browser through every journey in .amp/journeys/<task_id>.json
while agent-browser records the browser, then writes the evidence files a
receipt takes: video.mp4 (every head journey, in order), poster.gif,
contact.png, transcript.txt (every command and its result) and receipt.json
(the verdicts, and the `files` list amp_add_evidence wants).

The head revision is the current checkout, which must have no uncommitted
changes: the recording is bound to HEAD's tree. It is served by --head-url (a
server you already run on this checkout) or by the serve command (--serve, or
the journeys file's "serve"). With --base-rev the same journeys are run on a
clean worktree of that revision, to show they fail without the change.

Verdict: PASS when every head journey passes, one of them is an unhappy path,
and (when the base was run) some journey that passes on head fails on base.
Exit status: 0 PASS, 1 FAIL, 3 CANT_TELL, 2 a usage error.

Needs agent-browser, ffmpeg, ffprobe, git and python3. Standard library only.
"""
import argparse
import hashlib
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

PASS, FAIL, CANT_TELL = "PASS", "FAIL", "CANT_TELL"
EXIT = {PASS: 0, FAIL: 1, CANT_TELL: 3}
MAX_JOURNEYS = 20
STEP_TIMEOUT_S = 90
# Commands a journey can't use: the recorder owns the recording and the session.
FORBIDDEN = {"record", "close", "connect", "batch", "session", "skills", "auth", "confirm", "deny"}


class CantTell(Exception):
    """The recording can't be made or trusted: the reason is the message."""


def usage(msg):
    """A usage error (a bad flag or journeys file): exit 2, never a verdict."""
    print(f"record.py: {msg}", file=sys.stderr)
    sys.exit(2)


def run(cmd, cwd=None, env=None, timeout=120):
    """Run cmd; return (exit code, combined output). A timeout is exit 124."""
    try:
        p = subprocess.run(cmd, cwd=cwd, env=env, timeout=timeout, stdout=subprocess.PIPE,
                           stderr=subprocess.STDOUT, text=True, errors="replace")
        return p.returncode, p.stdout.strip()
    except subprocess.TimeoutExpired as e:
        out = e.stdout.decode(errors="replace") if isinstance(e.stdout, bytes) else (e.stdout or "")
        return 124, (out.strip() + f"\n(timed out after {timeout} s)").strip()


def git(root, *args):
    code, out = run(["git", "-C", root, *args])
    if code != 0:
        raise CantTell(f"git {' '.join(args)}: {out}")
    return out


def load_journeys(path):
    """Read and check the journeys file. A bad file is a usage error."""
    try:
        with open(path) as f:
            doc = json.load(f)
    except (OSError, ValueError) as e:
        usage(f"{path}: {e}")
    js = doc.get("journeys") if isinstance(doc, dict) else None
    if not isinstance(js, list) or not 1 <= len(js) <= MAX_JOURNEYS:
        usage(f"{path}: \"journeys\" must list 1 to {MAX_JOURNEYS} journeys")
    for i, j in enumerate(js, 1):
        if not isinstance(j, dict) or not str(j.get("name", "")).strip():
            usage(f"{path}: journey {i} needs a name")
        steps = j.get("steps")
        if not isinstance(steps, list) or not steps:
            usage(f"{path}: journey {i} ({j['name']}) needs steps")
        for s in steps:
            if not isinstance(s, list) or not s or not all(isinstance(a, str) for a in s):
                usage(f"{path}: journey {i}: each step is a list of strings, got {s!r}")
            if s[0] in FORBIDDEN or s[0].startswith("-"):
                usage(f"{path}: journey {i}: a step can't be \"{s[0]}\"")
    return doc


def translate(step, base_url):
    """One journey step as (agent-browser args, the args as logged).

    A leading "/" on an `open` URL is the served site. `expect` is an
    assertion: text / no-text / visible / hidden / url. An argument "env:NAME"
    is read from the environment and logged as written, so a secret typed into
    a form never reaches the transcript."""
    if step[0] == "open" and len(step) > 1 and step[1].startswith("/"):
        step = ["open", base_url.rstrip("/") + step[1], *step[2:]]
    elif step[0] == "expect":
        kind, val = (step[1:3] + ["", ""])[:2]
        if kind == "text":
            step = ["wait", "--text", val]
        elif kind == "no-text":
            step = ["wait", "--fn", f"!document.body.innerText.includes({json.dumps(val)})"]
        elif kind == "visible":
            step = ["wait", val]
        elif kind == "hidden":
            step = ["wait", val, "--state", "hidden"]
        elif kind == "url":
            step = ["wait", "--url", val]
        else:
            usage(f"expect {kind!r}: use text, no-text, visible, hidden or url")
    real = []
    for a in step:
        if a.startswith("env:"):
            if a[4:] not in os.environ:
                raise CantTell(f"the environment has no {a[4:]} (a step uses {a})")
            real.append(os.environ[a[4:]])
        else:
            real.append(a)
    return real, step


class Transcript:
    def __init__(self, path):
        self.f = open(path, "w")
        self.t0 = time.time()

    def line(self, text=""):
        self.f.write(text + "\n")
        self.f.flush()

    def cmd(self, side, n, shown, code, out):
        self.line(f"[{time.time() - self.t0:7.1f}s] {side} journey {n}: agent-browser {' '.join(shown)}")
        self.line(f"           exit {code}" + (f": {out[:2000]}" if out else ""))


def record_journey(side, n, journey, base_url, viewport, webm, tr):
    """Drive one journey while agent-browser records it. Returns (verdict, why)."""
    env = dict(os.environ)
    env["AGENT_BROWSER_SESSION"] = f"ampv-{os.getpid()}-{side}-{n}"
    env.setdefault("AGENT_BROWSER_DEFAULT_TIMEOUT", "10000")

    def ab(real, shown=None, log=True):
        code, out = run(["agent-browser", *real], env=env, timeout=STEP_TIMEOUT_S)
        if log:
            tr.cmd(side, n, shown or real, code, out)
        return code, out

    tr.line(f"== {side} journey {n}: {journey['name']}" + (" (unhappy path)" if journey.get("unhappy") else ""))
    steps = [list(s) for s in journey["steps"]]
    start = base_url
    if steps[0][0] == "open" and len(steps[0]) == 2:  # the recording opens the first page itself
        start = translate(steps.pop(0), base_url)[0][1]
    verdict, why = PASS, ""
    try:
        for setup in (["open", "about:blank"], ["set", "viewport", str(viewport[0]), str(viewport[1])],
                      ["record", "start", webm, start]):
            code, out = ab(setup)
            if code != 0:
                return CANT_TELL, f"agent-browser {' '.join(setup[:2])} failed: {out[:300]}"
        ab(["wait", "600"], log=False)
        for step in steps:
            real, shown = translate(step, base_url)
            code, out = ab(real, shown)
            if code != 0:
                verdict, why = FAIL, f"step `{' '.join(shown)}` failed: {out[:300]}"
                break
            ab(["wait", "400"], log=False)  # let a viewer see the result
        ab(["wait", "1000"], log=False)
    finally:
        code, out = ab(["record", "stop"])
        ab(["close"], log=False)
    if code != 0 or not os.path.exists(webm) or os.path.getsize(webm) == 0:
        return CANT_TELL, f"no recording was saved: {out[:300]}"
    tr.line(f"   {side} journey {n}: {verdict}" + (f" ({why})" if why else ""))
    return verdict, why


class Server:
    """The serve command, run in cwd with $PORT set, until stop()."""

    def __init__(self, command, cwd, ready, timeout_s, log_path):
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        self.port = s.getsockname()[1]
        s.close()
        self.url = f"http://127.0.0.1:{self.port}"
        self.log = open(log_path, "w")
        env = dict(os.environ, PORT=str(self.port))
        self.proc = subprocess.Popen(command, shell=True, cwd=cwd, env=env, stdout=self.log,
                                     stderr=subprocess.STDOUT, start_new_session=True)
        deadline = time.time() + timeout_s
        probe = self.url + (ready if ready.startswith("/") else "/" + ready)
        while True:
            if self.proc.poll() is not None:
                self.stop()
                raise CantTell(f"the serve command exited ({self.proc.returncode}): see {log_path}")
            try:
                urllib.request.urlopen(probe, timeout=3).close()
                return
            except urllib.error.HTTPError as e:
                if e.code < 500:
                    return
            except OSError:
                pass
            if time.time() > deadline:
                self.stop()
                raise CantTell(f"the serve command didn't answer on {probe} within {timeout_s} s: see {log_path}")
            time.sleep(0.5)

    def stop(self):
        if self.proc.poll() is None:
            try:
                os.killpg(self.proc.pid, signal.SIGTERM)
                self.proc.wait(timeout=10)
            except (OSError, subprocess.TimeoutExpired):
                try:
                    os.killpg(self.proc.pid, signal.SIGKILL)
                except OSError:
                    pass
        self.log.close()


def probe(path):
    """(frames, duration in ms) of a video file's first video stream."""
    code, out = run(["ffprobe", "-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries",
                     "stream=nb_read_frames:format=duration", "-of", "json", path], timeout=300)
    if code != 0:
        return 0, 0
    try:
        d = json.loads(out)
        frames = int(d["streams"][0].get("nb_read_frames") or 0)
        return frames, int(float(d["format"].get("duration") or 0) * 1000)
    except (ValueError, KeyError, IndexError):
        return 0, 0


def encode(webms, out):
    """video.mp4, poster.gif and contact.png from the head recordings."""
    video = os.path.join(out, "video.mp4")
    inputs = [a for w in webms for a in ("-i", w)]
    chain = "".join(f"[{i}:v]scale=trunc(iw/2)*2:trunc(ih/2)*2,setsar=1,fps=25[v{i}];" for i in range(len(webms)))
    chain += "".join(f"[v{i}]" for i in range(len(webms))) + f"concat=n={len(webms)}:v=1:a=0,format=yuv420p[v]"
    code, o = run(["ffmpeg", "-loglevel", "error", "-y", *inputs, "-filter_complex", chain, "-map", "[v]",
                   "-c:v", "libx264", "-preset", "veryfast", "-crf", "26", "-movflags", "+faststart", video], timeout=900)
    if code != 0:
        raise CantTell(f"ffmpeg couldn't write video.mp4: {o[-400:]}")
    frames, duration_ms = probe(video)
    if frames == 0:
        raise CantTell("video.mp4 has no frames")
    secs = max(duration_ms / 1000, 0.1)
    run(["ffmpeg", "-loglevel", "error", "-y", "-i", video, "-vf",
         f"fps={min(1.0, 16 / secs):.4f},scale=640:-2:flags=lanczos", "-frames:v", "16",
         os.path.join(out, "poster.gif")], timeout=300)
    run(["ffmpeg", "-loglevel", "error", "-y", "-i", video, "-vf",
         f"select='not(mod(n\\,{max(1, frames // 8)}))',scale=480:-2,tile=4x2", "-frames:v", "1",
         os.path.join(out, "contact.png")], timeout=300)
    return frames, duration_ms


def overall(journeys, base_ran):
    """The run's verdict and why, from each journey's head and base verdicts."""
    heads = [j["head"] for j in journeys]
    if CANT_TELL in heads:
        return CANT_TELL, "a head journey couldn't be recorded"
    if FAIL in heads:
        return FAIL, "a head journey failed"
    if not any(j["unhappy"] for j in journeys):
        return CANT_TELL, "no unhappy-path journey: add one with \"unhappy\": true that shows the error case"
    if base_ran and not any(j["head"] == PASS and j["base"] == FAIL for j in journeys):
        return CANT_TELL, "every journey also passes on the base revision, so none of them shows the change"
    return PASS, "" if base_ran else "base not run"


def file_entry(out, name, **extra):
    path = os.path.join(out, name)
    with open(path, "rb") as f:
        digest = hashlib.sha256(f.read()).hexdigest()
    return {"name": name, "bytes": os.path.getsize(path), "sha256": digest, **extra}


def main():
    ap = argparse.ArgumentParser(description="Record a ticket's browser journeys as receipt evidence.")
    ap.add_argument("--task", required=True, help="the ticket id (journeys are .amp/journeys/<task>.json)")
    ap.add_argument("--journeys", help="the journeys file, when it isn't .amp/journeys/<task>.json")
    ap.add_argument("--head-url", help="a server already running on this checkout, e.g. http://localhost:5173")
    ap.add_argument("--serve", help="command that serves a checkout on $PORT (overrides the file's serve.command)")
    ap.add_argument("--base-rev", help="also run the journeys on this revision, e.g. $(git merge-base HEAD origin/main)")
    ap.add_argument("--out", help="where the evidence goes (default: a temp directory)")
    a = ap.parse_args()

    missing = [b for b in ("agent-browser", "ffmpeg", "ffprobe", "git") if not shutil.which(b)]
    if missing:
        print(f"CANT_TELL: not installed: {', '.join(missing)}")
        return EXIT[CANT_TELL]
    code, root = run(["git", "rev-parse", "--show-toplevel"])
    if code != 0:
        print("CANT_TELL: not in a git repository, so the recording can't be bound to a revision")
        return EXIT[CANT_TELL]

    jpath = os.path.abspath(a.journeys or os.path.join(root, ".amp", "journeys", f"{a.task}.json"))
    doc = load_journeys(jpath)
    serve = doc.get("serve") or {}
    command = a.serve or serve.get("command")
    viewport = doc.get("viewport") or [1280, 800]
    if not a.head_url and not command:
        usage("give --head-url, or a serve command (--serve, or \"serve\" in the journeys file)")
    if a.base_rev and not command:
        usage("--base-rev needs a serve command to serve the base revision")

    out = worktree = None
    try:
        if git(root, "status", "--porcelain", "--untracked-files=no"):
            raise CantTell("uncommitted changes: commit first, the recording is bound to HEAD's tree")
        if run(["git", "-C", root, "ls-files", "--error-unmatch", jpath])[0] != 0:
            raise CantTell(f"{os.path.relpath(jpath, root)} isn't committed")
        commit, tree = git(root, "rev-parse", "HEAD"), git(root, "rev-parse", "HEAD^{tree}")
        out = os.path.abspath(a.out or os.path.join(tempfile.gettempdir(), "amp-verify", f"{a.task}-{tree[:12]}"))
        shutil.rmtree(out, ignore_errors=True)
        os.makedirs(os.path.join(out, "head"))
        tr = Transcript(os.path.join(out, "transcript.txt"))
        tr.line(f"task {a.task}  commit {commit}  tree {tree}")
        journeys = [{"journey_n": n, "journey": j["name"], "criterion_ref": str(j.get("criterion_ref", "")),
                     "unhappy": bool(j.get("unhappy")), "head": CANT_TELL, "base": "not_run"}
                    for n, j in enumerate(doc["journeys"], 1)]

        def run_side(side, url):
            for n, j in enumerate(doc["journeys"], 1):
                webm = os.path.join(out, side, f"journey-{n}.webm")
                verdict, why = record_journey(side, n, j, url, viewport, webm, tr)
                journeys[n - 1][side] = verdict
                if why:
                    journeys[n - 1][side + "_note"] = why

        def served(side, cwd):
            return Server(command, os.path.join(cwd, serve.get("cwd", "")), serve.get("ready", "/"),
                          int(serve.get("timeout_s", 120)), os.path.join(out, f"serve-{side}.log"))

        # Head: this checkout.
        head_note = ""
        if a.head_url:
            head_note = "head served externally"
            tr.line(f"head: {a.head_url} (served externally)")
            run_side("head", a.head_url)
        else:
            srv = served("head", root)
            try:
                tr.line(f"head: `{command}` on {srv.url}")
                run_side("head", srv.url)
            finally:
                srv.stop()

        # Base: a clean worktree of --base-rev. A base that can't be served is "not run".
        base_ran, base_note = False, ""
        if a.base_rev:
            os.makedirs(os.path.join(out, "base"))
            worktree = tempfile.mkdtemp(prefix="amp-verify-base-")
            try:
                base_commit = git(root, "rev-parse", a.base_rev + "^{commit}")
                git(root, "worktree", "add", "--detach", "--force", worktree, base_commit)
                if serve.get("setup"):
                    code, o = run(["sh", "-c", serve["setup"]], cwd=os.path.join(worktree, serve.get("cwd", "")), timeout=900)
                    if code != 0:
                        raise CantTell(f"serve.setup failed on base: {o[-300:]}")
                srv = served("base", worktree)
                try:
                    tr.line(f"base: {base_commit} `{command}` on {srv.url}")
                    run_side("base", srv.url)
                    base_ran = True
                finally:
                    srv.stop()
            except CantTell as e:
                base_note = f"base not run: {e}"
                tr.line(base_note)

        webms = [os.path.join(out, "head", f"journey-{j['journey_n']}.webm") for j in journeys]
        webms = [w for w in webms if os.path.exists(w) and os.path.getsize(w) > 0]
        if not webms:
            raise CantTell("no head journey was recorded")
        frames, duration_ms = encode(webms, out)
        at = 0
        for j in journeys:  # where each journey starts in video.mp4
            w = os.path.join(out, "head", f"journey-{j['journey_n']}.webm")
            if w in webms:
                j["start_ms"] = at
                at += probe(w)[1]

        verdict, why = overall(journeys, base_ran)
        note = "; ".join(x for x in (why, base_note, head_note) if x)
        tr.line(f"== verdict: {verdict}" + (f" ({note})" if note else ""))
        tr.f.close()
        files = [file_entry(out, "video.mp4", duration_ms=duration_ms, frames=frames)]
        files += [file_entry(out, n) for n in ("poster.gif", "contact.png", "transcript.txt")
                  if os.path.exists(os.path.join(out, n))]
        receipt = {"task_id": a.task, "commit": commit, "tree_sha": tree, "base_rev": a.base_rev or "",
                   "verdict": verdict, "note": note, "journeys": journeys}
        with open(os.path.join(out, "receipt.json"), "w") as f:
            json.dump(receipt, f, indent=1)
        files.append(file_entry(out, "receipt.json"))

        print(f"{verdict}" + (f": {note}" if note else ""))
        for j in journeys:
            base = "" if j["base"] == "not_run" else f", base {j['base']}"
            print(f"  {j['journey_n']}. {j['journey']}: head {j['head']}{base}"
                  + (f" ({j['head_note']})" if j.get("head_note") else ""))
        print(f"evidence: {out}")
        print("amp_add_evidence arguments:")
        print(json.dumps({"commit": commit, "tree_sha": tree, "files": files}, indent=1))
        return EXIT[verdict]
    except CantTell as e:
        print(f"CANT_TELL: {e}")
        return EXIT[CANT_TELL]
    finally:
        if worktree:
            run(["git", "-C", root, "worktree", "remove", "--force", worktree])
            shutil.rmtree(worktree, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
