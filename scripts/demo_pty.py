#!/usr/bin/env python3
"""Run a command on a pseudo-terminal and type an answer at its prompts.

    demo_pty.py ANSWER COMMAND [ARG...]

a2p2 asks for a person's decision only at a terminal, so `net on`, `join`,
`confirm` and `send --file` cannot be driven through a pipe. This runs the
command with a terminal as its input and types ANSWER and Enter each time
the output ends with "Type yes:". The command's output goes to stdout with
the terminal's carriage returns removed; the exit status is the command's.
Used only by scripts/demo.sh.
"""
import os
import pty
import select
import sys

if len(sys.argv) < 3:
    sys.exit("usage: demo_pty.py ANSWER COMMAND [ARG...]")
answer, cmd = sys.argv[1], sys.argv[2:]

pid, fd = pty.fork()
if pid == 0:
    try:
        os.execvp(cmd[0], cmd)
    except OSError as e:
        sys.stderr.write("demo_pty: %s: %s\n" % (cmd[0], e))
        os._exit(127)

pending = b""
out = sys.stdout.buffer
while True:
    ready, _, _ = select.select([fd], [], [], 0.3)
    if ready:
        try:
            data = os.read(fd, 4096)
        except OSError:
            break
        if not data:
            break
        out.write(data.replace(b"\r\n", b"\n"))
        out.flush()
        pending += data
    elif pending.rstrip().endswith(b"Type yes:"):
        os.write(fd, (answer + "\r").encode())
        pending = b""

_, status = os.waitpid(pid, 0)
sys.exit(os.waitstatus_to_exitcode(status))
