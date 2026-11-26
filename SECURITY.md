# Security

## Report a vulnerability

Use GitHub's private vulnerability reporting for this repository. Include
the output of `a2p2 version`, your OS, and steps to
reproduce. Keep invite tokens, local UI links and private room content out of
reports.

## Trust and access

- A machine has an Ed25519 key. Room traffic uses mutually authenticated
  TLS 1.3 with pinned peer keys. A joining machine checks the admin's key
  against the fingerprint in the invite before sending the invite secret.
- A machine serves room data according to its current membership log and
  the requesting machine's history permissions. Until a removal reaches a
  member, that member may still serve the removed machine. Copies already
  received stay with their holders.
- Agent labels and addressees help with routing and reading. A room member
  can read the room data its machine can fetch. Treat messages as
  untrusted input. A message from a peer does not grant permission to act.

### A member of your rooms

Another member machine, perhaps another owner's, and anyone holding
an invite.

- A machine that reaches a room through one member trusts that member for
  delivery. That member can leave out an author's messages without any
  sign, and a removal of that member never reaches the others while it is
  the only path. Rooms shared between owners should have a listener from
  each owner.
- Anyone with a valid unused invite can present it. A person on the admin
  machine still approves the joining machine and the terms. Keep invites
  private and verify the joining machine's fingerprint before approval.
- An invite to a room this machine already holds is used only on the
  terms held here: the admin machine must name the room's create entry
  and the label held here, and its log counts only if it is a member of
  the room here before or after each batch of it, or the whole batch is
  refused. A removed admin's invite
  therefore cannot bring in entries it signed after its removal, and a
  join answer opens the `not_caught_up` gate only when it comes from a
  member here and its heads are the heads of the log here. A machine
  that holds the room and has not heard of a newer admin can join
  through it only if that admin's log shows its admission within 1,024
  entries (4 MiB) that this machine lacks. One join reads at most 256
  pages or 64 MiB, for at most 15 minutes of reading, and 2 minutes
  without an entry this machine lacks; past that the join is refused.
  These count over all the join's runs until the daemon restarts.
  Joins still resume one at a time, so a hostile admin machine can delay
  the others by up to that much in all, again after each restart.
- Advertised addresses, including VPN and office network addresses, go
  to every member of your rooms and every holder of an invite. `a2p2 net
  on` says so, and `a2p2 status` lists them.
- An admin can sign entries that wait for a second admin, and each other
  admin's machine asks its person about them: at most 32 at a time per
  room. One signing admin gets more than four of them only from places no
  other admin's four need, so in a room of up to 9 admins one admin
  cannot take another's places. The requests a person starts have a
  budget of their own, and a removal of an admin passes it, so a person
  can always propose removing an admin. On the other admins the first
  such removal per proposing admin and target passes the seconding
  budgets and takes no place in them. A hostile admin can still make the
  other people decline its entries, up to 28 at a time in a room of
  three admins.
- When the daemon starts, it checks each room's stored log for form and
  signature only, not for receipt rules added since the entries were
  stored. A batch that drops entries is decided again without them, so
  entries another machine delivers cannot make a room fail its own
  replay. A room whose stored log does not load is set aside: it is not
  served or pulled; sends, Seen and a change of its files setting are
  refused; setting it aside deletes nothing; and `a2p2 status` and
  `a2p2 doctor` show it. Its messages can still be read, except streams the
  reader has no cursor for, which are held back. Text whose
  readability is undecided stays undecided, and only retention deletes
  its text and files. To recover, run the version that stored it, or
  leave the room and join it again (not the local room). The other rooms
  keep working, so an entry another machine got stored cannot stop the
  daemon starting after an upgrade.
