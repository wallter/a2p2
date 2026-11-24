// Package localapi is the daemon's API for processes on the same machine:
// JSON over HTTP over a Unix socket (PROTOCOL-next section 7). Access to
// the socket is the only credential; see "Trust model" in docs/DESIGN.md.
// Every call is one call into node.Surface.
package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// maxBody bounds a request body.
const maxBody = 256 << 10

// Time renders a time as PROTOCOL-next section 0 does; zero is empty.
func Time(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// Requests and responses. Every call is POST /v1/<name>.
type (
	RegisterReq struct {
		Harness string `json:"harness"`
		Label   string `json:"label,omitempty"`
		Cwd     string `json:"cwd,omitempty"`
	}
	SendReq struct {
		Agent      string   `json:"agent"`
		Room       string   `json:"room,omitempty"`
		Text       string   `json:"text"`
		To         []string `json:"to,omitempty"`
		ReplyTo    string   `json:"reply_to,omitempty"`
		AskHandled bool     `json:"ask_handled,omitempty"`
		Replaces   string   `json:"replaces,omitempty"`
		Choices    []string `json:"choices,omitempty"`
	}
	SendResp struct {
		ID      string `json:"id"`
		Room    string `json:"room"`
		Created string `json:"created"`
	}
	InboxReq struct {
		Agent  string `json:"agent"`
		Room   string `json:"room,omitempty"`
		Limit  int    `json:"limit,omitempty"`
		WaitMS int64  `json:"wait_ms,omitempty"`
	}
	Entry struct {
		Key           string   `json:"key"`
		Version       string   `json:"version,omitempty"`
		Kind          string   `json:"kind"`
		ID            string   `json:"id"`
		Room          string   `json:"room"`
		From          string   `json:"from"`
		FromNode      string   `json:"from_node"`
		AuthorRemoved bool     `json:"author_removed,omitempty"`
		To            []string `json:"to,omitempty"`
		Parent        string   `json:"parent,omitempty"`
		Thread        string   `json:"thread,omitempty"`
		ParentState   string   `json:"parent_state,omitempty"`
		Late          bool     `json:"late,omitempty"`
		Created       string   `json:"created"`
		Text          string   `json:"text,omitempty"`
		Choices       []string `json:"choices,omitempty"`
	}
	Wait struct {
		Room   string `json:"room"`
		Author string `json:"author"`
		Have   int64  `json:"have"`
		Known  int64  `json:"known"`
		Since  string `json:"since,omitempty"`
		Reason string `json:"reason"`
	}
	InboxResp struct {
		Entries []Entry `json:"entries"`
		Waiting []Wait  `json:"waiting"`
	}
	Entries struct {
		Entries []Entry `json:"entries"`
	}
	ThreadReq struct {
		Agent string `json:"agent"`
		ID    string `json:"id"`
	}
	SeenEntry struct {
		Key     string `json:"key"`
		Version string `json:"version,omitempty"`
	}
	SeenReq struct {
		Agent   string      `json:"agent"`
		Entries []SeenEntry `json:"entries"`
	}
	SeenResult struct {
		Key    string `json:"key"`
		OK     bool   `json:"ok"`
		Reason string `json:"reason,omitempty"`
	}
	SeenResp struct {
		Results []SeenResult `json:"results"`
	}
	AckReq struct {
		Agent string `json:"agent"`
		ID    string `json:"id"`
	}
	AckResp struct {
		Handled string `json:"handled"`
	}
	HistoryReq struct {
		Room   string `json:"room,omitempty"`
		Before string `json:"before,omitempty"`
		Limit  int    `json:"limit,omitempty"`
	}
	IDReq struct {
		ID string `json:"id"`
	}
	// Copy: machine node showed this one, at the time at (this machine's
	// observation), that it holds the author's stream up to the message's
	// position. Evidence is "position": it does not tell forks apart.
	// Member is membership now.
	Copy struct {
		Node     string `json:"node"`
		At       string `json:"at"`
		Member   bool   `json:"member"`
		Evidence string `json:"evidence"`
	}
	// Handled: at is when the report was written, as its author signed it.
	Handled struct {
		Agent string `json:"agent"`
		At    string `json:"at"`
	}
	// TraceResp: saved is true when this machine holds the message's
	// signed header; reason is the state of its text here. An id not held
	// here is an error.
	TraceResp struct {
		ID      string    `json:"id"`
		Room    string    `json:"room"`
		Saved   bool      `json:"saved"`
		Reason  string    `json:"reason"`
		Copies  []Copy    `json:"copies"`
		Handled []Handled `json:"handled"`
	}
	WaitReq struct {
		WaitMS int64 `json:"wait_ms,omitempty"`
	}
	Alert struct {
		Kind  string `json:"kind"`
		ID    string `json:"id"`
		Room  string `json:"room"`
		Text  string `json:"text"`
		Since string `json:"since,omitempty"`
	}
	AlertsResp struct {
		Alerts []Alert `json:"alerts"`
	}
	// Request is a decision waiting for (or taken by) this machine's
	// person, every term its own field. Node is the full node id and
	// Fingerprint the same grouped for reading.
	Request struct {
		ID          string `json:"id"`
		Kind        string `json:"kind"`
		Action      string `json:"action"`
		Room        string `json:"room"`
		RoomLabel   string `json:"room_label"`
		Node        string `json:"node"`
		Fingerprint string `json:"fingerprint"`
		Name        string `json:"name,omitempty"`
		Role        string `json:"role,omitempty"`
		History     bool   `json:"history"`
		Seconding   bool   `json:"seconding,omitempty"`
		Created     string `json:"created"`
		Deadline    string `json:"deadline,omitempty"`
		// Text says exactly what approving it does.
		Text string `json:"text"`
	}
	RequestsResp struct {
		Requests []Request `json:"requests"`
	}
	ConfirmReq struct {
		ID      string `json:"id"`
		Approve bool   `json:"approve"`
	}
	ConfirmResp struct {
		ID      string   `json:"id"`
		State   string   `json:"state"`
		Reason  string   `json:"reason,omitempty"`
		Entries []string `json:"entries,omitempty"`
		Effect  string   `json:"effect,omitempty"`
	}
	RoomReq struct {
		Room string `json:"room"`
	}
	InviteReq struct {
		Room    string `json:"room"`
		History bool   `json:"history,omitempty"`
		Role    string `json:"role,omitempty"`
	}
	InviteResp struct {
		Token   string `json:"token"`
		Expires string `json:"expires"`
		Room    string `json:"room"`
	}
	JoinReq struct {
		Token string `json:"token"`
	}
	JoinRoom struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Create string `json:"create"`
	}
	// PreviewResp is the previewed answer of PROTOCOL-next 5.3, with the
	// preview's id, which accept must name.
	PreviewResp struct {
		State   string   `json:"state"`
		Room    JoinRoom `json:"room"`
		Role    string   `json:"role"`
		History bool     `json:"history"`
		Member  bool     `json:"member"`
		Admin   string   `json:"admin"`
		Expires string   `json:"expires"`
		Preview string   `json:"preview"`
	}
	AcceptReq struct {
		Token   string `json:"token"`
		Preview string `json:"preview"`
		WaitMS  int64  `json:"wait_ms,omitempty"`
	}
	AcceptResp struct {
		State   string `json:"state"`
		Room    string `json:"room,omitempty"`
		Reason  string `json:"reason,omitempty"`
		Role    string `json:"role,omitempty"`
		History bool   `json:"history,omitempty"`
		Note    string `json:"note,omitempty"`
	}
	MemberReq struct {
		Room string `json:"room"`
		Node string `json:"node"`
	}
	// ChangeResp: room/admin and room/remove raise a request for a person
	// (P3); confirm signs it.
	ChangeResp struct {
		Request string  `json:"request"`
		State   string  `json:"state"`
		Text    string  `json:"text"`
		Terms   Request `json:"terms"`
	}
	// NetReq is a person's networking decision: state off, outbound or
	// on, and with on the address to listen on. It has the authority of
	// confirm; no agent tool or hook sends it.
	NetReq struct {
		State  string `json:"state"`
		Listen string `json:"listen,omitempty"`
	}
	// Network is a machine's networking state. NetResp is the decision
	// recorded; it applies when the daemon next starts.
	Network struct {
		State   string `json:"state"`
		Listen  string `json:"listen,omitempty"`
		Request string `json:"request,omitempty"`
	}
	NetResp struct {
		Network
		Text    string `json:"text"`
		Applies string `json:"applies"`
	}
	StatusReq struct {
		// ShowNotice: a person is shown this status, so a one-time notice
		// is returned and recorded as shown.
		ShowNotice bool `json:"show_notice,omitempty"`
	}
	RoomListReq struct {
		Room string `json:"room,omitempty"`
	}
	Member struct {
		Node  string `json:"node"`
		Name  string `json:"name,omitempty"`
		Admin bool   `json:"admin,omitempty"`
	}
	Change struct {
		Entry  string `json:"entry"`
		Kind   string `json:"kind"`
		Author string `json:"author"`
		Target string `json:"target,omitempty"`
		State  string `json:"state,omitempty"`
		Reason string `json:"reason,omitempty"`
	}
	NoEffect struct {
		Entry  string `json:"entry"`
		Reason string `json:"reason"`
	}
	Fork struct {
		Author  string    `json:"author"`
		Seq     int64     `json:"seq"`
		Entries [2]string `json:"entries"`
		Open    bool      `json:"open,omitempty"`
	}
	Room struct {
		ID       string     `json:"id"`
		Label    string     `json:"label"`
		Members  []Member   `json:"members"`
		Admins   []string   `json:"admins"`
		Pending  []Change   `json:"pending"`
		NoEffect []NoEffect `json:"no_effect"`
		Forks    []Fork     `json:"forks"`
		Limits   []string   `json:"limits"`
		// Waiting is the number of requests in this room waiting for this
		// machine's person.
		Waiting int `json:"waiting"`
	}
	RoomsResp struct {
		Rooms []Room `json:"rooms"`
	}
	// UnreadReq is what a harness hook knows about itself; Lines asks for
	// first lines too (U4).
	UnreadReq struct {
		Harness string `json:"harness"`
		Cwd     string `json:"cwd"`
		Lines   bool   `json:"lines,omitempty"`
	}
	RoomCount struct {
		Room  string `json:"room"`
		Label string `json:"label"`
		Count int    `json:"count"`
	}
	FirstLine struct {
		ID   string `json:"id"`
		Room string `json:"room"`
		From string `json:"from"`
		Line string `json:"line"`
	}
	// Unread is counts and sender labels, and first lines only when asked.
	// It never marks anything seen.
	Unread struct {
		Agent string      `json:"agent,omitempty"` // empty: no agent matches this hook
		Count int         `json:"count"`
		From  []string    `json:"from,omitempty"`
		Rooms []RoomCount `json:"rooms,omitempty"`
		First []FirstLine `json:"first,omitempty"`
	}
	Peer struct {
		Node      string   `json:"node"`
		Name      string   `json:"name,omitempty"`
		Rooms     []string `json:"rooms"`
		Addrs     []string `json:"addrs,omitempty"`
		LastHeard string   `json:"last_heard,omitempty"`
	}
	Status struct {
		Node    string        `json:"node"`
		Name    string        `json:"name"`
		Human   string        `json:"human"`
		Local   string        `json:"local"`
		Rooms   []Room        `json:"rooms"`
		Agents  []store.Agent `json:"agents"`
		Peers   []Peer        `json:"peers"`
		Network Network       `json:"network"`
		Waiting int           `json:"waiting"` // requests waiting for this machine's person
		Listen  []string      `json:"listen"`  // addresses other machines can dial; empty: no listener
		UI      string        `json:"ui"`      // URL of the web page including its access token
		Version string        `json:"version"`
		PID     int           `json:"pid"`
		Notice  string        `json:"notice,omitempty"`
	}
	Empty struct{}
)

