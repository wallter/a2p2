// Package localapi is the daemon's API for processes on the same machine:
// JSON over HTTP over a Unix socket. Access to the socket is the only
// credential; see "Trust model" in docs/DESIGN.md.
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

// Request and response bodies. Every endpoint is POST /v1/<name>.
type (
	RegisterReq struct {
		Harness string `json:"harness"`
		Label   string `json:"label,omitempty"`
		Cwd     string `json:"cwd,omitempty"`
	}
	InboxReq struct {
		Agent  string `json:"agent"`
		Room   string `json:"room,omitempty"`
		Limit  int    `json:"limit,omitempty"`
		WaitMS int    `json:"wait_ms,omitempty"`
	}
	Messages struct {
		Messages []store.Message `json:"messages"`
	}
	SeenReq struct {
		Agent string   `json:"agent"`
		IDs   []string `json:"ids"`
	}
	AckReq struct {
		Agent string `json:"agent"`
		ID    string `json:"id"`
	}
	HistoryReq struct {
		Room   string `json:"room,omitempty"`
		Before int64  `json:"before,omitempty"`
		Limit  int    `json:"limit,omitempty"`
	}
	IDReq struct {
		ID string `json:"id"`
	}
	// UnreadReq is what a harness hook knows about itself.
	UnreadReq struct {
		Harness string `json:"harness"`
		Cwd     string `json:"cwd"`
	}
	// Unread is a summary safe to inject into a prompt: counts and sender
	// labels, never message text.
	Unread struct {
		Agent string   `json:"agent,omitempty"` // empty: no agent matches this hook
		Count int      `json:"count"`
		From  []string `json:"from,omitempty"`
	}
	RoomReq struct {
		Room string `json:"room"`
	}
	InviteResp struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires"`
		Room    string    `json:"room"`
	}
	JoinReq struct {
		Token string `json:"token"`
	}
	RemoveReq struct {
		Room string `json:"room"`
		Node string `json:"node"`
	}
	Status struct {
		node.Status
		Peers   []node.PeerStatus `json:"peers"`
		Listen  []string          `json:"listen"` // addresses other nodes can dial; empty = networking off
		UI      string            `json:"ui"`     // URL of the web UI including its access token
		Version string            `json:"version"`
		PID     int               `json:"pid"`
	}
	Empty struct{}
)

// Info is daemon-level state the API reports but does not own.
type Info struct {
	Version string
	Addrs   func() []string // dialable peer addresses; nil or empty when networking is off
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
		body := http.MaxBytesReader(w, r.Body, 4*core.MaxEnvelope)
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

// Handler serves the local API for a node.
func Handler(n *node.Node, m *node.Mesh, info Info) http.Handler {
	mux := http.NewServeMux()
	addrs := func() []string {
		if info.Addrs == nil {
			return nil
		}
		return info.Addrs()
	}
	handle(mux, "register", func(ctx context.Context, r RegisterReq) (store.Agent, error) {
		return n.Register(ctx, r.Harness, r.Label, r.Cwd)
	})
	handle(mux, "send", func(ctx context.Context, r node.SendReq) (store.Message, error) {
		return n.Send(ctx, r)
	})
	handle(mux, "inbox", func(ctx context.Context, r InboxReq) (Messages, error) {
		msgs, err := n.Inbox(ctx, r.Agent, r.Room, r.Limit, time.Duration(r.WaitMS)*time.Millisecond)
		return Messages{msgs}, err
	})
	handle(mux, "seen", func(ctx context.Context, r SeenReq) (Empty, error) {
		return Empty{}, n.Seen(ctx, r.Agent, r.IDs...)
	})
	handle(mux, "ack", func(ctx context.Context, r AckReq) (Empty, error) {
		return Empty{}, n.Ack(ctx, r.Agent, r.ID)
	})
	handle(mux, "history", func(ctx context.Context, r HistoryReq) (Messages, error) {
		msgs, err := n.History(ctx, r.Room, r.Before, r.Limit)
		return Messages{msgs}, err
	})
	handle(mux, "trace", func(ctx context.Context, r IDReq) (node.Trace, error) {
		return n.Trace(ctx, r.ID)
	})
	handle(mux, "unread", func(ctx context.Context, r UnreadReq) (Unread, error) {
		a, ok, err := n.AgentFor(ctx, core.CleanLabel(r.Harness), r.Cwd)
		if err != nil || !ok {
			return Unread{}, err
		}
		msgs, err := n.Inbox(ctx, a.ID, "", 50, 0)
		u := Unread{Agent: a.ID, Count: len(msgs)}
		seen := map[string]bool{}
		for _, m := range msgs {
			if !seen[m.From] {
				seen[m.From] = true
				u.From = append(u.From, m.From)
			}
		}
		return u, err
	})
	handle(mux, "status", func(ctx context.Context, _ Empty) (Status, error) {
		st, err := n.Status(ctx)
		if err != nil {
			return Status{}, err
		}
		out := Status{Status: st, Listen: addrs(), Version: info.Version, PID: info.PID}
		if info.UIURL != nil {
			out.UI = info.UIURL()
		}
		out.Peers, err = m.Peers(ctx)
		return out, err
	})
	handle(mux, "room/create", func(ctx context.Context, r RoomReq) (store.Room, error) {
		if len(addrs()) == 0 {
			return store.Room{}, &node.UserError{Msg: "networking is off, so nobody could join; run: a2p2 net on"}
		}
		return n.CreateRoom(ctx, r.Room, addrs())
	})
	handle(mux, "room/invite", func(ctx context.Context, r RoomReq) (InviteResp, error) {
		tok, exp, err := n.Invite(ctx, r.Room, addrs())
		return InviteResp{Token: tok, Expires: exp, Room: r.Room}, err
	})
	handle(mux, "room/join", func(ctx context.Context, r JoinReq) (store.Room, error) {
		if len(addrs()) == 0 {
			return store.Room{}, &node.UserError{Msg: "networking is off, so other members could not reach this node; run: a2p2 net on"}
		}
		return m.Join(ctx, r.Token)
	})
	handle(mux, "room/remove", func(ctx context.Context, r RemoveReq) (store.Room, error) {
		return n.RemoveMember(ctx, r.Room, r.Node)
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
	var body bytes.Buffer
	if req != nil {
		if err := json.NewEncoder(&body).Encode(req); err != nil {
			return err
		}
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://a2p2/v1/"+name, &body)
	if err != nil {
		return err
	}
	res, err := c.hc.Do(hr)
	if err != nil {
		return fmt.Errorf("a2p2 daemon not reachable: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var ae apiError
		_ = json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&ae)
		if ae.Error == "" {
			ae.Error = res.Status
		}
		return &Error{Msg: ae.Error, User: res.StatusCode == http.StatusBadRequest}
	}
	if resp == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(resp)
}
