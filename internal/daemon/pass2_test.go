package daemon

// Tests of the second surfaces pass: a networking decision is applied by
// the daemon itself, a bind failure leaves a working daemon that says so,
// and the page binds only to a literal loopback address.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// freePort returns a loopback address nothing listens on now.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func accepts(addr string) bool {
	c, err := net.DialTimeout("tcp4", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// The net call records the decision and the daemon applies it at once,
// with real binds: nobody calls stop or starts the daemon again.
func TestANetworkingDecisionIsAppliedByTheDaemonItself(t *testing.T) {
	m := newMachineOpts(t, "solo", nil, true)
	addr := freePort(t)
	pid := call[localapi.Status](m, "status", localapi.Empty{}).PID
	st := m.decide(localapi.NetReq{State: node.NetworkOn, Listen: addr})
	if st.Network.Applied != "" || len(st.Listen) != 1 || st.Listen[0] != addr {
		t.Fatalf("on: %+v %v", st.Network, st.Listen)
	}
	eventually(t, "the listener to accept", func() bool { return accepts(addr) })
	st = m.decide(localapi.NetReq{State: node.NetworkOff})
	if len(st.Listen) != 0 || st.PID != pid {
		t.Fatalf("off: listen %v pid %d (was %d)", st.Listen, st.PID, pid)
	}
	if accepts(addr) {
		t.Fatal("networking off, yet the address still accepts connections")
	}
	if m.done == nil {
		t.Fatal("the daemon stopped")
	}
}

// An address that cannot be bound: the daemon keeps serving its socket
// and page, status says "on" with the applied state and the reason, an
// alert says so, and net off (or a new net on) still works.
func TestAnAddressThatCannotBeBoundLeavesAWorkingDaemonThatSaysSo(t *testing.T) {
	m := newMachineOpts(t, "solo", nil, true)
	busy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	st := m.decide(localapi.NetReq{State: node.NetworkOn, Listen: busy.Addr().String()})
	if st.Network.Applied != node.NetworkOutbound || !strings.HasPrefix(st.Network.Reason, "not listening: ") || len(st.Listen) != 0 {
		t.Fatalf("on, port taken: %+v %v", st.Network, st.Listen)
	}
	as := call[localapi.AlertsResp](m, "alerts", localapi.WaitReq{})
	if len(as.Alerts) != 1 || as.Alerts[0].Kind != node.AlertNetwork || !strings.Contains(as.Alerts[0].Text, "not listening") {
		t.Fatalf("alerts: %+v", as)
	}
	var page struct {
		Status localapi.Status `json:"status"`
	}
	if code := m.ui("GET", "/api/state", nil, &page); code != 200 || page.Status.Network.Reason != st.Network.Reason {
		t.Fatalf("page: %d %+v", code, page.Status.Network)
	}
	if st := m.decide(localapi.NetReq{State: node.NetworkOff}); st.Network.Applied != "" || st.Network.Reason != "" {
		t.Fatalf("off after a failed bind: %+v", st.Network)
	}
	addr := busy.Addr().String()
	busy.Close()
	if st := m.decide(localapi.NetReq{State: node.NetworkOn, Listen: addr}); st.Network.Applied != "" {
		t.Fatalf("on once the port is free: %+v", st.Network)
	}
	eventually(t, "the listener to accept", func() bool { return accepts(addr) })
	// And a daemon that starts with the address taken starts all the same.
	m.stop()
	again, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	m.start()
	if st := call[localapi.Status](m, "status", localapi.Empty{}); st.Network.State != node.NetworkOn || st.Network.Applied != node.NetworkOutbound {
		t.Fatalf("started with the port taken: %+v", st.Network)
	}
}

func TestThePageListensOnlyOnALiteralLoopbackAddress(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:7422": true, "[::1]:0": true, "127.0.0.1:0": true,
		"0.0.0.0:7422": false, "localhost:7422": false, "example.com:7422": false, "[::]:7422": false,
		"10.0.0.1:7422": false, "127.0.0.1": false, "": false,
	} {
		if err := CheckUIAddr(addr); (err == nil) != ok {
			t.Errorf("%q: %v", addr, err)
		}
	}
	// The daemon refuses before it binds anything: the address stays free.
	addr := freePort(t)
	_, port, _ := net.SplitHostPort(addr)
	home, err := os.MkdirTemp(sockTemp(), "a2p2")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	if err := SaveConfig(home, Config{Name: "solo", UI: "0.0.0.0:" + port}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = RunWith(ctx, home, "test", RunOpts{})
	if err == nil || !strings.Contains(err.Error(), "loopback IP address") {
		t.Fatalf("ui on 0.0.0.0: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "a2p2.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the daemon got as far as its database: %v", err)
	}
}
