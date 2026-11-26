//go:build bench

// Command a2p2-netload is the load driver of the network benchmark
// (scripts/bench-net). It runs in a container of its own with no network,
// and reaches each daemon through the daemon's local socket in that
// daemon's home volume, mounted read-write at /homes/<node>. One process
// therefore sees every machine with one clock: a message's latency is
// from the start of its send call on one machine to the inbox call that
// returned it on another, both timed here.
//
//	a2p2-netload setup  < spec   form one room of every node (net, invites, joins)
//	a2p2-netload run    < spec   send and read one case; prints the result
//	a2p2-netload file   < spec   send one file, approve it, fetch it on the other side
//	a2p2-netload drain  < spec   read the inboxes until nothing new arrives
//
// Each spec is one JSON object on stdin and each answer one JSON object on
// stdout. It uses only the local API (internal/localapi), as the CLI and
// the MCP shim do; approving a join or a send with files is the API's
// confirm call, which is what `a2p2 confirm` makes after a person says yes.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wallter/a2p2/internal/daemon"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: a2p2-netload setup|run|file|drain < spec.json")
		os.Exit(2)
	}
	var out any
	var err error
	switch os.Args[1] {
	case "setup":
		out, err = setup()
	case "run":
		out, err = runCase()
	case "file":
		out, err = sendFile()
	case "drain":
		out, err = drain()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "a2p2-netload:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(out)
}

