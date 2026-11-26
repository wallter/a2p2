//go:build bench

package bench

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// TestProbePlan prints modernc SQLite's query plans, on a copy of a
// store, for the two queries the profiles show scanning a room.
func TestProbePlan(t *testing.T) {
	path := os.Getenv("PROBE_DB")
	if path == "" {
		t.Skip("PROBE_DB not set")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`SELECT * FROM records WHERE room = 'x' AND effect = 'u' AND (ref = 'y' OR parent = 'y')`,
		`SELECT * FROM records WHERE room = 'x' AND ref = 'y' ORDER BY author, epoch, seq`,
	} {
		rows, err := db.Query("EXPLAIN QUERY PLAN " + q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, notused int
			var detail string
			rows.Scan(&id, &parent, &notused, &detail)
			t.Logf("%s\n    -> %s", q, detail)
		}
		rows.Close()
	}
	var v string
	db.QueryRow(`SELECT sqlite_version()`).Scan(&v)
	t.Logf("sqlite_version %s", v)
}

// TestProbeAB sends from A to B one at a time, B finding each by an inbox
// without wait every millisecond (no wake involved), to tell the delivery's
// own delay from the waiting inbox's.
func TestProbeAB(t *testing.T) {
	if os.Getenv("PROBE_AB") == "" {
		t.Skip("PROBE_AB not set")
	}
	a, b := startDaemon(t, "alpha"), startDaemon(t, "beta")
	a.setNet("on", fmt.Sprintf("127.0.0.1:%d", freePort(t)))
	b.setNet("outbound", "")
	room := call[localapi.Room](t, a.c, "room/create", localapi.RoomReq{Room: "bench"}).ID
	join(t, a, b, room)
	aa, bb := a.agent("ann"), b.agent("bea")
	ac, bc := a.client(), b.client()
	var l lat
	for i := 0; i < 200; i++ {
		time.Sleep(5 * time.Millisecond)
		t0 := time.Now()
		var m localapi.SendResp
		if err := ac.Call(context.Background(), "send", localapi.SendReq{Agent: aa, Room: room, Text: text(200)}, &m); err != nil {
			t.Fatal(err)
		}
		for found := false; !found; {
			var in localapi.InboxResp
			if err := bc.Call(context.Background(), "inbox", localapi.InboxReq{Agent: bb, Room: room}, &in); err != nil {
				t.Fatal(err)
			}
			for _, e := range in.Entries {
				found = found || e.ID == m.ID
			}
			_ = seen(bc, bb, in.Entries)
			if !found {
				time.Sleep(time.Millisecond)
			}
			if time.Since(t0) > 30*time.Second {
				t.Fatal("not delivered in 30 s")
			}
		}
		l.add(time.Since(t0))
	}
	result(t, "probe A->B polled", "inbox no wait every 1 ms", &l, 0, "-")
}

// TestProbeHistory sends PROBE_H messages in process, logging the mean
// send time per thousand, then times one empty inbox and one backlog page.
func TestProbeHistory(t *testing.T) {
	h, _ := strconv.Atoi(os.Getenv("PROBE_H"))
	if h == 0 {
		t.Skip("PROBE_H not set")
	}
	x := newInproc(t)
	ctx := context.Background()
	backlog, _ := x.surf.Register(ctx, "bench", "backlog", "/w")
	in := node.SendInput{Agent: x.agent, Text: text(200)}
	t0 := time.Now()
	for i := 0; i < h; i++ {
		if _, err := x.surf.Send(ctx, in); err != nil {
			t.Fatal(err)
		}
		if i%1000 == 999 {
			t.Logf("sent %d, mean of the last 1000: %v", i+1, time.Since(t0)/1000)
			t0 = time.Now()
		}
	}
	r, _ := x.surf.Register(ctx, "bench", "reader", "/w")
	t0 = time.Now()
	v, err := x.surf.Inbox(ctx, r.ID, "", 0, 0)
	t.Logf("empty inbox: %v (%d entries, %v)", time.Since(t0), len(v.Entries), err)
	t0 = time.Now()
	v, err = x.surf.Inbox(ctx, backlog.ID, "", 0, 0)
	t.Logf("backlog inbox: %v (%d entries, %v)", time.Since(t0), len(v.Entries), err)
}

// TestProbeStall sends from B (outbound only) to A one message at a time;
// if one is not visible on A within 20 s it dumps A's goroutines (SIGQUIT
// to A's own pid, which ends that daemon) into A's log.
func TestProbeStall(t *testing.T) {
	if os.Getenv("PROBE_STALL") == "" {
		t.Skip("PROBE_STALL not set")
	}
	a, b := startDaemon(t, "alpha"), startDaemon(t, "beta")
	a.setNet("on", fmt.Sprintf("127.0.0.1:%d", freePort(t)))
	b.setNet("outbound", "")
	room := call[localapi.Room](t, a.c, "room/create", localapi.RoomReq{Room: "bench"}).ID
	join(t, a, b, room)
	aa, bb := a.agent("ann"), b.agent("bea")
	ra := startReader(t, a.client(), aa, room)
	defer ra.close()
	bc := b.client()
	for i := 0; i < 500; i++ {
		time.Sleep(5 * time.Millisecond)
		var m localapi.SendResp
		if err := bc.Call(context.Background(), "send", localapi.SendReq{Agent: bb, Room: room, Text: text(200)}, &m); err != nil {
			t.Fatal(err)
		}
		select {
		case <-awaitCh(ra, m.ID):
		case <-time.After(20 * time.Second):
			for _, p := range []*proc{a, b} {
				st := call[localapi.Status](t, p.c, "status", localapi.Empty{})
				t.Logf("message %d not visible on A after 20 s; SIGQUIT to %s (pid %d)", i, st.Name, st.PID)
				syscall.Kill(st.PID, syscall.SIGQUIT)
				time.Sleep(2 * time.Second)
				data, _ := os.ReadFile(filepath.Join(p.home, "daemon.log"))
				os.WriteFile(os.Getenv("PROBE_STALL")+"."+st.Name, data, 0o600)
			}
			return
		}
	}
	t.Log("no stall in 500 sends")
}

func awaitCh(r *reader, id string) <-chan struct{} {
	c := make(chan struct{})
	go func() {
		for v := range r.got {
			if v.id == id {
				close(c)
				return
			}
		}
	}()
	return c
}