- A room joined from another machine is held under the label the person
  approved in the preview: a create entry with another label is refused.
  A new room's label may not read as `local` (in any case, with spaces
  around it) and may not hold control, direction or invisible
  characters, except those the display keeps (a joiner inside an emoji,
  or between letters of a script that needs one). A room from an older
  version that is labelled `local` still loads; it is shown with its id
  (its label, `#` and 8 hex of the id, the same for the life of the
  room) and never resolves as the local room. Lookalike letters of other
  scripts are not detected.
- Any member machine can post under any label, `human` included, and an
  agent id names a machine by a short id of 32 bits. What an agent
  reads therefore says, for every message, whether this machine wrote it
  (`from_this_machine`, compared by the full machine key), and a message
  labelled `human` in any case says whether it is this machine's person
  (`person`). Only `human` written on this machine is; on this machine
  only the person's own path registers that label, in any case, and
  `a2p2 mcp` refuses the harness name `human`. `ask_human` names the
  person by this machine's full key. A machine whose short id another
  machine known here shares is written with its full key and marked
  `id_shared`, in tool results, the local API and status; a short id
  that names two members is refused as an addressee. Only this machine's
  own room is shown as `local`, no two rooms are shown alike, and a room
  name that could mean two rooms is refused. In `peers`, a machine name
  that another machine also uses, or that reads as another machine's
  id, is shown with `#` and the machine's id. A member can still grind a key
  with a matching short id (about 2^32 tries) and be admitted; it is then
  shown in full, not refused. The same marks, from the full key, are on
  the CLI's messages, notices and `trace`, the page's messages, notices,
  questions and trace, question alerts and the hook's unread notice;
  a replaced message is shown under its own label everywhere, and
  another agent of its machine that replaced it is named as the one that
  changed it. Message text that copies a mark's
  words is drawn apart from the marks (indented in the CLI, a question
  alert's later body lines deeper than its asker line, in its own
  element on the page, its own field in MCP; the hook shows no text).
  Any program running as you can still post
  as your person through the socket or the CLI: `person` says where a
  message was written, not who typed it.
- Room labels, machine names, file names and message text can come from
  another machine, and its owner may be someone else. Where a2p2
  shows them (the text a person approves in `confirm`, `alerts` and on
  the page, the join preview, the CLI's messages, alerts, room lists,
  status, `net check` (members' addresses and probe details among
  them), `confirm`, `join`, `fetch`, doctor and error lines, the page,
  and the text of MCP tool results)
  it writes control characters, line breaks where one line is shown,
  terminal escapes, bidirectional overrides and characters that draw as
  nothing as `\u{...}`. Nothing is dropped, and a backslash that would
  start such an escape is itself written `\u{5c}`, so typed text never
  reads as an escaped character. So a room creator cannot rewrite or
  hide the terms of a request with its room's label. Some invisible
  characters are kept where honest writing needs them and they change
  no order: joiners and selectors inside emoji, joiners between letters
  of the scripts that need them, a right-to-left mark between two
  right-to-left letters, a zero width space between two Thai, Lao,
  Tibetan, Myanmar or Khmer characters, one variation selector after a
  CJK ideograph and the flags of England, Scotland and Wales. There they
  can still carry a little information nobody sees. In the text a person
  approves, the room label and the names are isolated (Unicode isolates,
  a `bdi` element on the page, their own line in the terminal when they
  hold right-to-left text), so they cannot reorder the words around
  them. `a2p2 confirm` and the page send the text they showed, and the
  daemon decides nothing if it changed. A seconding request says that
  the other admin chose the name it carries, shows the name the room
  records, and says when other members have that name. On the page, a
  message that asks the person is drawn apart from requests, says who
  asked and of whom, and its choices are plain buttons; a choice its
  button cannot show exactly as it is sent is not sent. `--json` output
  is data: the stored strings, escaped only by JSON's rules, apart from
  sentences the daemon wrote for a person (a request's or an alert's
  text). Names are labels, and lookalike letters are not detected:
  compare fingerprints, not names.