func readSpec(v any) error {
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

var bg = context.Background()

// Node is one machine as the driver reaches it.
type Node struct {
	Name   string `json:"name"`
	Home   string `json:"home"`             // its home volume, here
	Listen string `json:"listen,omitempty"` // empty: outbound only
	ID     string `json:"node,omitempty"`
	Agent  string `json:"agent,omitempty"`
	c      *localapi.Client
}

func (n *Node) client() *localapi.Client {
	if n.c == nil {
		n.c = localapi.NewClient(daemon.SockPath(n.Home))
	}
	return n.c
}

// fresh is a new client (its own connections), one per reader or sender.
func (n *Node) fresh() *localapi.Client { return localapi.NewClient(daemon.SockPath(n.Home)) }

func call[T any](c *localapi.Client, name string, req any) (T, error) {
	var out T
	err := c.Call(bg, name, req, &out)
	if err != nil {
		err = fmt.Errorf("%s: %w", name, err)
	}
	return out, err
}

func until(what string, wait time.Duration, cond func() (bool, error)) error {
	deadline := time.Now().Add(wait)
	for {
		ok, err := cond()
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("timed out waiting for %s: %w", what, err)
			}
			return fmt.Errorf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---- setup: one room, every node a member ----

type SetupSpec struct {
	Room  string  `json:"room"`
	Nodes []*Node `json:"nodes"` // the first listens and is the room's only admin
}

type SetupOut struct {
	Room   string             `json:"room"`
	Nodes  map[string]*Node   `json:"nodes"`
	Joins  map[string]float64 `json:"join_secs"`
	Errors []string           `json:"errors,omitempty"`
}

func setup() (any, error) {
	var sp SetupSpec
	if err := readSpec(&sp); err != nil {
		return nil, err
	}
	if len(sp.Nodes) < 2 || sp.Nodes[0].Listen == "" {
		return nil, errors.New("setup needs two nodes or more, the first listening")
	}
	out := SetupOut{Nodes: map[string]*Node{}, Joins: map[string]float64{}}
	for _, n := range sp.Nodes {
		c := n.client()
		if err := until("the daemon of "+n.Name, 60*time.Second, func() (bool, error) {
			st, err := call[localapi.Status](c, "status", localapi.Empty{})
			n.ID = st.Node
			return err == nil, err
		}); err != nil {
			return nil, err
		}
		state := "on"
		if n.Listen == "" {
			state = "outbound"
		}
		res, err := call[localapi.NetResp](c, "net", localapi.NetReq{State: state, Listen: n.Listen})
		if err != nil {
			return nil, err
		}
		if err := until("net "+state+" on "+n.Name, 60*time.Second, func() (bool, error) {
			st, err := call[localapi.Status](c, "status", localapi.Empty{})
			return err == nil && st.Network.Request == res.Request && st.Network.Reason != "being applied", err
		}); err != nil {
			return nil, err
		}
		out.Nodes[n.Name] = n
	}
	admin := sp.Nodes[0]
	room, err := call[localapi.Room](admin.client(), "room/create", localapi.RoomReq{Room: sp.Room})
	if err != nil {
		return nil, err
	}
	out.Room = room.ID
	for _, j := range sp.Nodes[1:] {
		t0 := time.Now()
		if err := join(admin, j, room.ID); err != nil {
			return nil, fmt.Errorf("join %s: %w", j.Name, err)
		}
		out.Joins[j.Name] = time.Since(t0).Seconds()
	}
	// Every node sees every member, then each registers its agent.
	for _, n := range sp.Nodes {
		if err := until("every member on "+n.Name, 120*time.Second, func() (bool, error) {
			rl, err := call[localapi.RoomsResp](n.client(), "room/list", localapi.RoomListReq{Room: room.ID})
			return err == nil && len(rl.Rooms) == 1 && len(rl.Rooms[0].Members) == len(sp.Nodes), err
		}); err != nil {
			return nil, err
		}
		a, err := call[store.Agent](n.client(), "register", localapi.RegisterReq{Harness: "bench", Label: "bench-" + n.Name, Cwd: "/w/" + n.Name})
		if err != nil {
			return nil, err
		}
		n.Agent = a.ID
	}
	return out, nil
}

// join takes j into room through admin, as internal/bench's join helper
// does: invite, preview, accept on j, then the admin's confirm.
func join(admin, j *Node, room string) error {
	tok, err := call[localapi.InviteResp](admin.client(), "room/invite", localapi.InviteReq{Room: room, Role: node.RoleMember})
	if err != nil {
		return err
	}
	pv, err := call[localapi.PreviewResp](j.client(), "room/join", localapi.JoinReq{Token: tok.Token})
	if err != nil {
		return err
	}
	if pv.State != node.JoinStatePreviewed {
		return fmt.Errorf("preview: %+v", pv)
	}
	acc, err := call[localapi.AcceptResp](j.client(), "room/join/accept", localapi.AcceptReq{Token: tok.Token, Preview: pv.Preview})
	if err != nil {
		return err
	}
	if acc.State != node.JoinStateAccepted {
		return fmt.Errorf("accept: %+v", acc)
	}
	tries := 0
	return until("the admin to admit "+j.Name, 180*time.Second, func() (bool, error) {
		rl, err := call[localapi.RoomsResp](j.client(), "room/list", localapi.RoomListReq{Room: room})
		if err == nil && len(rl.Rooms) == 1 && slices.ContainsFunc(rl.Rooms[0].Members, func(m localapi.Member) bool { return m.Node == j.ID }) {
			return true, nil
		}
		qs, err := call[localapi.RequestsResp](admin.client(), "requests", localapi.Empty{})
		if err != nil {
			return false, err
		}
		for _, q := range qs.Requests {
			if q.Node != j.ID || q.Room != room {
				continue
			}
			tries++
			// As a person who waits, then accepts the risk (AGENTS.md).
			cf, err := call[localapi.ConfirmResp](admin.client(), "confirm", localapi.ConfirmReq{ID: q.ID, Approve: true, NotCaughtUpOK: tries > 3})
			if err != nil {
				return false, err
			}
			if cf.State == store.RequestApproved {
				time.Sleep(200 * time.Millisecond)
			}
		}
		time.Sleep(300 * time.Millisecond)
		return false, nil
	})
}

// ---- reading ----

// arrival is one benchmark message seen in a receiver's inbox.
type arrival struct {
	tag, from string
	seq       int
	at        time.Time
	id        string
	files     []localapi.EntryFile
}

// parse reads "nb <tag> <from> <seq> ..." from a message's text.
func parse(text string) (tag, from string, seq int, ok bool) {
	f := strings.SplitN(text, " ", 5)
	if len(f) < 4 || f[0] != "nb" {
		return "", "", 0, false
	}
	seq, err := strconv.Atoi(f[3])
	return f[1], f[2], seq, err == nil
}

// reader reads one node's inbox in a loop, confirming what it was shown,
// and hands every benchmark message to got.
type reader struct {
	n      *Node
	c      *localapi.Client
	room   string
	got    func(arrival)
	stop   chan struct{}
	done   chan struct{}
	errs   atomic.Int64
	reads  atomic.Int64
	others atomic.Int64 // entries that are not this case's messages
}

func startReader(n *Node, room string, got func(arrival)) *reader {
	r := &reader{n: n, c: n.fresh(), room: room, got: got, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for {
			select {
			case <-r.stop:
				return
			default:
			}
			var in localapi.InboxResp
			err := r.c.Call(bg, "inbox", localapi.InboxReq{Agent: n.Agent, Room: room, Limit: 500, WaitMS: 1000}, &in)
			now := time.Now()
			r.reads.Add(1)
			if err != nil {
				r.errs.Add(1)
				time.Sleep(200 * time.Millisecond)
				continue
			}
			for _, e := range in.Entries {
				if e.Kind != "message" {
					r.others.Add(1)
					continue
				}
				tag, from, seq, ok := parse(e.Text)
				if !ok {
					r.others.Add(1)
					continue
				}
				r.got(arrival{tag: tag, from: from, seq: seq, at: now, id: e.ID, files: e.Files})
			}
			if len(in.Entries) > 0 {
				req := localapi.SeenReq{Agent: n.Agent}
				for _, e := range in.Entries {
					req.Entries = append(req.Entries, localapi.SeenEntry{Key: e.Key, Version: e.Version})
				}
				var res localapi.SeenResp
				if err := r.c.Call(bg, "seen", req, &res); err != nil {
					r.errs.Add(1)
				}
			}
		}
	}()
	return r
}

func (r *reader) close() { close(r.stop); <-r.done }

// ---- run: one case ----

type RunSpec struct {
	Room      string           `json:"room"`
	Tag       string           `json:"tag"`
	Nodes     map[string]*Node `json:"nodes"`
	Senders   []string         `json:"senders"`
	Receivers []string         `json:"receivers"`
	Mode      string           `json:"mode"` // serial, burst or rate
	Count     int              `json:"count"`
	Rate      float64          `json:"rate"`
	Duration  float64          `json:"duration"`
	Size      int              `json:"size"`
	Timeout   float64          `json:"timeout"` // serial, burst: the whole case; rate: draining after the sending
	Timeline  bool             `json:"timeline"`
}

type Lat struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
	Max float64 `json:"max_ms"`
}

func latOf(d []time.Duration) Lat {
	if len(d) == 0 {
		return Lat{}
	}
	slices.Sort(d)
	p := func(q float64) float64 {
		i := int(q*float64(len(d))+0.5) - 1
		return float64(d[max(0, min(i, len(d)-1))].Microseconds()) / 1000
	}
	return Lat{len(d), p(.50), p(.95), p(.99), p(1)}
}

type Tick struct {
	T         float64 `json:"t"`
	Sent      int     `json:"sent"`
	Delivered int     `json:"delivered"` // deliveries (message x receiver) so far
}

type RunOut struct {
	Tag        string  `json:"tag"`
	Mode       string  `json:"mode"`
	Size       int     `json:"size"` // text bytes actually sent
	Sent       int     `json:"sent"`
	SendErrors int     `json:"send_errors"`
	SendSecs   float64 `json:"send_secs"`
	SendCall   Lat     `json:"send_call"`
	SendLagMax float64 `json:"send_lag_max_ms,omitempty"` // rate: how far the sender fell behind its schedule
	Expected   int     `json:"expected"`                  // deliveries expected (message x receiver)
	Delivered  int     `json:"delivered"`
	Complete   bool    `json:"complete"`
	TimedOut   bool    `json:"timed_out"`
	// Secs: from the first send's start to the last delivery (or to the
	// end, when not complete).
	Secs float64 `json:"secs"`
	// Rate: deliveries a second per receiver over secs; for rate mode
	// RateWindow is the same over the sending window only.
	Rate       float64        `json:"msgs_per_s"`
	RateWindow float64        `json:"msgs_per_s_window,omitempty"`
	BacklogEnd int            `json:"backlog_at_send_end,omitempty"` // deliveries outstanding when the sending stopped
	BacklogMax int            `json:"backlog_max,omitempty"`
	Latency    Lat            `json:"latency"`
	PerRecv    map[string]int `json:"per_receiver"`
	ReadErrors int64          `json:"read_errors"`
	Timeline   []Tick         `json:"timeline,omitempty"`
}

// body is the message text: "nb tag from seq " padded to size bytes.
func body(tag, from string, seq, size int) string {
	s := fmt.Sprintf("nb %s %s %d ", tag, from, seq)
	if len(s) >= size {
		return s
	}
	const pad = "the quick brown fox jumps over the lazy dog 0123456789 "
	return s + strings.Repeat(pad, (size-len(s))/len(pad)+1)[:size-len(s)]
}

func runCase() (any, error) {
	var sp RunSpec
	if err := readSpec(&sp); err != nil {
		return nil, err
	}
	if sp.Size <= 0 {
		sp.Size = 200
	}
	type key struct {
		from string
		seq  int
	}
	var (
		mu        sync.Mutex
		starts    = map[key]time.Time{}
		seen      = map[string]map[key]bool{}
		lats      []time.Duration
		delivered int
		perRecv   = map[string]int{}
		lastAt    time.Time
		notify    = make(chan struct{}, 1)
	)
	expectFor := func(from string) int { // receivers of a sender's message
		n := 0
		for _, r := range sp.Receivers {
			if r != from {
				n++
			}
		}
		return n
	}
	sentTotal := atomic.Int64{}
	expected := atomic.Int64{}
	got := func(recv string) func(arrival) {
		return func(a arrival) {
			if a.tag != sp.Tag || a.from == recv {
				return
			}
			k := key{a.from, a.seq}
			mu.Lock()
			defer mu.Unlock()
			if seen[recv][k] {
				return
			}
			seen[recv][k] = true
			if t0, ok := starts[k]; ok {
				lats = append(lats, a.at.Sub(t0))
			}
			delivered++
			perRecv[recv]++
			if a.at.After(lastAt) {
				lastAt = a.at
			}
			select {
			case notify <- struct{}{}:
			default:
			}
		}
	}
	var readers []*reader
	for _, r := range sp.Receivers {
		seen[r] = map[key]bool{}
		perRecv[r] = 0
		readers = append(readers, startReader(sp.Nodes[r], sp.Room, got(r)))
	}
	defer func() {
		for _, r := range readers {
			r.close()
		}
	}()
	time.Sleep(300 * time.Millisecond) // the readers are waiting

	size := sp.Size
	var sendErrs atomic.Int64
	var sendMu sync.Mutex
	var sendLat []time.Duration
	var lagMax time.Duration
	// sendOne sends message seq from s, retrying a refusal that may pass
	// later; a body over the limit is shortened once, for every sender.
	sendOne := func(c *localapi.Client, s *Node, seq int) bool {
		for try := 0; try < 50; try++ {
			k := key{s.Name, seq}
			t0 := time.Now()
			mu.Lock()
			starts[k] = t0
			sz := size
			mu.Unlock()
			var m localapi.SendResp
			err := c.Call(bg, "send", localapi.SendReq{Agent: s.Agent, Room: sp.Room, Text: body(sp.Tag, s.Name, seq, sz)}, &m)
			if err == nil {
				d := time.Since(t0)
				sendMu.Lock()
				sendLat = append(sendLat, d)
				sendMu.Unlock()
				sentTotal.Add(1)
				expected.Add(int64(expectFor(s.Name)))
				return true
			}
			sendErrs.Add(1)
			var e *localapi.Error
			if errors.As(err, &e) && e.Code == node.ErrorTooLarge {
				mu.Lock()
				if size == sz {
					size = sz - 128
				}
				mu.Unlock()
				continue
			}
			if errors.As(err, &e) && !e.Retryable {
				fmt.Fprintln(os.Stderr, "send:", err)
				return false
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
	}
	allIn := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return int64(delivered) >= expected.Load()
	}
	waitAll := func(deadline time.Time) bool {
		for !allIn() {
			left := time.Until(deadline)
			if left <= 0 {
				return false
			}
			select {
			case <-notify:
			case <-time.After(min(left, 200*time.Millisecond)):
			}
		}
		return true
	}

	start := time.Now()
	var ticks []Tick
	tickStop := make(chan struct{})
	tickDone := make(chan struct{})
	backlogMax := 0
	go func() {
		defer close(tickDone)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-tickStop:
				return
			case now := <-t.C:
				mu.Lock()
				d := delivered
				mu.Unlock()
				s := int(sentTotal.Load())
				if b := int(expected.Load()) - d; b > backlogMax {
					backlogMax = b
				}
				if sp.Timeline {
					ticks = append(ticks, Tick{T: now.Sub(start).Seconds(), Sent: s, Delivered: d})
				}
			}
		}
	}()

	timedOut := false
	var sendEnd time.Time
	var windowDelivered int
	switch sp.Mode {
	case "serial":
		deadline := start.Add(time.Duration(sp.Timeout * float64(time.Second)))
		for _, sn := range sp.Senders {
			s := sp.Nodes[sn]
			c := s.fresh()
			for i := 1; i <= sp.Count && !timedOut; i++ {
				if !sendOne(c, s, i) {
					continue
				}
				if !waitAll(deadline) {
					timedOut = true
				}
			}
		}
		sendEnd = time.Now()
	case "burst":
		deadline := start.Add(time.Duration(sp.Timeout * float64(time.Second)))
		var wg sync.WaitGroup
		for _, sn := range sp.Senders {
			wg.Add(1)
			go func(s *Node) {
				defer wg.Done()
				c := s.fresh()
				for i := 1; i <= sp.Count && time.Now().Before(deadline); i++ {
					sendOne(c, s, i)
				}
			}(sp.Nodes[sn])
		}
		wg.Wait()
		sendEnd = time.Now()
		timedOut = !waitAll(deadline)
	case "rate":
		end := start.Add(time.Duration(sp.Duration * float64(time.Second)))
		var wg sync.WaitGroup
		for _, sn := range sp.Senders {
			wg.Add(1)
			go func(s *Node) {
				defer wg.Done()
				c := s.fresh()
				for i := 1; ; i++ {
					at := start.Add(time.Duration(float64(i-1) / sp.Rate * float64(time.Second)))
					if !at.Before(end) {
						return
					}
					if d := time.Until(at); d > 0 {
						time.Sleep(d)
					} else if -d > 0 {
						sendMu.Lock()
						lagMax = max(lagMax, -d)
						sendMu.Unlock()
					}
					sendOne(c, s, i)
				}
			}(sp.Nodes[sn])
		}
		wg.Wait()
		sendEnd = time.Now()
		mu.Lock()
		windowDelivered = delivered
		mu.Unlock()
		timedOut = !waitAll(sendEnd.Add(time.Duration(sp.Timeout * float64(time.Second))))
	default:
		return nil, fmt.Errorf("unknown mode %q", sp.Mode)
	}
	close(tickStop)
	<-tickDone
	finish := time.Now()

	mu.Lock()
	defer mu.Unlock()
	out := RunOut{Tag: sp.Tag, Mode: sp.Mode, Size: size, Sent: int(sentTotal.Load()), SendErrors: int(sendErrs.Load()),
		SendSecs: sendEnd.Sub(start).Seconds(), SendCall: latOf(sendLat), Expected: int(expected.Load()),
		Delivered: delivered, TimedOut: timedOut, PerRecv: perRecv, Latency: latOf(lats), Timeline: ticks,
		BacklogMax: backlogMax}
	out.Complete = delivered >= out.Expected && !timedOut
	if lagMax > 0 {
		out.SendLagMax = float64(lagMax.Microseconds()) / 1000
	}
	end := lastAt
	if !out.Complete || end.IsZero() {
		end = finish
	}
	out.Secs = end.Sub(start).Seconds()
	// Messages a second as one receiver sees them: deliveries divided by
	// the receivers each message has.
	perMsg := 1.0
	if out.Sent > 0 && out.Expected > 0 {
		perMsg = float64(out.Expected) / float64(out.Sent)
	}
	if out.Secs > 0 {
		out.Rate = float64(delivered) / perMsg / out.Secs
	}
	if sp.Mode == "rate" {
		if w := sendEnd.Sub(start).Seconds(); w > 0 {
			out.RateWindow = float64(windowDelivered) / perMsg / w
		}
		out.BacklogEnd = out.Expected - windowDelivered
	}
	for _, r := range readers {
		out.ReadErrors += r.errs.Load()
	}
	return out, nil
}

