# Members on a private overlay network

Use this when the machines of a room share a WireGuard or similar private
network (for example Tailscale) and reach each other at its addresses.
a2p2 runs over it unchanged. It needs no overlay and includes none.

Not yet run on a real second machine by the project; the steps follow the
design and the tests with real daemons in containers. Overlay networks used
unchanged are not tried (TRACE, N3).

## Choose how each listener is addressed

Find the machine's overlay address with the overlay's own tool. Then pick
one:

- Listen on that address only: `a2p2 net on 100.64.0.5:7423`. A person
  runs it at a terminal and types `yes`. It listens on that one address and
  advertises that address only, so nothing else is exposed or given out.
- Listen on all addresses and say which to give out. Run `a2p2 net on`, and
  add this to `config.json` in the a2p2 home:

  ```text
  {"advertise": ["100.64.0.5:7423"]}
  ```

  The "advertise" entry replaces the addresses the daemon would detect. It is read
  when the daemon starts, so run `a2p2 stop` and then any a2p2 command to
  restart it. Write each entry as IP:PORT. The listener is open on every
  interface, so close port 7423 on the ones you do not mean to serve.

A machine that cannot accept connections uses `a2p2 net on --outbound-only`
and needs one member the others can reach.

## Check it

1. `a2p2 net check` lists the addresses advertised and, for each member,
   whether it was reached, refused or timed out.
2. Before a room exists, `a2p2 net check HOST:PORT` or
   `a2p2 net check --invite` tests an address.
3. Then invite and join as the README's two-machine steps say.

## Unique-local IPv6

An overlay that gives out `fc00::/7` addresses has them advertised after
the others. A machine tries one only if it holds an address in the same
/48 now, so a peer elsewhere never waits on it (DESIGN section 11).

## What the overlay can see

The overlay's operator can see which machines talk to each other, and when.
It cannot read the messages that a2p2 carries over its pinned TLS, and it
cannot stand in for a member: a2p2 checks each machine's key, not its
address. Every member can still read all of the room.
