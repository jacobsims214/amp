#!/usr/bin/env python3
"""Drives `/remote` (#2376) in a real pty against #2373's mock relay and prints what the person sees.

The stand-in AMP is tests/remote_rig.rs (OAuth + mock AMP MCP + mock relay on one loopback origin);
it also plays the browser. The chat is the real binary in a pty of the given size, logged in with
`amp-harness login --no-browser` the way a person logs in. HOME is a temp dir under target/, the
credential store is the file (no keychain, no GUI). Needs pexpect and pyte:

    python3 -m venv target/pyenv && target/pyenv/bin/pip install pexpect pyte
    target/pyenv/bin/python scripts/remote-pty-check.py 120x40
    target/pyenv/bin/python scripts/remote-pty-check.py 80x24
    target/pyenv/bin/python scripts/remote-pty-check.py errors
    target/pyenv/bin/python scripts/remote-pty-check.py copy
    target/pyenv/bin/python scripts/remote-pty-check.py outage
    target/pyenv/bin/python scripts/remote-pty-check.py restart

`outage` (#2424) takes the relay down past the connect token's life and checks that remote control
comes back by itself, with the paired device still working. `restart` (#2424) kills the chat with
SIGKILL, starts it again, and checks that the left-behind session stays listed until `/remote`
ends it, and that the device paired before needs no new pairing.

`copy` (#2423) checks that the link is copied as it is shown, `/remote copy` and `/remote open`,
and that the rows on screen, joined, are the link. Every mode puts recording stand-ins for
pbcopy, wl-copy, xclip, xsel, open and xdg-open first on the chat's PATH, so nothing here touches
the real clipboard or browser; OSC 52 goes to the pty, which records it.

Run from v2/harness after `cargo build -p amp-harness --all-targets`.
"""

import base64
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

import pexpect
import pyte

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
BIN = os.path.join(ROOT, "target", "debug", "amp-harness")
WORK = os.path.join(ROOT, "target", "remote-pty")

FAUX_CONFIG = """
[providers.faux]
api = "faux"
[providers.faux.models."faux-1"]

[roles.manager]
model = "faux/faux-1"
"""

SCRIPT = [
    {"text": "Hi phone, this reply came from the laptop."},
    {"text": "Looking you up first.", "tool_calls": [{"name": "amp_whoami", "id": "slow-1"}]},
    {"text": "Done with the first one."},
    {"text": "And this is the queued second one."},
] + [{"text": f"spare reply {i}"} for i in range(10)]


def rig_binary():
    out = subprocess.run(
        ["cargo", "test", "-p", "amp-harness", "--test", "remote_rig", "--no-run",
         "--message-format=json"],
        cwd=ROOT, capture_output=True, text=True, check=True)
    for line in out.stdout.splitlines():
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        if msg.get("executable") and msg.get("target", {}).get("name") == "remote_rig":
            return msg["executable"]
    raise SystemExit("remote_rig test binary not found")


class Rig:
    def __init__(self, env):
        self.proc = subprocess.Popen([rig_binary()], cwd=ROOT, env=env, stdin=subprocess.PIPE,
                                     stdout=subprocess.PIPE, text=True, bufsize=1)
        line = self.proc.stdout.readline().strip()
        assert line.startswith("URL "), line
        self.url = line[4:]

    def send(self, cmd, until=None):
        """Sends one line; returns the answer lines (up to one starting with `until`)."""
        self.proc.stdin.write(cmd + "\n")
        self.proc.stdin.flush()
        lines = []
        while True:
            line = self.proc.stdout.readline().strip()
            lines.append(line)
            if line.startswith("ERR"):
                raise SystemExit(f"rig: {line} (after {cmd!r})")
            if until is None or line.startswith(until) or not line:
                return lines

    def close(self):
        try:
            self.proc.stdin.write("quit\n")
            self.proc.stdin.flush()
            self.proc.wait(timeout=5)
        except Exception:
            self.proc.kill()


STUB_TOOLS = ["pbcopy", "wl-copy", "xclip", "xsel", "open", "xdg-open"]


def stubs(dir_):
    """Recording stand-ins for the clipboard tools and browser openers: each run writes its stdin
    (copy tools) or its last argument (openers) to <dir>/bin/<name>.<pid>.out."""
    bin_ = os.path.join(dir_, "bin")
    os.makedirs(bin_, exist_ok=True)
    for name in STUB_TOOLS:
        path = os.path.join(bin_, name)
        with open(path, "w") as f:
            if name in ("open", "xdg-open"):
                f.write('#!/bin/sh\nfor a; do last="$a"; done\nprintf %s "$last" > "$0.$$.out"\n')
            else:
                f.write('#!/bin/sh\ncat > "$0.$$.out"\n')
        os.chmod(path, 0o755)
    return bin_