// ---- drain: read every inbox until quiet ----

type DrainSpec struct {
	Room  string           `json:"room"`
	Nodes map[string]*Node `json:"nodes"`
	Quiet float64          `json:"quiet"` // seconds with nothing new
	Max   float64          `json:"max"`
}

func drain() (any, error) {
	var sp DrainSpec
	if err := readSpec(&sp); err != nil {
		return nil, err
	}
	var n atomic.Int64
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var readers []*reader
	for _, nd := range sp.Nodes {
		readers = append(readers, startReader(nd, sp.Room, func(a arrival) { n.Add(1); last.Store(time.Now().UnixNano()) }))
	}
	start := time.Now()
	for time.Since(start).Seconds() < sp.Max && time.Since(time.Unix(0, last.Load())).Seconds() < sp.Quiet {
		time.Sleep(200 * time.Millisecond)
	}
	for _, r := range readers {
		r.close()
	}
	return map[string]any{"read": n.Load(), "secs": time.Since(start).Seconds(),
		"quiet": time.Since(time.Unix(0, last.Load())).Seconds() >= sp.Quiet}, nil
}

// ---- file: one attachment from one node to another ----

type FileSpec struct {
	Room    string           `json:"room"`
	Tag     string           `json:"tag"`
	Nodes   map[string]*Node `json:"nodes"`
	From    string           `json:"from"`
	To      string           `json:"to"`
	Path    string           `json:"path"` // in the sender's container
	Timeout float64          `json:"timeout"`
}

