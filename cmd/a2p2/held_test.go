package main

import (
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
)

// D71: status and doctor say how many of a machine's addresses are held
// and not dialled, and why, every string escaped for the terminal.
func TestStatusSaysHowManyAddressesAreHeldAndNotDialledAndWhy(t *testing.T) {
	if heldText(nil) != "" {
		t.Fatal("a line with nothing held")
	}
	got := heldText([]node.HeldAddr{
		{Addr: "127.0.0.1:6379", Why: node.AddrRefusal("127.0.0.1:6379")},
		{Addr: "evil\x1b[2J.example:22", Why: node.AddrRefusal("10.0.0.1:22")},
	})
	if !strings.HasPrefix(got, "2 addresses are held and not dialled") || !strings.Contains(got, "loopback") ||
		!strings.Contains(got, "port below 1024") || strings.Contains(got, "\x1b") {
		t.Fatalf("%q", got)
	}
	if one := heldText([]node.HeldAddr{{Addr: "127.0.0.1:1", Why: "a loopback address"}}); !strings.HasPrefix(one, "1 address is") {
		t.Fatalf("%q", one)
	}
}
