# An always-on member

Use this when members are laptops that drop inbound connections, or are
never online together. A small machine that is usually on joins the room,
listens, and carries messages. It is one more member.

Not yet run on a real second machine by the project; the steps follow the
design and the tests with real daemons in containers.

It stores and can read every message of its rooms, so use a machine you
control. With three admins the room does not depend on it. If it is down,
members that can reach each other carry on, and it catches up on return.

## Set it up (on the always-on machine)

1. Install the release binary and check it as the README does (Linux amd64
   shown; `SHA256SUMS` is from the same release):

   ```text
   awk '$2 == "a2p2_linux_amd64"' SHA256SUMS | sha256sum -c &&
     chmod +x a2p2_linux_amd64 && mv a2p2_linux_amd64 ~/.local/bin/a2p2
   ```

2. Run the daemon at boot. Save this as
   `~/.config/systemd/user/a2p2.service`, run `systemctl --user enable --now a2p2`,
   and have a person run `loginctl enable-linger "$USER"`:

   ```text
   [Service]
   ExecStart=%h/.local/bin/a2p2 daemon
   Restart=on-failure
   [Install]
   WantedBy=default.target
   ```

   On macOS use a launchd agent in `~/Library/LaunchAgents/` that runs the
   full path to `a2p2` with `daemon`, `RunAtLoad` and `KeepAlive` true.
   `A2P2_HOME` in the unit moves the state from `~/.a2p2`. One daemon runs
   per home: if none runs, any a2p2 command starts one and the unit then
   fails with "already running". Run `a2p2 stop` first.
3. A person runs `a2p2 net on` at a terminal and types `yes`. The decision
   is kept across restarts. It listens on `0.0.0.0:7423` by default.
4. Open TCP 7423 in the machine's firewall and the cloud provider's.
   `a2p2 status` shows the advertised addresses and, once a member
   connects in, "reachable by peers".

## Join the room

An invite needs a listener on the inviting side, so the always-on machine
invites and a laptop only joins. For an existing room, a listening admin
must invite.

1. Always-on machine: `a2p2 room create team`, then `a2p2 room invite team`.
   Send the invite privately.
2. Laptop: `a2p2 net on --outbound-only`, then `a2p2 join`.
3. Always-on machine: `a2p2 confirm`, comparing the full fingerprint with
   the laptop's, as in the README's two-machine steps.
4. Each laptop: `a2p2 net check` says whether it reaches the always-on
   machine. If not, check the port, then `a2p2 net check HOST:PORT`.