def recorded(dir_, name):
    """What each run of the stand-in `name` got, oldest first."""
    bin_ = os.path.join(dir_, "bin")
    files = [os.path.join(bin_, f) for f in os.listdir(bin_)
             if f.startswith(name + ".") and f.endswith(".out")]
    files.sort(key=os.path.getmtime)
    return [open(f).read() for f in files]


def home_env(dir_, url, extra=None):
    env = {k: v for k, v in os.environ.items()
           if not k.startswith("AMP_") and k not in ("ANTHROPIC_API_KEY", "OPENROUTER_API_KEY",
                                                      "RUST_LOG")}
    env.update({
        "HOME": os.path.join(dir_, "home"),
        "XDG_CONFIG_HOME": os.path.join(dir_, "home", ".config"),
        "XDG_STATE_HOME": os.path.join(dir_, "home", ".local", "state"),
        "AMP_CREDENTIAL_STORE": "file",
        "AMP_MCP_URL": url,
        "AMP_HARNESS_CEILING_DIR": dir_,
        "AMP_HARNESS_FAUX_SCRIPT": os.path.join(dir_, "script.json"),
        "AMP_HARNESS_REMOTE_INSECURE_LOOPBACK": "1",
        "AMP_HARNESS_HOST_NAME": "jacobs-mbp",
        "AMP_HARNESS_AUTO_RELOAD": "0",
        "TERM": "xterm-256color",
        "COLORTERM": "truecolor",
        # #2423: the stand-ins come first, so the real clipboard and browser are never used.
        "PATH": stubs(dir_) + os.pathsep + os.environ.get("PATH", ""),
    })
    env.update(extra or {})
    return env


def workdir(name):
    d = os.path.join(WORK, name)
    shutil.rmtree(d, ignore_errors=True)
    os.makedirs(os.path.join(d, "home", ".config", "amp-harness"))
    os.makedirs(os.path.join(d, "project"))
    with open(os.path.join(d, "project", ".amp.json"), "w") as f:
        json.dump({"project_id": 2, "project_name": "AMP v2"}, f)
    with open(os.path.join(d, "home", ".config", "amp-harness", "config.toml"), "w") as f:
        f.write(FAUX_CONFIG)
    with open(os.path.join(d, "script.json"), "w") as f:
        json.dump(SCRIPT, f)
    return d


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def login(dir_, env):
    """`amp-harness login --no-browser`, answering the browser step by hand."""
    p = subprocess.Popen([BIN, "login", "--no-browser"], cwd=os.path.join(dir_, "project"),
                         env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                         stderr=subprocess.PIPE, text=True)
    deadline = time.time() + 30
    url = None
    while time.time() < deadline:
        line = p.stderr.readline()
        if not line:
            break
        t = line.strip()
        if t.startswith("http") and "/authorize" in t:
            url = t
            break
    assert url, "no authorize URL"
    opener = urllib.request.build_opener(NoRedirect)
    try:
        opener.open(url)
    except urllib.error.HTTPError as e:
        back = e.headers["Location"]
    urllib.request.urlopen(back).read()
    p.wait(timeout=30)
    assert p.returncode == 0, p.stderr.read()


class Chat:
    def __init__(self, dir_, env, cols, rows):
        self.cols, self.rows = cols, rows
        self.screen = pyte.Screen(cols, rows)
        self.stream = pyte.ByteStream(self.screen)
        self.raw = b""
        self.child = pexpect.spawn(BIN, ["--no-setup"], cwd=os.path.join(dir_, "project"),
                                   env=env, dimensions=(rows, cols))

    def pump(self, secs=0.3):
        end = time.time() + secs
        while time.time() < end:
            try:
                data = self.child.read_nonblocking(65536, timeout=0.05)
                self.raw += data
                self.stream.feed(data)
            except pexpect.TIMEOUT:
                pass
            except pexpect.EOF:
                return

    def text(self):
        return "\n".join(line.rstrip() for line in self.screen.display)

    def wait(self, needle, timeout=20):
        end = time.time() + timeout
        while time.time() < end:
            self.pump(0.2)
            if (needle(self.text()) if callable(needle) else needle in self.text()):
                self.pump(0.3)
                return True
        raise SystemExit(f"timed out waiting for {needle!r}; screen:\n{self.text()}")

    def type(self, text):
        for ch in text:
            self.child.send(ch)
            time.sleep(0.01)
        self.pump(0.2)

    def enter(self, text):
        self.type(text)
        self.child.send("\r")
        self.pump(0.4)

    def link(self):
        """The pairing link on screen (wrapped lines joined)."""
        lines = [l.strip() for l in self.screen.display]
        found = None
        for i, l in enumerate(lines):
            if l.startswith("http://127.0.0.1") and "/sessions/" in l:
                parts = [l]
                for nxt in lines[i + 1:]:
                    if not re.fullmatch(r"[A-Za-z0-9_\-#=&%.:/?]+", nxt):
                        break
                    parts.append(nxt)
                found = "".join(parts)
        assert found, self.text()
        return found