// Info is daemon-level state the API reports but does not own.
type Info struct {
	Version string
	UIURL   func() string
	PID     int
	Stop    func() // asks the daemon to shut down
}

type apiError struct {
	Error string `json:"error"`
}

func handle[Req, Resp any](mux *http.ServeMux, name string, fn func(context.Context, Req) (Resp, error)) {
	mux.HandleFunc("POST /v1/"+name, func(w http.ResponseWriter, r *http.Request) {
		var req Req
		body := http.MaxBytesReader(w, r.Body, maxBody)
		if err := json.NewDecoder(body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, apiError{"bad request: " + err.Error()})
			return
		}
		resp, err := fn(r.Context(), req)
		var ue *node.UserError
		switch {
		case errors.As(err, &ue):
			writeJSON(w, http.StatusBadRequest, apiError{ue.Msg})
		case err != nil:
			slog.Error("local api", "call", name, "err", err)
			writeJSON(w, http.StatusInternalServerError, apiError{"internal error: " + err.Error()})
		default:
			writeJSON(w, http.StatusOK, resp)
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func userErr(msg string) error { return &node.UserError{Msg: msg} }

// waitOf checks a wait in milliseconds (at most 120,000).
func waitOf(ms int64) (time.Duration, error) {
	if ms < 0 || ms > node.MaxWait.Milliseconds() {
		return 0, userErr("wait_ms is 0 to 120000")
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// EntryOf renders a node entry (PROTOCOL-next section 7, Entry).
func EntryOf(e node.Entry) Entry {
	out := Entry{Key: e.Key, Version: e.Version, Kind: e.Kind, ID: e.ID, Room: e.Room, From: e.From, FromNode: e.FromNode,
		AuthorRemoved: e.AuthorRemoved, Parent: e.Parent, Thread: e.Thread, ParentState: e.ParentState, Late: e.Late,
		Created: Time(e.Created), Text: e.Text, Choices: e.Choices}
	for _, a := range e.To {
		out.To = append(out.To, node.AgentID(a))
	}
	return out
}

func entriesOf(es []node.Entry) []Entry {
	out := make([]Entry, 0, len(es))
	for _, e := range es {
		out = append(out, EntryOf(e))
	}
	return out
}

// RoomOf renders a room as room/list shows it.
func RoomOf(r node.RoomInfo) Room {
	out := Room{ID: r.ID, Label: r.Label, Members: []Member{}, Admins: append([]string{}, r.Admins...), Pending: []Change{},
		NoEffect: []NoEffect{}, Forks: []Fork{}, Limits: append([]string{}, r.Limits...), Waiting: len(r.Requests)}
	for _, m := range r.Members {
		out.Members = append(out.Members, Member{Node: m.Node, Name: m.Name, Admin: m.Admin})
	}
	for _, c := range r.Pending {
		out.Pending = append(out.Pending, Change{Entry: c.Entry, Kind: c.Kind, Author: c.Author, Target: c.Target, State: c.State, Reason: c.Reason})
	}
	for _, c := range r.NoEffect {
		out.NoEffect = append(out.NoEffect, NoEffect{Entry: c.Entry, Reason: c.Reason})
	}
	for _, f := range r.Forks {
		out.Forks = append(out.Forks, Fork{Author: f.Author, Seq: f.Seq, Entries: f.Entries, Open: f.Open})
	}
	return out
}

func roomsOf(rs []node.RoomInfo) []Room {
	out := make([]Room, 0, len(rs))
	for _, r := range rs {
		out = append(out, RoomOf(r))
	}
	return out
}

// RequestOf renders a request for a person.
func RequestOf(q node.RequestView) Request {
	return Request{ID: q.ID, Kind: q.Kind, Action: q.Action, Room: q.Room, RoomLabel: q.RoomLabel, Node: q.Machine,
		Fingerprint: q.Fingerprint, Name: q.Name, Role: q.Role, History: q.History, Seconding: q.Seconding,
		Created: Time(q.Created), Deadline: Time(q.Deadline), Text: q.Text}
}

// NetworkOf renders a networking state.
func NetworkOf(n node.Network) Network {
	return Network{State: n.State, Listen: n.Listen, Request: n.Request}
}

// AlertOf renders an alert.
func AlertOf(a node.Alert) Alert {
	return Alert{Kind: a.Kind, ID: a.ID, Room: a.Room, Text: a.Text, Since: Time(a.Since)}
}

// TraceOf renders a trace.
func TraceOf(t node.TraceView) TraceResp {
	out := TraceResp{ID: t.ID, Room: t.Room, Saved: t.Saved, Reason: t.Reason, Copies: []Copy{}, Handled: []Handled{}}
	for _, c := range t.Copies {
		out.Copies = append(out.Copies, Copy{Node: c.Node, At: Time(c.At), Member: c.Member, Evidence: "position"})
	}
	for _, h := range t.Handled {
		out.Handled = append(out.Handled, Handled{Agent: h.Agent, At: Time(h.At)})
	}
	return out
}

// PreviewOf renders a join preview.
func PreviewOf(p node.Preview) PreviewResp {
	return PreviewResp{State: p.State, Room: JoinRoom{ID: p.Room, Label: p.Label, Create: p.Create}, Role: p.Role,
		History: p.History, Member: p.Member, Admin: p.Admin, Expires: Time(p.Expires), Preview: p.ID}
}

// StatusOf renders the machine at a glance.
func StatusOf(st node.StatusView, listen []string, info Info) Status {
	out := Status{Node: st.Node, Name: st.Name, Human: st.Human, Local: st.Local, Rooms: roomsOf(st.Rooms),
		Agents: st.Agents, Peers: []Peer{}, Listen: listen, Version: info.Version, PID: info.PID, Notice: st.Notice,
		Network: NetworkOf(st.Network), Waiting: st.Waiting}
	if out.Listen == nil {
		out.Listen = []string{}
	}
	if out.Agents == nil {
		out.Agents = []store.Agent{}
	}
	for _, p := range st.Peers {
		out.Peers = append(out.Peers, Peer{Node: p.Node, Name: p.Name, Rooms: p.Rooms, Addrs: p.Addrs, LastHeard: Time(p.LastHeard)})
	}
	if info.UIURL != nil {
		out.UI = info.UIURL()
	}
	return out
}

// Handler serves the local API over s.
func Handler(s *node.Surface, info Info) http.Handler {
	mux := http.NewServeMux()
	handle(mux, "register", func(ctx context.Context, r RegisterReq) (store.Agent, error) {
		return s.Register(ctx, r.Harness, r.Label, r.Cwd)
	})
	handle(mux, "send", func(ctx context.Context, r SendReq) (SendResp, error) {
		m, err := s.Send(ctx, node.SendInput{Agent: r.Agent, Room: r.Room, Text: r.Text, To: r.To, ReplyTo: r.ReplyTo,
			AskHandled: r.AskHandled, Replaces: r.Replaces, Choices: r.Choices})
		return SendResp{ID: m.ID, Room: m.Room, Created: Time(m.Created)}, err
	})
	handle(mux, "inbox", func(ctx context.Context, r InboxReq) (InboxResp, error) {
		wait, err := waitOf(r.WaitMS)
		if err != nil {
			return InboxResp{}, err
		}
		v, err := s.Inbox(ctx, r.Agent, r.Room, r.Limit, wait)
		out := InboxResp{Entries: entriesOf(v.Entries), Waiting: []Wait{}}
		for _, w := range v.Waiting {
			out.Waiting = append(out.Waiting, Wait{Room: w.Room, Author: w.Author, Have: w.Have, Known: w.Known,
				Since: Time(w.Since), Reason: w.Reason})
		}
		return out, err
	})
	handle(mux, "thread", func(ctx context.Context, r ThreadReq) (Entries, error) {
		es, err := s.Thread(ctx, r.Agent, r.ID)
		return Entries{entriesOf(es)}, err
	})
	handle(mux, "seen", func(ctx context.Context, r SeenReq) (SeenResp, error) {
		reads := make([]store.Read, 0, len(r.Entries))
		for _, e := range r.Entries {
			reads = append(reads, store.Read{Key: e.Key, Version: e.Version})
		}
		res, err := s.Seen(ctx, r.Agent, reads)
		out := SeenResp{Results: []SeenResult{}}
		for _, x := range res {
			out.Results = append(out.Results, SeenResult{Key: x.Key, OK: x.Confirmed, Reason: x.Reason})
		}
		return out, err
	})
	handle(mux, "ack", func(ctx context.Context, r AckReq) (AckResp, error) {
		h, err := s.Ack(ctx, r.Agent, r.ID)
		return AckResp{Handled: h.ID}, err
	})
	handle(mux, "history", func(ctx context.Context, r HistoryReq) (Entries, error) {
		es, err := s.History(ctx, r.Room, r.Before, r.Limit)
		return Entries{entriesOf(es)}, err
	})
	handle(mux, "trace", func(ctx context.Context, r IDReq) (TraceResp, error) {
		t, err := s.Trace(ctx, r.ID)
		return TraceOf(t), err
	})
	handle(mux, "alerts", func(ctx context.Context, r WaitReq) (AlertsResp, error) {
		wait, err := waitOf(r.WaitMS)
		if err != nil {
			return AlertsResp{}, err
		}
		as, err := s.Alerts(ctx, wait)
		out := AlertsResp{Alerts: []Alert{}}
		for _, a := range as {
			out.Alerts = append(out.Alerts, AlertOf(a))
		}
		return out, err
	})
	handle(mux, "requests", func(ctx context.Context, _ Empty) (RequestsResp, error) {
		qs, err := s.Waiting(ctx)
		out := RequestsResp{Requests: []Request{}}
		for _, q := range qs {
			out.Requests = append(out.Requests, RequestOf(q))
		}
		return out, err
	})
	handle(mux, "confirm", func(ctx context.Context, r ConfirmReq) (ConfirmResp, error) {
		c, err := s.Confirm(ctx, r.ID, r.Approve)
		return ConfirmResp{ID: c.ID, State: c.State, Reason: c.Reason, Entries: c.Entries, Effect: c.Effect}, err
	})
	handle(mux, "room/create", func(ctx context.Context, r RoomReq) (Room, error) {
		info, err := s.CreateRoom(ctx, r.Room)
		return RoomOf(info), err
	})
	handle(mux, "room/invite", func(ctx context.Context, r InviteReq) (InviteResp, error) {
		inv, err := s.Invite(ctx, r.Room, r.Role, r.History)
		return InviteResp{Token: inv.Token, Expires: Time(inv.Expires), Room: inv.Room}, err
	})
	handle(mux, "room/join", func(ctx context.Context, r JoinReq) (PreviewResp, error) {
		p, err := s.JoinPreview(ctx, r.Token)
		var ref *node.JoinRefused
		if errors.As(err, &ref) {
			err = userErr(ref.Error())
		}
		return PreviewOf(p), err
	})
	handle(mux, "room/join/accept", func(ctx context.Context, r AcceptReq) (AcceptResp, error) {
		wait, err := waitOf(r.WaitMS)
		if err != nil {
			return AcceptResp{}, err
		}
		o, err := s.JoinAccept(ctx, r.Token, r.Preview, wait)
		var ref *node.JoinRefused
		if errors.As(err, &ref) {
			return AcceptResp{State: node.JoinStateRefused, Reason: ref.Code}, nil
		}
		return AcceptResp{State: o.State, Room: o.Room, Reason: o.Reason, Role: o.Role, History: o.History, Note: o.Note}, err
	})
	change := func(fn func(context.Context, string, string) (store.Request, error)) func(context.Context, MemberReq) (ChangeResp, error) {
		return func(ctx context.Context, r MemberReq) (ChangeResp, error) {
			q, err := fn(ctx, r.Room, r.Node)
			if err != nil {
				return ChangeResp{}, err
			}
			v, err := s.Describe(ctx, q)
			return ChangeResp{Request: q.ID, State: "waiting", Text: v.Text, Terms: RequestOf(v)}, err
		}
	}
	handle(mux, "room/admin", change(s.Admin))
	handle(mux, "room/remove", change(s.Remove))
	handle(mux, "room/leave", func(ctx context.Context, r RoomReq) (Empty, error) {
		return Empty{}, s.Leave(ctx, r.Room)
	})
	handle(mux, "room/list", func(ctx context.Context, r RoomListReq) (RoomsResp, error) {
		rs, err := s.RoomList(ctx, r.Room)
		return RoomsResp{roomsOf(rs)}, err
	})
	handle(mux, "unread", func(ctx context.Context, r UnreadReq) (Unread, error) {
		a, ok, err := s.AgentFor(ctx, core.CleanLabel(r.Harness), r.Cwd)
		if err != nil || !ok {
			return Unread{}, err
		}
		u, err := s.Unread(ctx, a.ID, r.Lines)
		out := Unread{Agent: u.Agent, Count: u.Count, From: u.From}
		for _, c := range u.Rooms {
			out.Rooms = append(out.Rooms, RoomCount{Room: c.Room, Label: c.Label, Count: c.Count})
		}
		for _, f := range u.First {
			out.First = append(out.First, FirstLine{ID: f.ID, Room: f.Room, From: f.From, Line: f.Line})
		}
		return out, err
	})
	handle(mux, "status", func(ctx context.Context, r StatusReq) (Status, error) {
		st, err := s.Status(ctx, r.ShowNotice)
		return StatusOf(st, s.Addrs(), info), err
	})
	// net records a person's networking decision, with the authority of
	// confirm: the CLI asks for it at a terminal and no agent tool or hook
	// calls it. That keeps it out of the agents' tools and recorded; it
	// does not prove that a person made it, since any program running as
	// this user can call this socket.
	handle(mux, "net", func(ctx context.Context, r NetReq) (NetResp, error) {
		n, err := s.SetNetwork(ctx, node.Network{State: r.State, Listen: r.Listen})
		return NetResp{Network: NetworkOf(n), Text: node.NetworkText(n), Applies: "when the daemon next starts"}, err
	})
	handle(mux, "stop", func(ctx context.Context, _ Empty) (Empty, error) {
		if info.Stop != nil {
			go info.Stop()
		}
		return Empty{}, nil
	})
	return mux
}

// Client talks to a daemon over its Unix socket.
type Client struct {
	hc *http.Client
}

// NewClient returns a client for the daemon listening on sock.
func NewClient(sock string) *Client {
	return &Client{hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}}
}

// Error is a failure reported by the daemon. User is true when the caller
// asked for something invalid (as opposed to the daemon breaking).
type Error struct {
	Msg  string
	User bool
}

func (e *Error) Error() string { return e.Msg }

// Call invokes POST /v1/<name>. req may be nil; resp may be nil.
func (c *Client) Call(ctx context.Context, name string, req, resp any) error {
	_, err := c.CallRaw(ctx, name, req, resp)
	return err
}

// CallRaw is Call that also returns the response body as received, for
// --json, which prints it unchanged.
func (c *Client) CallRaw(ctx context.Context, name string, req, resp any) ([]byte, error) {
	var body bytes.Buffer
	if req != nil {
		if err := json.NewEncoder(&body).Encode(req); err != nil {
			return nil, err
		}
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://a2p2/v1/"+name, &body)
	if err != nil {
		return nil, err
	}
	res, err := c.hc.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("a2p2 daemon not reachable: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var ae apiError
		_ = json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&ae)
		if ae.Error == "" {
			ae.Error = res.Status
		}
		return nil, &Error{Msg: ae.Error, User: res.StatusCode == http.StatusBadRequest}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil || resp == nil {
		return raw, err
	}
	return raw, json.Unmarshal(raw, resp)
}
