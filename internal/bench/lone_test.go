//go:build bench

package bench

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// M9: a lone addressed message between two daemons on loopback, the
// reader already waiting in its inbox, each message sent well after the
// last was read (700 ms: the requester's budget is full again): A
// listening and B outbound only, then both listening, each way; then
// ping-pong between long-lived clients, the round trip on A's clock;
// then a lone message as a person times it by hand, a reader process `a2p2 inbox --wait`
// and a sender process `a2p2 send --to`, so process start is in it.
// A2P2_BENCH_LONE_PAUSES (Go durations, comma separated) replaces the
// 700 ms pause: short pauses can exhaust the budget once the initial burst
// is spent, so requests may wait for tokens.
func TestM9LoneMessage(t *testing.T) {
	n := scaled(30, 10)
	pauses := []time.Duration{700 * time.Millisecond}
	if v := os.Getenv("A2P2_BENCH_LONE_PAUSES"); v != "" {
		pauses = nil
		for _, f := range strings.Split(v, ",") {
			d, err := time.ParseDuration(f)
			if err != nil {
				t.Fatal(err)
			}
			pauses = append(pauses, d)
		}
	}
	for _, arr := range []struct{ name, bnet string }{{"A listens, B outbound", "outbound"}, {"both listen", "on"}} {
		a, b := startDaemon(t, "alpha"), startDaemon(t, "beta")
		aAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
		a.setNet("on", aAddr)
		var bAddr string
		if arr.bnet == "on" {
			bAddr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
			b.setNet("on", bAddr)
		} else {
			b.setNet("outbound", "")
		}
		room := call[localapi.Room](t, a.c, "room/create", localapi.RoomReq{Room: "bench"}).ID
		join(t, a, b, room)
		if bAddr != "" {
			// The first invite makes B an admin. A accepts B's invite to
			// the same room so both loopback addresses are trusted.
			join(t, b, a, room)
		}
		aa, bb := a.agent("ann"), b.agent("bea")
		ac, bc := a.client(), b.client()
		ra, rb := startReader(t, ac, aa, room), startReader(t, bc, bb, room)
		// one round each way first: streams begin, connections open
		for _, w := range []struct {
			c     *localapi.Client
			agent string
			r     *reader
		}{{ac, aa, rb}, {bc, bb, ra}} {
			var m localapi.SendResp
			if err := w.c.Call(context.Background(), "send", localapi.SendReq{Agent: w.agent, Room: room, Text: "hello"}, &m); err != nil {
				t.Fatal(err)
			}
			w.r.await(t, m.ID)
		}
		if bAddr != "" {
			awaitInbound(t, a, b, aAddr)
			awaitInbound(t, b, a, bAddr)
		}
		for _, pause := range pauses {
			time.Sleep(3 * time.Second) // the budget full again
			sendToVisible(t, a, ac, aa, room, []string{bb}, rb, n, pause, 2*time.Minute, "M9 lone A->B", arr.name+", pause "+pause.String())
			time.Sleep(3 * time.Second)
			sendToVisible(t, b, bc, bb, room, []string{aa}, ra, n, pause, 2*time.Minute, "M9 lone B->A", arr.name+", pause "+pause.String())
		}
		pingPong(t, ac, bc, aa, bb, room, ra, rb, arr.name)
		ra.close() // the processes below read as ann and bea
		rb.close()
		cliLone(t, a, b, "ann", "bea", "M9 lone A->B CLI", arr.name)
		cliLone(t, b, a, "bea", "ann", "M9 lone B->A CLI", arr.name)
		a.stop()
		b.stop()
		a.home, b.home = "", "" // stopped and removed: nothing for the cleanup
	}
}

func awaitInbound(t *testing.T, listener, dialer *proc, addr string) {
	t.Helper()
	eventually(t, fmt.Sprintf("%s to accept a connection from %s", addr, dialer.node), func() bool {
		st := call[localapi.Status](t, listener.c, "status", localapi.Empty{})
		return slices.ContainsFunc(st.Reach.ConnectedIn, func(c node.ConnectIn) bool {
			return c.Node == dialer.node && c.Local == addr
		})
	})
}

// pingPong is rounds of ann's message to bea and bea's answer, each
// round 1 s after the last (the budget full), timed on A's clock from
// ann's send to her waiting inbox returning the answer. Both clients are
// long-lived; bea answers as soon as her waiting inbox returns the
// question.
func pingPong(t *testing.T, ac, bc *localapi.Client, aa, bb, room string, ra, rb *reader, cs string) {
	t.Helper()
	var l lat
	for i := 0; i < scaled(50, 10); i++ {
		time.Sleep(time.Second)
		t0 := time.Now()
		var q, r localapi.SendResp
		if err := ac.Call(context.Background(), "send", localapi.SendReq{Agent: aa, Room: room, Text: "PING " + strconv.Itoa(i), To: []string{bb}}, &q); err != nil {
			t.Fatal(err)
		}
		rb.await(t, q.ID)
		if err := bc.Call(context.Background(), "send", localapi.SendReq{Agent: bb, Room: room, Text: "PONG " + strconv.Itoa(i), To: []string{aa}}, &r); err != nil {
			t.Fatal(err)
		}
		ra.await(t, r.ID)
		l.add(time.Since(t0))
	}
	result(t, "M9 ping-pong A->B->A", cs+", round trip, 1 s apart", &l, 0, "-")
}

// cliLone times a lone message as a person does by hand: a reader process
// `a2p2 inbox --wait` on r, then a sender process `a2p2 send --to` on s,
// from the send's start to the reader's first line; and the send process
// alone.
func cliLone(t *testing.T, s, r *proc, from, to, what, cs string) {
	t.Helper()
	type inboxResult struct {
		at     time.Time
		output string
		err    error
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var l, sendL lat
	for i := 0; i < scaled(10, 5); i++ {
		cmd := exec.CommandContext(ctx, r.bin, "inbox", "--as", to, "--room", "bench", "--wait", "90", "--limit", "1")
		cmd.Env = env(r.home)
		cmd.Dir = filepath.Join(r.home, "fakehome")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		inbox := make(chan inboxResult, 1)
		go func() {
			br := bufio.NewReader(out)
			line, lineErr := br.ReadString('\n')
			at := time.Now()
			rest, readErr := io.ReadAll(br)
			inbox <- inboxResult{at: at, output: line + string(rest), err: errors.Join(lineErr, readErr, cmd.Wait())}
		}()
		time.Sleep(time.Second) // the reader is waiting
		t0 := time.Now()
		o, err := s.run("send", "--as", from, "--room", "bench", "--to", to+"@"+r.node[:8], "TIMING "+strconv.Itoa(i))
		sendTime := time.Since(t0)
		if err != nil {
			cancel()
			<-inbox
			t.Fatalf("send: %v %s", err, o)
		}
		read := <-inbox
		if read.err != nil {
			t.Fatalf("inbox: %v\nstdout: %s\nstderr: %s", read.err, read.output, &stderr)
		}
		id := strings.TrimSpace(o)
		if id == "" || !strings.Contains(read.output, "\n    id "+id+"\n") || read.at.Before(t0) {
			t.Fatalf("inbox: want entry %q after send started, got %q at %v relative to send", id, read.output, read.at.Sub(t0))
		}
		l.add(read.at.Sub(t0))
		sendL.add(sendTime)
	}
	result(t, what, cs+", send process to inbox line", &l, 0, "-")
	result(t, what, cs+" [send process only]", &sendL, 0, "-")
}
