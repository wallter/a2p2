// Package web is the loopback UI for the human on this node.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
)

//go:embed static
var assets embed.FS

const maxBody = 64 << 10

type server struct {
	n     *node.Node
	m     *node.Mesh
	token string
}

// Handler serves the UI. Every request must present token; see DESIGN.md §6.
func Handler(n *node.Node, m *node.Mesh, token string) http.Handler {
	s := &server{n, m, token}
	mux := http.NewServeMux()
	for path, file := range map[string]string{"/": "index.html", "/app.js": "app.js", "/style.css": "style.css"} {
		mux.HandleFunc("GET "+path, s.static(file))
	}
	mux.HandleFunc("GET /api/state", s.api(s.state))
	mux.HandleFunc("GET /api/history", s.api(s.history))
	mux.HandleFunc("GET /api/trace", s.api(s.trace))
	mux.HandleFunc("POST /api/send", s.api(s.send))
	mux.HandleFunc("POST /api/seen", s.api(s.seen))
	mux.HandleFunc("POST /api/ack", s.api(s.ack))
	return s.guard(mux)
}

func (s *server) static(file string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && file == "index.html" {
			http.NotFound(w, r)
			return
		}
		b, _ := assets.ReadFile("static/" + file)
		ct := map[string]string{"html": "text/html", "js": "text/javascript", "css": "text/css"}[file[strings.LastIndex(file, ".")+1:]]
		w.Header().Set("Content-Type", ct+"; charset=utf-8")
		w.Write(b)
	}
}

// guard applies the Host/Origin checks and security headers to every request.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = strings.Trim(r.Host, "[]")
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// api wraps a JSON handler with token auth, content-type checks and error mapping.
func (s *server) api(fn func(*http.Request, http.ResponseWriter) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeJSON(w, 401, map[string]string{"error": "missing or invalid token; run `a2p2 ui` for a fresh link"})
			return
		}
		if r.Method == http.MethodPost {
			if mt, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(strings.ToLower(mt)) != "application/json" {
				writeJSON(w, 415, map[string]string{"error": "content-type must be application/json"})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		v, err := fn(r.WithContext(ctx), w)
		var ue *node.UserError
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &ue):
			writeJSON(w, 400, map[string]string{"error": ue.Msg})
		case errors.As(err, &mbe):
			writeJSON(w, 413, map[string]string{"error": "request too large"})
		case err != nil:
			writeJSON(w, 500, map[string]string{"error": "internal error"})
		default:
			writeJSON(w, 200, v)
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return &node.UserError{Msg: "bad JSON body"}
	}
	return nil
}

func (s *server) state(r *http.Request, _ http.ResponseWriter) (any, error) {
	ctx := r.Context()
	st, err := s.n.Status(ctx)
	if err != nil {
		return nil, err
	}
	peers, err := s.m.Peers(ctx)
	if err != nil {
		return nil, err
	}
	type peer struct {
		node.PeerStatus
		State string `json:"state"` // ok | problem | unknown (never contacted)
	}
	ps := []peer{}
	for _, p := range peers {
		state := "unknown"
		if p.LastError != "" {
			state = "problem"
		} else if !p.LastOK.IsZero() {
			state = "ok"
		}
		ps = append(ps, peer{p, state})
	}
	unread, err := s.n.Inbox(ctx, s.n.HumanID(), "", 50, 0)
	if err != nil {
		return nil, err
	}
	return map[string]any{"node": st.Node, "name": st.Name, "human": s.n.HumanID(), "rooms": st.Rooms,
		"agents": st.Agents, "peers": ps, "listen": s.m.Addrs(), "outbox": st.Outbox, "unread": unread, "time": st.Time}, nil
}

func (s *server) history(r *http.Request, _ http.ResponseWriter) (any, error) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	return s.n.History(r.Context(), q.Get("room"), before, limit)
}

func (s *server) trace(r *http.Request, _ http.ResponseWriter) (any, error) {
	return s.n.Trace(r.Context(), r.URL.Query().Get("id"))
}

func (s *server) send(r *http.Request, _ http.ResponseWriter) (any, error) {
	var in struct {
		ID      string   `json:"id"`
		Room    string   `json:"room"`
		Text    string   `json:"text"`
		To      []string `json:"to"`
		ReplyTo string   `json:"reply_to"`
		Kind    string   `json:"kind"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	kind := core.Kind(in.Kind)
	switch {
	case kind == "" || kind == core.KindMessage:
	case kind == core.KindAnswer && in.ReplyTo != "":
	case kind == core.KindAnswer:
		return nil, &node.UserError{Msg: "an answer needs reply_to"}
	default:
		return nil, &node.UserError{Msg: "kind must be message or answer"}
	}
	id := in.ID
	if id == "" {
		id = core.NewID(time.Now())
	}
	return s.n.Send(r.Context(), node.SendReq{ID: id, From: s.n.HumanID(), Room: in.Room, To: in.To,
		Text: in.Text, ReplyTo: in.ReplyTo, Kind: kind})
}

func (s *server) seen(r *http.Request, _ http.ResponseWriter) (any, error) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if len(in.IDs) > 200 {
		return nil, &node.UserError{Msg: "too many ids"}
	}
	return map[string]bool{"ok": true}, s.n.Seen(r.Context(), s.n.HumanID(), in.IDs...)
}

func (s *server) ack(r *http.Request, _ http.ResponseWriter) (any, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.n.Ack(r.Context(), s.n.HumanID(), in.ID)
}
