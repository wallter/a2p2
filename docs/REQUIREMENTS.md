# Requirements

**Status.** Draft 0.4 (2026-11-23), with build notes from 2026-11-24 in
[section 10](#10-decided-and-what-still-needs-the-owner). These requirements
include the unreleased stage 1 core and later work such as subscription
levels (U1–U4), large rooms and attachments (L1). See
[Design](DESIGN-next.md) for stage 1 and [Testing](TESTING.md) for checks
on the code, and the [requirement register](TRACE.md) for which
requirements the code meets today. The v0.2 design describes the previous
delivery core.

Draft 0.4 applies findings from the [membership model](../internal/members/FINDINGS.md)
and [delivery model](../internal/model/delivery/FINDINGS.md), mainly to
D3 to D5, D7, T3, T9 and [the limits](#6-limits-in-plain-words). Draft 0.3
added several admins per room, the option to share earlier history, and
threads based on the owner's answers to draft 0.2.

## 1. What a2p2 is

A messaging layer for AI agents and the people who run them. It works across
agent harnesses, across machines and across networks, with no infrastructure
run by anyone but its users.

It is a mechanism, not a workflow. The core carries messages reliably and lets
each participant choose how much it receives. How agents summarise, triage or
act on messages is documentation and recipes that an installing agent adopts
and adapts.

The nearest familiar picture is a team chat with rooms and threads, for
agents, where the machines in a room carry the messages among themselves and
there is no server.

## 2. Words used here

| Word | Meaning |
|---|---|
| Machine | One computer running the a2p2 daemon. It may host many agents |
| Agent | One mailbox on a machine: a harness session, a script, or a person |
| Room | A private group of machines that share messages |
| Member | A machine admitted to a room |
| Admin | A machine whose key may change a room's member list. A room can have several |
| Copy | A machine has a copy of a message when it has saved it to disk |
| Handled | An agent said, explicitly, that it has dealt with a message |
| Addressed | A message names the agents it is for, or is marked for everyone |
| Thread | A message and the replies under it, to any depth |

## 3. Who it serves

| Situation | What it needs from the core |
|---|---|
| One person's laptops and workstation | Pair once, easily; messages reach a machine that was asleep |
| A team in several time zones | A message sent before one person sleeps reaches another who wakes later, through any machine that was up in between |
| An engineer on one project | A few rooms, everything delivered |
| An architect moving between teams | Change how closely a room is followed, alone, and read what was missed |
| A manager who wants status only | Announcements and direct questions without the rest; a cheap overview |
| Unattended agents (incident stream, root-cause analysis) | Take part with no harness, through the CLI or socket; refer to a message in another room |
| Company-wide announcements | Very large membership, low rate, only admins post, most receive little |
| Many agents per machine | Agents share their machine's connections and its one copy of each message |

## 4. Principles

- **P1 Bare core.** If something can be a recipe, it is not a feature. Every
  tool, flag, option and message type must justify itself in
  `docs/DECISIONS.md`.
- **P2 No required machine.** Nothing is hosted by a third party, and no
  single machine must be up for messages to flow among the machines that are
  up. An always-on machine helps and is otherwise an ordinary member.
- **P3 Agents operate, a person confirms trust.** An agent can use and tune
  the layer by itself. Changing who is trusted or what the machine exposes
  needs a separate confirmation on that machine: a step that is not among
  the agent tools, is made at a terminal or the local page, names exactly
  what is approved, and is recorded. The daemon will not act without it.
- **P4 Honest guarantees.** Every guarantee has a test named for it. Limits
  are stated in plain words where users will see them.
- **P5 Cost is multiplied.** Bytes on the wire, connections, disk, and tokens
  in an agent's context are budgeted and measured, because each is paid on
  every machine and in every agent turn.
- **P6 Reuse before building.** Adopt an existing library or protocol when it
  covers most of the need; when none does, borrow its lessons and say which.

## 5. Functional requirements

### Delivery

- **D1 Durable send.** Once `send` returns, the message survives a crash or
  restart of the sending machine.
- **D2 Store and forward.** A message reaches a member machine that was
  offline when it was sent, from any member machine that has a copy, without
  the author being online.
- **D3 At-least-once, no duplicates.** Retries, lost responses, restarts and
  multiple paths never put a message in an inbox twice. The exceptions are
  the ones in section 6: a crash between being shown a message and confirming
  it, and a restore from backup on the reader's own machine.
- **D4 Order.** Messages from one author arrive in the order sent. No order
  is promised across authors. If an author's machine is restored from a
  backup, what it wrote before the restore may arrive after what it wrote
  later, and is marked as late.
- **D5 Acknowledgement.** The sender can see three things: the message is
  saved here; which machines it has been in touch with reported a copy, and
  when; and that the recipient agent reported handling it. A copy report is
  a dated observation, not a promise that the copy still exists. An agent
  can report handling only for a message whose text it was shown. Handling
  reports are sent for addressed messages and travel like any message, so
  they arrive even if sender and handler are never online together.
  Room-wide posts get none unless the sender asks.
- **D6 Self-healing.** After any machine sleeps, restarts or loses power, and
  after a network split ends, machines catch up without anyone acting.
- **D7 Retention.** A room keeps the text of messages for 30 days, measured
  by each machine's own clock, and keeps a small signed stub after that so
  that what expired can still be accounted for and passed on. A message that
  expired before it could be delivered is shown as missing, never silently
  dropped. Text is missing on a machine only for a reason that machine can
  check itself: it expired, its author withdrew it, or the machine was added
  without earlier history. Otherwise the machine keeps trying to fetch it.
- **D8 Authorship is provable.** A machine can verify who wrote a message no
  matter which machine passed it on.
- **D9 Withdraw and replace.** An author can withdraw a message or replace
  its text. Agents that have not yet been shown the original never see it,
  and machines drop the withdrawn text. A message already read by an agent
  cannot be taken back; the reader is told it was withdrawn or replaced.
- **D10 Threads.** Any message can be replied to, and a reply can itself be
  replied to. Each reply records the message it answers and the message that
  started the thread, so several threads run in a room at once and can
  branch. A reply is addressed to the author of the message it answers
  unless the sender says otherwise; the reply tool and command also address
  those who have posted in the thread. An agent can read one thread, or the
  branch under one message, without reading the rest of the room. When a
  parent was withdrawn, has expired, or predates the reader's admission, the
  reply is still shown, with the parent marked as missing.

### Rooms and trust

- **T1 Private rooms.** Only admitted machines can read or write a room.
- **T2 Machine identity.** A machine is identified by a key it generates.
  Agent names are labels a machine assigns, not credentials.
- **T3 Explicit admission.** A machine joins a room only by an action of one
  of the room's admins and an action on the joining machine. Pairing takes
  one command and one confirmation on each side and one copy-and-paste;
  nothing long is typed. The person adding a machine chooses whether it may
  read what was said before it joined; if they do not choose, it may not. A
  machine added without earlier history still carries the signed stubs of
  earlier messages, without their text, so it can pass the room's history on
  to others intact.
- **T4 Admins stay out of the way.** Any member can show another member a
  genuine member list. Only changing it needs an admin. Messages never wait
  for an admin, and nothing an agent does day to day depends on knowing who
  the admins are.
- **T5 Removal.** A removed machine stops being served by each member as soon
  as that member learns of the removal.
- **T6 Posting policy.** A room is either open to all members to post, or
  admin-only. Nothing finer: role-based access control is out of scope.
- **T7 Encrypted in transit**, between authenticated machines.
- **T8 Peer text is data.** Messages are delivered to agents labelled as
  coming from a peer, are never instructions from the agent's user, and never
  count as a person's consent.
- **T9 Several admins, none special.** A room can have several admins, and
  its creator has no power the others lack. Any one admin can add or remove a
  member machine, so a room keeps working when an admin machine is lost.
  Adding or removing an admin takes two admins while the room has more than
  one, so a single stolen key cannot take a room over. When admins act
  without knowing of each other, every machine reaches the same member list
  whatever order the changes arrive in, and a removal is never undone
  silently. A machine's vote on its own removal does not count. A change
  that is waiting for a second admin is shown as waiting. Unless the person
  adding them says otherwise, the first three machines in a room are its
  admins; making the third an admin takes a person's confirmation on both
  machines that already are.
- **T10 No automatic promotion.** No machine becomes an admin because another
  has been quiet for some time. A machine cannot tell an admin that is gone
  from one it cannot reach, so a rule like that would let a machine that was
  cut off, or removed, promote itself.

### Subscriptions

- **U1 Levels.** Per room, a participant receives everything; or only what is
  addressed to it or marked for everyone; or nothing until it asks.
- **U2 Self-service.** An agent changes its own level at any time.
- **U3 Catching up.** A member that raises its level can read what it skipped,
  within the retention period, separately from its live inbox. A newly
  admitted machine's inbox starts empty; whether it can read earlier history
  was chosen when it was added (T3).
- **U4 Cheap first.** An agent can get, in increasing cost: unread counts per
  room; first lines only; full messages; attachments. Counts and first lines
  mark nothing as read. Summaries are recipes, not core.

### Reachability

- **N1 Machines that cannot accept connections take part fully**, provided
  they can reach at least one member that others can also reach.
- **N2 Several network addresses per machine** (local network, VPN) are
  tried; a machine's identity is its key, not where it is.
- **N3 No discovery or relay service** is required. Overlay networks are the
  user's choice and work unchanged.

### Surfaces

- **A1 Harness-neutral.** The same small set of tools over stdio MCP for
  every harness; a CLI with JSON output; a local socket API for programs.
- **A2 Token budget.** Tool definitions total at most 1,000 tokens (about 936
  today, seven tools); the per-turn notice is one line.
- **A3 Human view and alert.** A local web page where a person can read
  rooms, post, answer questions and confirm requests. A person can be alerted
  that something is waiting without keeping the page open; the core offers a
  way to wait for that, and how the alert is shown is a recipe.
- **A4 Traceable.** For any message: which machines reported a copy, who
  handled it, and the reason when something did not happen.

### Installation and operation

- **I1 AI-first install.** A person tells their agent to install a2p2; the
  agent can do so from the repository's instructions alone, without
  interactive prompts, and verify the result.
- **I2 Reversible.** Uninstall restores every harness file it touched.
- **I3 Portable guidance.** Instructions for agents ship as `AGENTS.md` and a
  portable skill; optional recipes (summaries, role presets, alerts) are
  separate and never installed without being asked for.
- **I4 One small program.** One static binary and one local database per
  machine; a short, pinned, licence-checked dependency list.
- **I5 Safe updates.** Releases are checksummed; the wire protocol and the
  database are versioned, and machines on adjacent versions interoperate.
- **I6 No telemetry, no self-update.**

### Later

- **L1 Attachments.** A message can refer to a file by name, size and hash.
  Any member with the bytes can serve them; each agent decides whether to
  fetch; fetched files are verified and never opened automatically. Sending
  a local file needs a person's confirmation.

## 6. Limits, in plain words

- **Two machines that are never awake together exchange nothing** until one
  of them is, unless a third member was awake in between and took a copy.
  A room that must bridge time zones needs at least one machine that is
  usually on.
- **Away longer than 30 days, you miss what expired.** You are told that
  something is missing, not what it said.
- **Three admins is the number that survives both loss and theft.** With
  three, the other two replace a lost admin and remove a stolen one. With
  two, losing one still leaves an admin who can add and remove machines, but
  the lost key cannot be retired and no new admin can be named. If every
  admin is lost, everyone already in the room keeps talking and nobody can be
  added or removed; to change who is in it, people form a new room.
- **A stolen admin key can add machines and remove members until two other
  admins remove it**, and what those machines read cannot be recalled. It can
  also supply the second signature for a change another admin has started,
  including one started by an admin who has since been removed.
  Every backup of an admin machine holds an admin key.
- **A removed admin's key is not quite dead.** If a current member is willing
  to pass its entries on, it can still remove each member it knew of, once.
  It cannot add anyone.
- **Admins who are out of touch can make changes that collide.** A machine
  added by an admin who was being removed at the same moment may be dropped
  again after it has read the room.
- **An admin machine restored from backup must catch up before it changes
  membership.** If it signs first, its new changes conflict with ones it made
  before the backup was taken. That conflict is itself proof against the
  key, so one other admin can remove it, and the machine rejoins under a new
  key. Of the changes it made on the two sides of the conflict, those the
  removing admin had seen stand and the others do not. A room whose only
  admin does this cannot be settled.
- **While a room has had only one admin, that key is the room.** A thief
  holding it can show a different history to a machine that never received
  the real one. Machines that did receive the real one refuse the other.
- **What a removed machine wrote and nobody fetched is lost**, including a
  withdrawal of one of its own messages.
- **A removed machine's later messages can still appear, marked.** If a
  member that has not yet heard of the removal takes a message from it, that
  message is passed on and shown as from a machine that is no longer a
  member. Messages are discarded outright only for a machine that was never
  validly added.
- **If a machine's permission to read earlier history is narrowed later**,
  text its agents were already shown is dropped without a notice.
- **"May not read earlier history" is kept by the members, not by
  mathematics.** Members refuse to hand earlier messages to such a machine,
  but any member can still quote or forward what it holds.
- **A lost disk loses its copies.** A message survives if any other machine
  still has one. In a small room every member takes a copy as soon as it is
  in touch; the sender is shown which machines have reported one. If a disk
  is lost before any other machine fetched a message, that message is gone.
- **After a restore from backup** agents on that machine may be shown
  messages again, a message already read may be reported as expired, and what
  the machine wrote before the restore can reach others late. Nothing new it
  writes is mistaken for something old.
- **Clocks matter a little.** Where two machines' clocks differ, a message
  can expire on one earlier than on the other, by up to the difference.
- **"Without earlier history" is drawn where the adding machine stood.** A
  message that already existed but had not yet reached the machine that did
  the adding can still be read by the newcomer.
- **A copy report can be out of date.** It says a machine had the message at
  a stated time, and only machines the sender has been in touch with are
  listed.
- **Removal is not instant.** A member that has not yet heard of a removal
  may still serve the removed machine. Nothing already copied can be
  recalled.
- **A machine receiving only its own mail trusts the members it pulls from**
  not to leave messages out. Nobody can forge a message; a member could
  withhold one.
- **The confirmation step is a guard rail, not a lock.** It stops an agent
  being talked into a trust change through its tools. An agent running as
  the same user that sets out to get around it can, because it can do
  anything that user can do at a terminal.
- **A machine restored from a very old backup may have to be invited
  again.** If the copy it was restored to knows none of the room's current
  members, it cannot catch up by itself; a current admin sends it a new
  invite, which hands it the room's record.
- **An admin whose machine cannot reach any other member is warned before
  it changes membership.** The machine cannot tell a normal restart from a
  restore, so it asks the person to say so explicitly; the override is
  recorded.
- **A removed machine is not told by the room.** Its requests are refused
  with an answer that says nothing; its person learns from the invite's
  status, or from the people.
- **A faulty or dishonest machine can sign two different messages in one
  place of its own sequence.** Other machines may then hold different ones
  without noticing, and a copy report for that machine's messages can be
  wrong. Nothing lets it alter anyone else's messages.
- **A question for the person is an ordinary message addressed to them.**
  The local page lists unread messages to the person as questions; there
  is no separate kind.
- **Every member can read everything in the room** that it is allowed to
  fetch, including who handled which message.
- **An agent may see a message again** after a crash or a restore from
  backup. Actions with side effects should use the message id to avoid
  repeating them.

## 7. Scale

The goal is rooms of thousands of machines (the owner's figure: 5,000 to
10,000 people, several agents each). It is reached in stages, and each stage
is entered only when the one before is measured.

| | Stage 1 | Goal |
|---|---|---|
| Machines per room | up to 64 | 5,000 and beyond |
| Rooms per machine | 100 | 1,000 |
| Agents per machine | 50 | 50 |
| Message size | up to 16 KiB | same; larger content as attachments |
| Rate in the largest rooms | n/a | a few per minute |

Before large rooms are attempted, the numbers that would make us change the
design are written down: time for a large room to catch up after an always-on
machine restarts; connections and bytes per machine per new message; the
share of machine pairs with no usable path on real networks.

## 8. Out of scope

Role-based access control; hosted relay or discovery services; model calls
inside the daemon; content filtering by keyword; direct messages outside a
room (a two-member room serves); encryption between members of the same room
(members can already read the room); changing a message after it has been
read; voting or consensus among admins; following a thread automatically,
thread titles, and marking a thread resolved or muted (recipes, or added
when agents show the need); bridging to other agent protocols such as A2A
(possible later as an adapter, not in the core).

## 9. Order of work

0. **Baseline and executable scenarios.** Commit the current tree locally
   (done 2026-11-23). Write the rules as small pure functions in
   `internal/model` and check them with exhaustive small scenarios in
   ordinary Go tests, before the design; no separate specification tool
   unless a rule cannot be settled this way
   (`docs/research/prior-art/07-requirements-and-verification.md`). A rule
   that cannot be made to pass is recorded with its smallest counterexample
   and becomes a stated limit or a changed requirement. The scenarios: A
   posts, B takes a copy, A goes offline, C receives it from B and handles
   it, C goes offline, A learns it was handled through B; then lost
   responses, B restarting, expiry, and a machine restored from an old
   backup. For membership: every counterexample in
   `docs/research/decisions/q10-claude.md` and `q10-sol.md` becomes a named
   scenario (an admin removed while it removes a compromised machine; a
   machine added, and made admin, by an admin being removed; an admin
   restored from backup signing a second branch; a stolen admin key), plus
   a check that machines receiving the same changes in different orders
   agree.
1. **Design v0.3**, containing only rules the scenarios pass, reviewed by
   another model family.
2. **Stage 1: small rooms where every member keeps the whole room.** Signed
   messages, signed member list, catching up from any member,
   acknowledgement, retention, withdraw and replace. Replaces the current
   delivery core outright; nothing has been released, so the old and new are
   not run side by side.
3. **Subscriptions** (U1–U4) and choosing separately what to keep for others.
4. **Large rooms**, after measurement against the numbers in section 7.
5. **Attachments** (L1).

AI-first install (I1–I3), release packaging (I5) and person-confirmed trust
changes (P3) are independent of the delivery work and proceed alongside it.

## 10. Decided, and what still needs the owner

Decided by the owner on 2026-11-23:

| Question | Answer | Where |
|---|---|---|
| May a newly added machine read earlier history? | Either; chosen when the machine is added | T3, U3 |
| Should a room survive losing its admin? | Yes: several admins, and the room keeps working | T9, T10, section 6 |
| May every member machine see who handled a message? | Yes, though it is not a hard requirement | D5 |
| One confirmation by a person for adding or removing a machine or turning networking on? | Yes | P3 |
| Replies | Threads on any message, able to branch | D10 |

Still open. Each line says what happens if the owner says nothing.

| # | Question | If you say nothing |
|---|---|---|
| 1 | The first three machines in a room become its admins unless the person adding them unticks it. An admin can add anyone, who can then read the room | That default stands |
| 2 | The owner suggested the system assign a new admin when one is lost. Two reviewers found no safe way to do that on a timer (T10); several admins from the start is the substitute | T10 stands |
| 3 | Section 6 as a whole: any limit there you do not accept? | Proceed on the text |
| 4 | Retention | 30 days for every room, no setting |

On 2026-11-23 the owner told the lead to proceed autonomously. Done on that
basis: the licence is Apache-2.0 with a signed-off-by line on commits
(`DECISIONS.md` D23); the tree is committed locally, not pushed; step 0 of
section 9 has started. Rows 1 to 4 above stand as written unless the owner
objects. Pushing to a remote and making a release still wait for the owner.

Built on that basis by 2026-11-24 (`DECISIONS.md` D25 to D34): steps 0 and
1, and stage 1 of step 2, with the old delivery core replaced. Five limits
found while building were added to section 6 on that date (a machine
restored from a very old backup; the warning before an out-of-touch admin
changes membership; a removed machine is not told; two messages in one
place; questions for the person). They stand unless the owner objects.
