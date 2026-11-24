package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/peer"
)

// refuseAll is a peer handler that serves nothing: a probe never gets as
// far as a request.
type refuseAll struct{}

func (refuseAll) Handle(context.Context, string, string, []byte) node.JoinReply {
	return node.JoinReply{}
}
func (refuseAll) Serve(context.Context, string, *node.PullRequest) (*node.PullResponse, error) {
	return nil, errors.New("not served")
}

// cli runs one a2p2 command line and returns what it printed.
func cli(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	err = run(context.Background(), args[0], args[1:])
	os.Stdout = old
	w.Close()
	return <-done, err
}

// net check --invite and HOST:PORT, through the command's own parsing of
// the arguments and of the invite token, against an a2p2 listener on
// loopback: the invite's key is reached, another key in the invite is a
// mismatch, a bad token is refused before anything is dialled.
func TestNetCheckOfAnInviteOrAnAddressGoesThroughTheCommandsOwnParsing(t *testing.T) {
	t.Setenv("A2P2_HOME", t.TempDir()) // never the real home; these forms use no daemon
	id, err := peer.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := peer.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- peer.Serve(ctx, ln, id, refuseAll{}) }()
	defer func() { cancel(); <-served }()
	addr := ln.Addr().String()
	token := func(key *peer.Identity) string {
		fp, ok := core.KeyFingerprint(key.NodeID())
		if !ok {
			t.Fatal("fingerprint")
		}
		s, err := core.EncodeToken(core.Token{Secret: [core.TokenSecretBytes]byte{1}, Fingerprint: fp, Addrs: []string{addr}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	check := func(args ...string) addrCheckOut {
		t.Helper()
		out, err := cli(t, append([]string{"net", "check", "--json"}, args...)...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var got addrCheckOut
		if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Addrs) != 1 {
			t.Fatalf("%v: %q %v", args, out, err)
		}
		return got
	}
	if got := check("--invite", token(id)); !got.Invite || got.Addrs[0].Result != node.ProbeReached || got.Addrs[0].Node != id.NodeID() {
		t.Fatalf("the invite's key: %+v", got)
	}
	if got := check("--invite", token(other)); got.Addrs[0].Result != node.ProbeWrongKey {
		t.Fatalf("another key: %+v", got)
	}
	if got := check(addr); got.Addrs[0].Result != node.ProbeReached || got.Addrs[0].Node != id.NodeID() {
		t.Fatalf("HOST:PORT: %+v", got)
	}
	if _, err := cli(t, "net", "check", "--invite", "a2p2-not-a-token"); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("a bad token: %v", err)
	}
}
