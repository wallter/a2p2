// Package bench measures the overhead of a2p2 on one computer: in-process
// micro-benchmarks of the parts of a send, and a load driver that starts
// real daemons from the built binary in throwaway homes and drives them
// over the local socket, through the MCP shim, and over loopback TLS
// between two daemons.
//
// Everything is behind the build tag bench, so make check and go test
// ./... do not run it. Run it with scripts/bench.sh (make bench-local).
// The package imports nothing outside the module's existing dependencies
// and is never imported by the daemon.
package bench