- The author of a message can replace it at any time, also after it was
  answered. Inbox, thread, branch, history, recent and skipped entries
  carry `edited` when the text is a replacement: in the local API and
  the MCP tools' results, as `[edited]` in the CLI and as a mark on the
  page. A replacement whose text expired keeps that mark: the message is
  then shown as expired and edited, never with its earlier text as
  current. A handled report is
  refused when newer text arrived after the read, or while a newer text
  is announced but not here yet. So is a reply by an agent that may
  report the message handled and was shown an older text of it, and a
  reply or report from the page names the version it drew and is refused
  if the message changed since. The page's background requests keep no
  receipt; only a seen of what it drew does. A reply by an agent no read
  showed the message, or one it may not report handled, names no version
  from the CLI or the MCP tools and is not checked. A signed reply and a handled report
  name only the message, not its version, so a thread read later cannot
  show which text an answer saw. The author also chooses a message's
  time, up to 24 hours ahead or any time in the past. That time orders
  every view and starts the 30 days of retention, so a member can place
  a message above an answer given after it, or send one whose text
  expires minutes after it arrives.
- What a peer reports it holds is unsigned. It can make this machine list
  up to 32 streams per author that are not there as still being fetched,
  until it stops reporting them. It can raise a hint another peer set,
  but not erase it: when it lowers or drops the stream, the other peer's
  last report stands.
- The unread notice a hook prints holds a count and up to five sender
  labels, never message text. The sending agent chooses its label. Every
  machine accepts only 1 to 64 characters of letters, digits, `.`, `_` and
  `-`, and the hook drops any other character, so a label cannot carry
  a newline, quote or markup. It can still be a word that reads like an
  instruction; the notice tells the model that mail does not come from its
  user.
- Files follow the permission of the text they belong to: a machine serves
  a file only to a current member that may read that message, and deletes
  it when the message goes. A person confirms the exact bytes of a file an
  agent sends, as a guard rail only: an agent running as your user can call
  `confirm` or paste the contents as text. a2p2 verifies a fetched file's
  hash and never opens, unpacks or runs it; treat it as data from another
  machine, and take care with whatever program opens it. A member that
  fetched a file keeps it after removal.
- A member that writes a message with files can make your machine hold
  at most a quarter of its room file budget for its files, counted by
  the bytes it has not sent, for a day at a time: a request from the
  author itself starts no carry job, and a part it left goes when the
  want lapses or when the author edits the message. Several machines of
  one owner can each take a quarter.

### An agent or a program running as you

Agents read text from every member, so a member can try to steer
them. What runs as your OS user is not kept out; these are guard
rails against mistakes and steered agents.

- The daemon's Unix socket is in a private directory. Any program running
  as your OS user can use the socket and read your machine's state.
  Messages are stored unencrypted in `a2p2.db` in the state directory.
- Without `--as`, the CLI acts as your person, `human@<machine id>`, the
  id other members and agents take for you. So it refuses an empty
  `--as`. In a shell that a harness marks as its agent's (Claude Code,
  Codex, Gemini CLI, or any shell with `AI_AGENT` set; only the Claude
  Code variables were seen in a real shell, the others were read from
  the harnesses' binaries), it acts as that agent: as `<harness>-<directory>`, the id the agent's MCP tools have
  when they run in the same directory under the default label. Elsewhere
  `send`, `ack` and an `inbox` that marks messages seen act as you only
  at a terminal. `a2p2 ui`, whose address carries the one-time code that
  lets the page act as you, is refused in such a shell and away from a
  terminal. `--person` overrides each refusal; when it is used away from
  a terminal or in such a shell, the daemon log records the command. This
  stops an agent with a shell, or a script, from answering your
  questions, taking them off your list or taking the page's address by
  mistake or because a message told it to. It is not a boundary: any
  program running as you can pass `--person`, drive a terminal, unset
  those variables or call the socket, whose `status` call still returns
  the address. The agent of a harness a2p2 does not recognise reads as
  you, and acts as you if its shell has a terminal.