type FileOut struct {
	Tag      string  `json:"tag"`
	Size     int64   `json:"size"`
	Staged   float64 `json:"send_call_secs"` // the send call: the file is copied and hashed
	Confirm  float64 `json:"confirm_secs"`   // the approval: the message is signed
	Visible  float64 `json:"visible_secs"`   // from the approval to the message in the receiver's inbox
	Fetched  float64 `json:"fetched_secs"`   // from the approval to a verified copy on the receiver
	Rate     float64 `json:"mb_per_s"`       // size / fetched, MB = 1e6 bytes
	Verified bool    `json:"verified"`       // the copy's SHA-256 checked here as well
	State    string  `json:"state"`
	Fetches  int     `json:"fetch_calls"`
	TimedOut bool    `json:"timed_out"`
}

func sendFile() (any, error) {
	var sp FileSpec
	if err := readSpec(&sp); err != nil {
		return nil, err
	}
	from, to := sp.Nodes[sp.From], sp.Nodes[sp.To]
	deadline := time.Now().Add(time.Duration(sp.Timeout * float64(time.Second)))
	out := FileOut{Tag: sp.Tag}
	arrived := make(chan arrival, 4)
	r := startReader(to, sp.Room, func(a arrival) {
		if a.tag == sp.Tag {
			arrived <- a
		}
	})
	defer r.close()
	time.Sleep(300 * time.Millisecond)
	t0 := time.Now()
	var w localapi.SendFilesResp
	if err := from.client().Call(bg, "send", localapi.SendReq{Agent: from.Agent, Room: sp.Room,
		Text: body(sp.Tag, from.Name, 1, 0), Files: []string{sp.Path}}, &w); err != nil {
		return nil, fmt.Errorf("send with a file: %w", err)
	}
	out.Staged = time.Since(t0).Seconds()
	qs, err := call[localapi.RequestsResp](from.client(), "requests", localapi.Empty{})
	if err != nil {
		return nil, err
	}
	var q *localapi.Request
	for i := range qs.Requests {
		if qs.Requests[i].ID == w.Request {
			q = &qs.Requests[i]
		}
	}
	if q == nil || len(q.Files) != 1 {
		return nil, fmt.Errorf("request %s not listed with its file", w.Request)
	}
	out.Size = q.Files[0].Size
	t1 := time.Now()
	cf, err := call[localapi.ConfirmResp](from.client(), "confirm", localapi.ConfirmReq{ID: q.ID, Approve: true, Terms: q.Terms})
	if err != nil {
		return nil, err
	}
	if cf.State != store.RequestApproved {
		return nil, fmt.Errorf("confirm: %+v", cf)
	}
	tc := time.Now()
	out.Confirm = tc.Sub(t1).Seconds()
	var a arrival
	select {
	case a = <-arrived:
	case <-time.After(time.Until(deadline)):
		out.TimedOut = true
		out.State = "message not visible"
		return out, nil
	}
	out.Visible = a.at.Sub(tc).Seconds()
	sha := q.Files[0].SHA256
	for time.Now().Before(deadline) {
		wait := min(time.Until(deadline), 120*time.Second)
		out.Fetches++
		fr, err := call[localapi.FetchResp](to.fresh(), "fetch", localapi.FetchReq{Agent: to.Agent, Room: sp.Room, SHA256: sha,
			Message: a.id, WaitMS: wait.Milliseconds()})
		if err != nil {
			out.State = err.Error()
			time.Sleep(time.Second)
			continue
		}
		out.State = fr.State
		if fr.State == node.FetchHere {
			out.Fetched = time.Since(tc).Seconds()
			out.Rate = float64(out.Size) / 1e6 / out.Fetched
			// The path is the receiver's, under its home (/a2p2).
			local := filepath.Join(to.Home, strings.TrimPrefix(fr.Path, "/a2p2/"))
			if f, err := os.Open(local); err == nil {
				h := sha256.New()
				_, _ = io.Copy(h, f)
				f.Close()
				out.Verified = hex.EncodeToString(h.Sum(nil)) == sha
			}
			return out, nil
		}
		if fr.State != node.FetchPartial && fr.State != node.FetchComing && fr.State != node.FetchFetching {
			time.Sleep(time.Second)
		}
	}
	out.TimedOut = true
	return out, nil
}