def show(title, chat, grep=None):
    print(f"\n===== {title} ({chat.cols}x{chat.rows}) =====")
    text = chat.text()
    if grep:
        text = "\n".join(l for l in text.splitlines() if re.search(grep, l))
    print(text)


def flow(cols, rows):
    d = workdir(f"flow-{cols}x{rows}")
    rig = Rig(dict(os.environ, AMP_REMOTE_RIG="1"))
    env = home_env(d, rig.url)
    login(d, env)
    chat = Chat(d, env, cols, rows)
    chat.wait("/help for commands")

    snap = subprocess.run([BIN, "--no-setup", "--snapshot", "100x80", "--input", "/help"],
                          cwd=os.path.join(d, "project"), env=env, capture_output=True,
                          text=True, timeout=60)
    print(f"\n===== /help (--snapshot 100x80, the AMP group) =====")
    lines = snap.stdout.splitlines()
    start = next(i for i, l in enumerate(lines) if l.strip() == "AMP")
    print("\n".join(lines[start:start + 12]))

    chat.type("/re")
    show("/ menu while typing /re", chat)
    chat.child.send("\x1b")  # dismiss the menu
    chat.child.send("\x15")
    chat.pump(0.3)
    chat.enter("")

    chat.enter("/remote")
    chat.wait("REMOTE ON")
    show("/remote", chat)
    chat.child.send("\x1b[5~")  # PageUp: the lines above the QR code
    chat.pump(0.5)
    show("/remote, scrolled up a page", chat)
    chat.child.send("\x1b[F")  # End
    chat.pump(0.5)
    link = chat.link()

    # Pairing, answered with Enter: the default is No.
    out = rig.send(f"pair {link}", until="CODE")
    code = out[-1].split()[-1]
    chat.wait("to control this session?")
    show(f"pairing question (browser shows {code})", chat)
    chat.child.send("\r")
    print(rig.proc.stdout.readline().strip())  # PAIRED rejected
    chat.wait("Refused")
    show("Enter refused it", chat, r"Refused|Allow|PAIR|code")

    # A new link, answered y.
    chat.enter("/remote pair")
    chat.wait(lambda t: t.count("/sessions/") >= 1 and "works once" in t)
    link = chat.link()
    out = rig.send(f"pair {link}", until="CODE")
    chat.wait("to control this session?")
    chat.child.send("y")
    print(rig.proc.stdout.readline().strip())  # PAIRED accepted
    chat.wait("Allowed")
    rig.send("viewers 1", until="VIEWERS")
    chat.wait("REMOTE ON · 1 device")
    show("y paired it; one viewer", chat)

    print(rig.send("prompt hello from the phone", until="ACK"))
    chat.wait("this reply came from the laptop")
    show("a remote prompt", chat)

    rig.send("slow 3000", until="SLOW")
    print(rig.send("prompt first, while nothing runs", until="ACK"))
    time.sleep(0.5)
    print(rig.send("prompt second, while the first runs", until="ACK"))
    chat.wait("queued #1")
    show("a queued remote prompt", chat)
    rig.send("slow 0", until="SLOW")
    chat.wait("queued second one", timeout=30)
    show("the queued prompt ran", chat)

    chat.enter("/remote devices")
    chat.wait("Paired devices")
    show("/remote devices", chat, r"Paired|\d\. |revoke")

    rig.send("drop", until="DROPPED")
    chat.wait("reconnected", timeout=30)
    show("connection dropped and came back", chat, r"REMOTE|connection|reconnect")

    rig.send("revoke", until="REVOKED")
    chat.wait("AMP revoked")
    show("AMP revoked the device", chat, r"revoked|REMOTE")

    chat.enter("/remote off")
    chat.wait("Remote control is off.")
    show("/remote off", chat)

    chat.enter("/remote")
    chat.wait("REMOTE ON")
    rig.send("end disconnected", until="ENDED")
    chat.wait("Disconnect")
    show("Disconnect from the web app", chat, r"Remote|REMOTE")

    chat.child.sendcontrol("c")
    chat.child.sendcontrol("c")
    chat.child.expect(pexpect.EOF, timeout=20)
    rig.close()