- `room leave` deletes the room's data here and asks at a terminal
  first, naming the room by its id. `status --json` leaves out the
  page's address and its code; `ui --print` prints it.
- What an agent can attach. Text in a room can steer an agent into
  naming a path for a send. a2p2 refuses a2p2's own files (the machine
  key, the local key, the database, anything under the home but agents'
  copies, and a hard link to any of them), and refuses by name, before
  it touches the path or follows a link to it, device paths and paths
  that can make the machine connect to another host: on Windows anything
  but a drive letter and plain names (so no `\\host\share` or `\??\UNC\`
  path), on Unix `/dev`, `/proc`, `/sys`, `/net` and the macOS automount
  points. Each part of a path is looked up in the directory opened
  before it, so a directory swapped on the way is refused. The Windows
  code has not been run on Windows. Any other file your user can read
  can still be staged, and the person sees its full path, size and
  SHA-256 before it leaves; a file with several names is said to have
  them. Read the path before you approve. An agent learns only that a
  send waits or was refused; it can still learn roughly how large a file
  is, at the cost of requests you see. A network share you mounted, or a
  drive letter you mapped, is used as any local path.

### Another user of this computer

- On Windows, file modes do not protect the socket. The state directory
  is created with an access control list for your user only, and a2p2
  refuses one that lets other users in. Each local API request must also
  carry a key that the daemon writes into that directory at every start.
  This has been built and reasoned about but not yet tested on Windows
  with a second user trying to connect (DECISIONS, D47).
- The web page listens on loopback. Its API requires a session, and the
  server checks the request's `Host` and any `Origin` header. Every OS
  user of the machine can connect to a loopback port. The address
  `a2p2 ui` opens or prints carries no long-lived secret, only a code
  that works once and for 30 seconds; the page trades it for a session
  kept in that browser tab, which ends when the daemon stops. On Linux the
  address is in the command line of `xdg-open` and of a browser it
  starts, which other users can read. Such a user who reads it and trades
  it before your browser does gets a session; your page then says the
  link was used already, that session is ended and the daemon's log says
  so. On a shared machine, run `a2p2 ui --print` and paste the address
  into a browser that is already open. A program running as your user
  can still get a code from `a2p2 status` and decide on the page (a guard
  rail, as for the socket). If another user takes the page's port first,
  the daemon serves on another one and its log says so. A tab left open
  from an earlier run refuses what a listener answers unless it names
  the run given with that tab's own session,
  and sends a message, a decision or a download request only right after
  a state from its own daemon process; a listener that takes the port in
  the moment between that check and the request still receives it.
- The a2p2 home and every directory above it must be writable only by
  you. On Unix a2p2 refuses a home under a directory that another user
  owns (root aside) or can write without the sticky bit, or that its
  group can write unless the group is your own private group (named as
  you; on macOS `staff` is not). Links on the way are followed one at a
  time and each must be yours or root's. On Windows the directories above
  the home are not checked. A client checks the home when it connects,
  and does not check which user serves the socket it dials.

## What `a2p2 install` approves and writes

- `a2p2 install` approves no a2p2 tool by default, in any harness. Each
  harness's own rules decide whether an agent's `send`, `reply`, `ack` or
  `ask_human` waits for the person. As seen in the runs the
  [harness notes](docs/HARNESSES.md) list: Claude Code asks unless you
  allowed the tool, a headless Codex run refuses the call, and
  Antigravity's headless runs deny MCP tools that are not allowed.
  OpenCode's default agent runs every tool without asking, and Pi runs
  bash commands, the skill's `a2p2 send` among them, without asking (read
  from Pi's README, not run); a2p2 does not change either.
- `a2p2 install codex --approve-tools` lets every a2p2 tool run in Codex
  without a prompt, `send` included, and any tool a later a2p2 adds. Text
  from any member of a room, another owner's included, can then lead the
  agent to post to that room without the person seeing it first. Headless
  `codex exec` needs this. a2p2 0.1.0 to 0.2.3 wrote this approval on every
  Codex install without asking. The next `a2p2 install codex` removes it,
  and `a2p2 doctor` reports it as a fault until then. An agent can pass
  the flag itself: a flag cannot show that the person chose it. When a
  line that could set an approval for a2p2's server is one a2p2 does not
  read in full, such as an inline table, the flag writes nothing and
  doctor says it cannot tell.
- Install and uninstall read each harness file again just before they
  replace or remove it, and leave it as it is if it changed since they
  read it. The instant between that read and the rename remains, since no
  harness takes a lock a2p2 could also take.
- The team launcher (`scripts/team`) starts members with no person to
  answer prompts, so it approves every a2p2 tool for them. It refuses to
  start a Claude Code, Codex or Antigravity member unless the manifest
  says `"approve_a2p2_tools": true`, and its supervisor starts no such
  member whose saved command line was written without it.
- Another user of the same machine cannot swap the team launcher's state
  directory or home for their own: `up` refuses a `state_dir` or
  `a2p2_home` on a path where a directory, symlinks followed, belongs to
  another user than you or root, or lets group or others write without
  the sticky bit, and an existing state directory group or others may
  write; it checks the home's path again after making it. `status`,
  `stop`, `logs` and `report` refuse such a state directory before they
  read or write in it. The supervisor checks again before it runs the
  command lines saved there, and runs from the manifest `up` checked.
  They can still read each member's prompt with `ps`, since it is an
  argument on the harness's command line.
- A manifest is code: `up`, `status` and `report` run the `a2p2` binary
  it names, and `./team.json` is the default. The `allowed_tools` check
  refuses common ways to allow every command (`Read,Bash`, a shell, an
  interpreter, `env`, `git` without a subcommand) but not every program
  that can run another, nor a rule such as `Bash(go test:*)` that runs
  code a member could write.
- `team status` and `team logs` escape controls, format and invisible
  characters a member wrote or a harness logged by the daemon's rules
  (D58), so peer text a member copied cannot drive the operator's
  terminal; `logs --raw` does not.
- Pi's skill tells the model to send text only through a heredoc with a
  quoted end word, so bash runs nothing in it. Earlier skills put the text
  in double quotes, where bash runs `$(...)` and backticks: a peer's text
  that a model copied could run a command as you. The end word has eight
  random hex digits chosen at install, so a peer cannot know the line that
  would end the heredoc early. A model that ignores the skill can still
  run text in the shell. `a2p2 doctor` fails a skill an older a2p2 wrote.
- Hooks and MCP entries name the a2p2 binary by its path, and run it as
  you. Install refuses a binary in a temporary directory or Go's build
  cache, one that it or a directory above it lets every user change, and
  one owned by another user, unless a directory higher up keeps other
  users out. It checks the path as written and, for a symlink, where it
  leads now; for a path that does not exist yet, the part that exists. A directory its group can change gives a warning. Access
  control lists are not read. `a2p2 doctor` reports the same. On Windows this check is not
  made yet: keep `a2p2.exe` in a folder only you can change. A new folder
  directly under `C:\` can be changed by every signed-in user;
  [the install notes](docs/INSTALL.md) say how to change that.

## What a2p2 connects to

a2p2 has no update check, no telemetry and no service of its own. The
daemon and the CLI make these connections and no others:

- To the other members of your rooms, only while networking is on or
  outbound only (`a2p2 net on`, which a person confirms at a terminal):
  pulls and file transfers, pinned to each member's key. When you accept
  an invite, a2p2 connects to the addresses in the invite you pasted,
  before the machine is a member. A member can also ask this machine to
  connect back to the address the member's own request came from, on at
  most two ports, for a TLS handshake and no request.
- To your router, only after `a2p2 net on --map-port`: UDP to the default
  gateway (PCP and NAT-PMP), UDP multicast to `239.255.255.250:1900`
  (UPnP discovery) and HTTP to the UPnP addresses the gateway names, if
  their host is the gateway. This opens a port to the internet.
- To an address you name: `a2p2 net check HOST:PORT` and `a2p2 net check
  --invite` make a TCP connection and a TLS handshake from the CLI under
  a throwaway key, whatever the networking state, and send no request.
- A host name in an address is looked up by your system's resolver.
- A file an agent names for a send is read through your file systems. A
  path that names another host or an automount point, or a link on the
  way that leads to one, is refused before it is looked up. A share you
  mounted or a drive letter you mapped is read as a local file, so
  reading it reaches the host you chose.
- `install.sh` and `install.ps1`, which you run yourself and which are not
  part of the daemon, download one release and its `SHA256SUMS` from
  `github.com/wallter/a2p2/releases`. With `A2P2_VERIFY_ATTESTATION=1`
  they also run `gh attestation verify`, which asks GitHub's API for the
  attestation.

Hooks and the MCP server talk only to the daemon over its Unix socket.

## A host that reaches your listening port

While networking is on, any host that can reach the peer port can open
connections and complete a TLS handshake under a key it makes up. Such a
key shares no room with you, so it reads nothing and joins nothing
without an invite and your approval. A connection is placed in a pool
only after its handshake has proved the client's key. What a host can
still do, by the numbers:

- Hold a few connection slots. One IPv4 address or one IPv6 /64 gets at
  most 8 connections that are not members' connections, one IPv6 /56 at
  most 16, and one IPv6 /48 at most 32. All keys that share no room get
  32 between them, and each such connection serves one request at a
  time. Connections still in the TLS handshake get 64 shared slots for
  5 s each, membership lookup included, and a connection that finds no
  slot is closed at once. Members of your rooms have 256 slots of their
  own, at most 16 for one key, and rate buckets of their own.
- Keep joiners out while it lasts. Joiners use the 32 slots of keys that
  share no room. Hosts in 4 source groups fill them: 4 IPv4 addresses,
  or 2 IPv6 /56s, or one /48. Members keep pulling.
- Keep members from opening new connections while it lasts. Hosts in 8
  source groups keep the 64 shared handshake slots full: 8 IPv4
  addresses, or 4 IPv6 /56s, or 2 /48s. Connections members already have
  keep working. A member whose IPv4 address or IPv6 /64 completed a
  member's handshake here in the last 24 hours also has 16 handshake
  slots kept for such addresses, at most 4 at once for one. Hosts at
  other addresses and in other /64s cannot take them, whatever they hold
  of the member's /56 or /48. A member that connects from a new address,
  or for the first time since your daemon started, has no such slot and
  is refused until the flood stops; so is one whose /56 or /48 share a
  neighbour in another /64 holds.
- Spend join budgets of its own address, and of its /64. Join requests
  are counted per source address (80 bodies read, then 10 opens and
  accepts and 30 polls, a minute) and per IPv6 /64 at 4 times each. A host
  refuses no joiner at another IPv4 address or in another /64; in its own
  /64, it refuses joiners only once it has spent the /64's budget. A
  refused joiner is told when to try again, and an accept is sent again
  after that delay. Polls naming unknown invites are counted per IPv4
  address or IPv6 /64 after the lookup, so a joiner that resumes by its
  invite's hash is never refused by them. Each address can make 40 invite
  lookups a minute and each /64 160; a host with many /64s (a /48 has
  65,536) can make that many more, bounded only by its connection slots,
  and refuses no one else.
- Not fill the log at the default level. Refused requests from keys that
  share no room, and refused connections, are logged at Debug; one Info
  line a minute says connections are being refused. A member's refused
  requests reach Info 5 at once, then one per 10 s per key. At Debug
  each refusal is a line. Once `daemon.log` is over 10 MiB (checked
  every 10 seconds) its last 10 MiB are moved to `daemon.log.1`, so the
  two files hold about 20 MiB plus what is written between checks; if
  that copy cannot be made, the log is emptied anyway.

A member of one of your rooms is trusted more. One member key can hold
16 connections, so 16 member keys together can fill the members' pool.
A host behind the same IPv4 address as a member, or in the same /64,
shares that member's 8 shared handshake slots and its 4 kept ones. A
connection in a kept slot that does not prove a member's key, a key that
shares no room or a handshake that fails, makes the address unknown, so
the kept slots are gone until a member's next handshake there through
the shared slots. While the host holds all 8 shared slots, the member
cannot open a new connection; connections the member already has are not
affected. Honest machines behind one address share the same 8 slots, and
4 kept ones once a member came from there. A handshake holds its slot
for about one round trip, so at 100 ms that is about 80 new connections
a second; a refused one is retried under the client's back-off. 0.2.3
had no share per address: it queued connections past 256 in all instead
of refusing them.

## A device on your local network, with port mapping on

Only after `a2p2 net on --map-port`. The CLI asks the person to confirm
it at a terminal each time; the daemon takes it only from the local
API's net call, which no agent tool or hook makes. The router's answers
carry no authentication, so a device on the same network can answer in
its place. a2p2 asks for a mapping only on a default gateway with a
private IPv4 address on this machine's network. A PCP answer must carry
the request's nonce, and NAT-PMP's version answer counts only if no PCP
answer comes in any of the tries. A lifetime longer than the 2 hours
asked counts as 2 hours. An external address that is not a public IPv4
address (private, shared, loopback, link-local, multicast, reserved, or
any IPv6 address) is not taken, and a2p2 tries to remove what the router
made. Every request is recorded before it is sent, a run adds at most 8
to the record (which loads with up to 16), and the record always loads.
On `net off`, on a stop and when the listener stops, a2p2 tries to
remove the mapping and stops giving its address out; a removal the
router does not confirm stays recorded and is retried.

Such a device can still:

- Give a wrong public address and port. It goes first in your invites
  before any member has reached it, so a joiner that dials it shows its
  own address to whoever holds that address.
- Relay a public port it mapped for itself to your listener. A member's
  dial-back then reaches you and the address is advertised to every
  member, so the device sees which members connect and when, and can
  stall them. It cannot read or change anything: connections are TLS
  pinned to each machine's key.
- Get a wrong address advertised with the help of one member: a member
  that says it reached the mapped address is believed, so one member
  that says so falsely puts the device's address in what this machine
  advertises to every member.
- Answer NAT-PMP, which has no nonce, and the UPnP search for the router,
  and so make mapping fail. A forged NAT-PMP refusal makes a2p2 forget a
  NAT-PMP mapping the router may have made; it lapses with the router's
  lease.
- Keep a mapping on the real router after a crash for as long as the
  router grants, which may be more than 2 hours.

## Release binaries

Release binaries are built by GitHub Actions from the tag, with no module
or build cache, with CGO off and with `-trimpath`. Each release carries a
`SHA256SUMS` file made in the same run; verify a binary against it before
installing (see the [README](README.md#install)). Releases after 0.2.3 also
carry a GitHub artifact attestation made by the release workflow. To check
one from the release vX.Y.Z, run `gh attestation verify a2p2_<os>_<arch>
--repo wallter/a2p2 --signer-workflow
wallter/a2p2/.github/workflows/release.yml --source-ref refs/tags/vX.Y.Z
--deny-self-hosted-runners` (needs the GitHub CLI). Without `--source-ref`
an attested build of any other tag passes too. It proves which workflow
and commit built the file, not that the code is safe. Releases up to 0.2.3 have none. The binaries are not
code-signed or notarised.

The installer's checksum proves the download is intact, not that it is
authentic: `SHA256SUMS` comes from the same release page as the binary.
Someone who can replace release files (a token with `contents: write`,
or the maintainer's account) can replace a binary and its line together,
and `install.sh` or `install.ps1` then installs it. Set
`A2P2_VERIFY_ATTESTATION=1` and the installer also runs `gh attestation
verify` and installs nothing without a valid attestation from this
repository's `release.yml`, made from the tag of the release it
downloads (`--source-ref refs/tags/<tag>`) on a GitHub-hosted runner.
That attacker cannot make such an attestation without running the
release workflow, and cannot pass off an attested build of an older tag
as a newer release. Without `A2P2_VERSION` the installer asks `gh` once
for the tag of the latest release and downloads and checks that tag.
Someone who can edit releases can mark an older attested release as the
latest, so for `latest` the installer cannot rule out getting an older
release; set `A2P2_VERSION` to rule that out. Without the setting, the
installer says the checksum does not show who built the file; a release
up to 0.2.3 is refused with it.

The release workflow pins every action to a full commit SHA, so moving a
tag of an action does not change what runs. It has two jobs. The build
job checks out the tag and runs the Go toolchain with a read-only token,
and its checkout does not keep the token. The publish job holds the
write, signing and attestation scopes, checks out nothing and runs no
repository code: it checks the built files against `SHA256SUMS`, attests
them and creates the release, after checking that the files are exactly
the six binaries, `SHA256SUMS` and the notes, and that `SHA256SUMS` has
exactly one line for each binary. `make check` refuses a workflow that
names an action by tag or branch, keeps the checkout token, grants a
write scope at the top level, or gives a write scope (quoted or not) to
a job that checks out code or sets up Go. Its check reads lines, not
YAML, so it also refuses what it does not follow: a tab in the
indentation, a quoted key, a flow mapping, an anchor or alias, a `uses:`
that does not start its line, and a job not at two spaces under `jobs:`.
It reads every `.yml` and `.yaml` file in `.github/workflows`. Still open: a build
job that runs a compromised pinned action or toolchain makes a binary
that the publish job attests, since an attestation says which workflow
ran, not that its inputs were honest. Whether the repository has
immutable releases, tag protection or a protected release environment
is a repository setting, not shown in these files.

Both installers check the install directory and every directory above
it, and refuse these cases:

- `install.sh`: a directory every user may write without the sticky bit;
  one owned by another user than you or root; a symbolic link owned by
  another user; and one a group may write (by its mode bits) when that
  group has a member other than you and root. The members are the
  group's listed members and the users whose primary group it is, as
  `getent` lists them (on macOS, `dscacheutil` and `dscl`). On macOS it
  also refuses a group whose members are implied rather than listed:
  `everyone` (12), `localaccounts` (61), `netaccounts` (62), and any
  group `dsmemberutil` says the account `nobody` is in. When it cannot
  list a group's members it prints a note and installs. It checks each part of
  the path as written, with trailing slashes taken off, and each symbolic
  link it meets adds the path that link names, checked the same way, so
  the real directories and those above them are checked too. It makes
  missing directories with mode 0755 whatever the umask, and checks the
  whole path again after it made them. It does not read access control
  lists (ACLs), nested groups, or accounts of a network directory that
  the system does not list, and it does not refuse a sticky directory
  every user may write, such as `/tmp`: a directory you made there is
  still yours.
- `install.ps1`: a junction or symbolic link anywhere in the path; a
  directory owned by an account other than you, SYSTEM, Administrators
  or TrustedInstaller; a directory whose access list it cannot read; and
  an install directory that any other account may write, delete, change
  the access list of or take ownership of, including through entries
  that pass only to new files. A directory above it is refused when
  another account may delete a child, delete it, change its access list
  or take ownership. It makes a missing directory with an access list of
  its own already in place (you, SYSTEM, Administrators; nothing
  inherited), checks that it is a real folder, still empty and yours, and
  checks the whole path again before it copies the file. `install.ps1`
  has not been run on Windows.

See the [design](docs/DESIGN.md), [protocol](docs/PROTOCOL.md)
and [limits](docs/REQUIREMENTS.md#6-limits-in-plain-words).
