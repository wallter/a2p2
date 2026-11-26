//go:build bench

// Command a2p2-netd is the a2p2 daemon of the network benchmark
// (scripts/bench-net): exactly `a2p2 daemon` (daemon.Run on $A2P2_HOME),
// except that its JSON log is at level Debug, so that every request a
// listener serves is logged with its route and status ("peer request",
// internal/peer/transport.go). The benchmark counts pull requests and 429
// answers from those lines; the product daemon logs them only at Debug.
// Message bodies are never logged at any level. Built only with the bench
// tag; never part of a release.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/wallter/a2p2/internal/daemon"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err := daemon.Run(ctx, daemon.Home(), "bench-netd"); err != nil {
		fmt.Fprintln(os.Stderr, "a2p2-netd:", err)
		os.Exit(1)
	}
}