def copy():
    """#2423: the link is copied as it is shown; /remote copy, /remote open; a selection."""
    cols, rows = 80, 30  # no room for the QR code: the link stays on screen
    d = workdir("copy")
    rig = Rig(dict(os.environ, AMP_REMOTE_RIG="1"))
    env = home_env(d, rig.url)
    login(d, env)
    chat = Chat(d, env, cols, rows)
    chat.wait("/help for commands")
    chat.enter("/remote")
    chat.wait("Pairing link copied to the clipboard.")
    show("/remote", chat)
    tool = "pbcopy" if sys.platform == "darwin" else None
    copies = [c for t in ("pbcopy", "wl-copy", "xclip", "xsel") for c in recorded(d, t)]
    assert len(copies) == 1, copies
    link = copies[0]
    assert tool is None or recorded(d, tool) == [link], recorded(d, tool)
    osc = re.findall(rb"\x1b\]52;c;([A-Za-z0-9+/=]+)\x07", chat.raw)
    assert [base64.b64decode(o).decode() for o in osc] == [link], osc

    # A plain selection: from the link's first character to its last, as the terminal has it.
    screen = [line.rstrip() for line in chat.screen.display]
    first = next(i for i, l in enumerate(screen) if l.startswith("http://127.0.0.1"))
    sel = []
    for l in screen[first:]:
        if not l or l.startswith(" ") or " " in l:
            break
        sel.append(l)
    print(f"selection: {len(sel)} rows, starting at column 0")
    assert len(sel) > 1, "the link wraps at 80 columns"
    assert "".join(sel) == link, (sel, "not the copied link")
    print("selection joined == the copied link: yes")

    chat.enter("/remote copy")
    chat.pump(1.0)
    copies = [c for t in ("pbcopy", "wl-copy", "xclip", "xsel") for c in recorded(d, t)]
    assert copies == [link, link], len(copies)
    print("/remote copy: copied again (stand-in got the same link)")
    chat.enter("/remote open")
    chat.wait("Opened the pairing link")
    opened = recorded(d, "open") + recorded(d, "xdg-open")
    assert opened == [link], opened
    print("/remote open: the stand-in opener got the exact link")
    show("/remote copy and /remote open", chat, r"copied|Opened|remote")

    chat.child.sendcontrol("c")
    chat.child.sendcontrol("c")
    chat.child.expect(pexpect.EOF, timeout=20)
    rig.close()
    print("copy: OK")


def pair_y(chat, rig):
    """Pairs the rig's browser with the link on screen, answered y at the laptop."""
    link = chat.link()
    rig.send(f"pair {link}", until="CODE")
    chat.wait("to control this session?")
    chat.child.send("y")
    out = rig.proc.stdout.readline().strip()
    assert out.replace('"', "") == "PAIRED accepted", out
    chat.wait("Allowed")


def wait_rig(rig, cmd, want, timeout=60):
    end = time.time() + timeout
    while time.time() < end:
        line = rig.send(cmd)[-1]
        if line == want:
            return line
        time.sleep(0.5)
    raise SystemExit(f"rig never said {want!r} to {cmd!r} (last: {line!r})")


def outage():
    """#2424: no time limit, and remote control comes back by itself after a relay outage whose
    length outlasts the connect token; the device paired before it keeps working."""
    cols, rows = 100, 30
    d = workdir("outage")
    rig = Rig(dict(os.environ, AMP_REMOTE_RIG="1"))
    env = home_env(d, rig.url)
    login(d, env)
    chat = Chat(d, env, cols, rows)
    chat.wait("/help for commands")
    chat.enter("/remote")
    chat.wait("REMOTE ON")
    chat.wait("until you end it")
    pair_y(chat, rig)
    sid = rig.send("session")[-1].split()[-1]
    show("/remote with no time limit", chat, r"Remote control|REMOTE")

    print(rig.send("down on"))
    chat.wait("REMOTE OFFLINE", timeout=90)
    show("relay down", chat, r"REMOTE")
    time.sleep(20)  # several reconnect rounds, up to the 30 s backoff cap
    print(rig.send("expire"))  # the outage outlasted the connect token
    chat.pump(0.5)
    assert "REMOTE OFFLINE" in chat.text(), chat.text()
    print(rig.send("down off"))
    wait_rig(rig, "stream", "STREAM true", timeout=90)
    chat.wait(lambda t: "REMOTE ON" in t and "REMOTE OFFLINE" not in t, timeout=30)
    assert rig.send(f"ended {sid}")[-1] == "ENDED_OF none"
    print(rig.send("prompt hello after the outage", until="ACK"))
    chat.wait("this reply came from the laptop")
    show("back by itself; the paired phone drives it again", chat, r"REMOTE|remote|laptop")

    chat.child.sendcontrol("c")
    chat.child.sendcontrol("c")
    chat.child.expect(pexpect.EOF, timeout=20)
    rig.close()
    print("outage: OK")


