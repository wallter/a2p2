package web

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// The user at the other end of a page connection, where the system says
// it (Linux, from /proc/net/tcp and tcp6; peeruid_linux.go). A link's code
// is the one secret that gives a session, and on Linux a browser started
// by xdg-open shows it in its arguments to every user. So where the user
// is known, a code is traded only for this daemon's own user (or root).
// Where it is not known (macOS, Windows, a /proc that cannot be read, a
// lookup with no single match), the code alone decides, as before the
// check: the check is defence in depth on Linux, and never locks the
// person out of their own page (D67).

// peerOf is peerUID (peeruid_linux.go, peeruid_other.go); tests replace it.
var peerOf = peerUID

// connTuple is the client's and the server's address and port of r's
// connection, as the server's socket sees them, with IPv4-mapped IPv6
// addresses written as IPv4. Both come from the connection, never from
// anything the client sends.
func connTuple(r *http.Request) (client, server netip.AddrPort, ok bool) {
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if local == nil {
		return client, server, false
	}
	s, err1 := netip.ParseAddrPort(local.String())
	c, err2 := netip.ParseAddrPort(r.RemoteAddr)
	if err1 != nil || err2 != nil {
		return client, server, false
	}
	return unmapAP(c), unmapAP(s), true
}

func unmapAP(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// TCP states in /proc/net/tcp whose lines are not a connected socket of
// a process: TIME_WAIT (no owner, uid 0) and LISTEN.
const (
	tcpTimeWait = 0x06
	tcpListen   = 0x0A
)

var errProcNet = errors.New("unreadable /proc/net/tcp line")

// procNetMatches finds, in the text of /proc/net/tcp or tcp6, the sockets
// whose own address and port are client and whose peer's are server (the
// full four-tuple), and returns the uid that owns each. On a loopback
// connection that is the client's socket: the daemon's own socket has the
// two ends the other way round. order is the byte order the kernel wrote
// the addresses in, the machine's own. Any line that does not parse is an
// error, so the caller treats the lookup as unknown.
func procNetMatches(text string, order binary.ByteOrder, client, server netip.AddrPort) ([]int, error) {
	var uids []int
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] == "sl" {
			continue
		}
		if len(f) < 8 {
			return nil, errProcNet
		}
		local, err1 := procNetAddr(f[1], order)
		remote, err2 := procNetAddr(f[2], order)
		st, err3 := strconv.ParseUint(f[3], 16, 8)
		uid, err4 := strconv.Atoi(f[7])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || uid < 0 {
			return nil, errProcNet
		}
		if st == tcpTimeWait || st == tcpListen || local != client || remote != server {
			continue
		}
		uids = append(uids, uid)
	}
	return uids, nil
}

// procNetAddr reads an address as /proc/net/tcp and tcp6 write it:
// ADDRESS:PORT in hexadecimal, the address as 32-bit words each printed
// as a number in the machine's byte order, the port as a number. An
// IPv4-mapped IPv6 address is returned as IPv4.
func procNetAddr(s string, order binary.ByteOrder) (netip.AddrPort, error) {
	a, p, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, errProcNet
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil || len(p) != 4 {
		return netip.AddrPort{}, errProcNet
	}
	raw, err := hex.DecodeString(a)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, errProcNet
	}
	b := make([]byte, len(raw))
	// raw holds the printed number's digits most significant first, so
	// reading them big-endian gives the number, and writing the number in
	// the machine's order gives the address's bytes as they were in memory.
	for i := 0; i < len(raw); i += 4 {
		order.PutUint32(b[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	addr, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), nil
}

// procNetLookup reads the tables in files (/proc/net/tcp, then tcp6) and
// gives the uid of the client's socket of the connection client to
// server. It is known only when exactly one connected socket in the
// tables has that four-tuple. A table that cannot be read (a missing tcp6
// aside, as when IPv6 is off), a line that does not parse, or more than
// one match make it not known. A table read in several chunks while
// sockets come and go can miss a line, so no match is read again, twice
// at most, and is then not known.
//
// The client's socket lives as long as the connection that carries the
// request, so it is in the table when this runs unless the client closed
// it, and then the answer cannot reach it. /proc/net is the reader's own
// (/proc/self/net), so hidepid does not hide it; a /proc that is not
// mounted or is cut down makes the user not known.
func procNetLookup(files []string, order binary.ByteOrder, client, server netip.AddrPort) (int, bool) {
	for try := 0; try < 3; try++ {
		var uids []int
		for i, f := range files {
			b, err := os.ReadFile(f)
			if i > 0 && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return 0, false
			}
			m, err := procNetMatches(string(b), order, client, server)
			if err != nil {
				return 0, false
			}
			uids = append(uids, m...)
		}
		switch len(uids) {
		case 0:
			continue
		case 1:
			return uids[0], true
		default:
			return 0, false
		}
	}
	return 0, false
}
