#!/usr/bin/env python3
"""Run a command on a fresh pseudo-terminal and answer its prompts (test helper for install.sh, #2276).

    pty-run.py [--expect TEXT --send TEXT]... [--timeout SECS] -- CMD [ARG...]

The command gets the pty as stdin, stdout, stderr and controlling terminal, so `[ -t 1 ]` and
/dev/tty work as they do for a person running `curl | sh` in a terminal. Each --expect waits until
TEXT has appeared in the output (after the previous one), then --send writes its answer ("\\n" is a
newline). Everything the command printed is copied to stdout. Exits with the command's status, or
124 on a timeout / an expected text that never came. Uses only the standard library (macOS + Linux).
"""

import os
import pty
import select
import signal
import sys
import time


def main(argv):
    steps, timeout = [], 60.0
    i = 0
    while i < len(argv) and argv[i] != "--":
        if argv[i] == "--expect":
            steps.append([argv[i + 1], None])
        elif argv[i] == "--send":
            if not steps or steps[-1][1] is not None:
                sys.exit("pty-run: --send needs an --expect before it")
            steps[-1][1] = argv[i + 1].encode().decode("unicode_escape")
        elif argv[i] == "--timeout":
            timeout = float(argv[i + 1])
        else:
            sys.exit(f"pty-run: unknown option {argv[i]}")
        i += 2
    cmd = argv[i + 1 :]
    if not cmd:
        sys.exit("pty-run: no command")

    pid, fd = pty.fork()
    if pid == 0:
        try:
            os.execvp(cmd[0], cmd)
        finally:
            os._exit(127)

    out, seen, deadline = b"", 0, time.monotonic() + timeout
    while True:
        if steps and steps[0][0].encode() in out[seen:]:
            seen = out.index(steps[0][0].encode(), seen) + len(steps[0][0])
            os.write(fd, (steps.pop(0)[1] or "").encode())
        left = deadline - time.monotonic()
        if left <= 0:
            os.kill(pid, signal.SIGKILL)
            sys.stdout.buffer.write(out)
            msg = f"waiting for {steps[0][0]!r}" if steps else "waiting for exit"
            print(f"\npty-run: timeout after {timeout:.0f}s {msg}", file=sys.stderr)
            return 124
        r, _, _ = select.select([fd], [], [], min(left, 0.2))
        if not r:
            continue
        try:
            chunk = os.read(fd, 4096)
        except OSError:  # Linux: EIO once the child side is closed
            chunk = b""
        if not chunk:
            break
        out += chunk

    _, status = os.waitpid(pid, 0)
    sys.stdout.buffer.write(out)
    sys.stdout.flush()
    if steps:
        print(f"\npty-run: the command ended before printing {steps[0][0]!r}", file=sys.stderr)
        return 124
    return os.waitstatus_to_exitcode(status) if hasattr(os, "waitstatus_to_exitcode") else status >> 8


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