def restart():
    """#2424: the chat is killed (SIGKILL). Its session stays listed; a new chat leaves it alone;
    /remote in the new chat ends it, starts a new one, and the phone paired before still works."""
    cols, rows = 100, 30
    d = workdir("restart")
    rig = Rig(dict(os.environ, AMP_REMOTE_RIG="1"))
    env = home_env(d, rig.url)
    login(d, env)
    chat = Chat(d, env, cols, rows)
    chat.wait("/help for commands")
    chat.enter("/remote")
    chat.wait("REMOTE ON")
    pair_y(chat, rig)
    old = rig.send("session")[-1].split()[-1]
    chat.child.kill(9)
    chat.child.wait()
    print(f"killed the chat; session {old}")
    time.sleep(1)
    assert rig.send(f"ended {old}")[-1] == "ENDED_OF none", "the relay heard nothing"

    chat = Chat(d, env, cols, rows)
    chat.wait("/help for commands")
    chat.pump(3)
    assert rig.send(f"ended {old}")[-1] == "ENDED_OF none", "a new chat alone leaves it listed"
    assert "REMOTE ON" not in chat.text(), "remote is off in a new chat"
    chat.enter("/remote")
    chat.wait("REMOTE ON")
    wait_rig(rig, f"ended {old}", "ENDED_OF harness_ended", timeout=30)
    new = rig.send("session")[-1].split()[-1]
    assert new != old, (new, old)
    print(f"/remote ended the left-behind {old} and started {new}")
    print(rig.send("prompt hello after the restart", until="ACK"))
    chat.wait("this reply came from the laptop")
    show("a restarted chat: no pairing again", chat, r"REMOTE|remote|laptop|Allow")

    chat.child.sendcontrol("c")
    chat.child.sendcontrol("c")
    chat.child.expect(pexpect.EOF, timeout=20)
    rig.close()
    print("restart: OK")


def errors():
    cols, rows = 80, 24

    def one(name, rig_env, extra, do_login, wait_for):
        d = workdir(f"err-{name}")
        rig = Rig(dict(os.environ, AMP_REMOTE_RIG="1", **rig_env))
        env = home_env(d, rig.url, extra)
        if do_login:
            login(d, env)
        chat = Chat(d, env, cols, rows)
        chat.wait("/help for commands")
        chat.enter("/remote")
        chat.wait(wait_for, timeout=30)
        show(name, chat, r"remote|Remote|REMOTE|AMP|login|token")
        chat.child.sendcontrol("c")
        chat.child.sendcontrol("c")
        chat.child.expect(pexpect.EOF, timeout=20)
        rig.close()

    one("not logged in / AMP with sign-in off", {}, {}, False, "/remote:")
    one("static amp_ token", {}, {"AMP_TOKEN": "amp_0000000000000000dummy"}, False, "/remote:")
    one("AMP without remote control (404)", {"AMP_REMOTE_RIG_REFUSE": "404"}, {}, True, "/remote:")
    one("org switch off (403)", {"AMP_REMOTE_RIG_REFUSE": "403"}, {}, True, "/remote:")
    one("turned off on this computer", {}, {"AMP_HARNESS_DISABLE_REMOTE": "1"}, True, "/remote:")
    one("plain http without the dev override", {}, {"AMP_HARNESS_REMOTE_INSECURE_LOOPBACK": "0"},
        True, "/remote:")
    one("idle timeout (5 s)", {"AMP_REMOTE_RIG_IDLE": "5"}, {}, True, "without remote activity.")


if __name__ == "__main__":
    arg = sys.argv[1] if len(sys.argv) > 1 else "120x40"
    if arg == "errors":
        errors()
    elif arg == "copy":
        copy()
    elif arg == "outage":
        outage()
    elif arg == "restart":
        restart()
    else:
        c, r = arg.lower().split("x")
        flow(int(c), int(r))
