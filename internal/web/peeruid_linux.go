package web

import (
	"encoding/binary"
	"net/http"
)

// peerUID is the uid of the process at the other end of r's connection,
// from /proc/net/tcp and tcp6 (procNetLookup in peeruid.go), or not known.
func peerUID(r *http.Request) (int, bool) {
	client, server, ok := connTuple(r)
	if !ok {
		return 0, false
	}
	return procNetLookup([]string{"/proc/net/tcp", "/proc/net/tcp6"}, binary.NativeEndian, client, server)
}
