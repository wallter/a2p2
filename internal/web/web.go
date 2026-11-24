// Package web is the loopback page for the person who runs this machine:
// rooms, members and admins, messages with threads, alerts, and requests
// waiting for a person with approve and decline (DESIGN-next sections 6
// and 7). The person acts as the human agent of this machine. Every call is
// one call into node.Surface.
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

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

//go:embed static
var assets embed.FS

const maxBody = 64 << 10

type server struct {
	s     *node.Surface
	token string
}

// Handler serves the page. Every API request must present token; see
// DESIGN.md section 6.
func Handler(s *node.Surface, token string) http.Handler {
	sv := &server{s, token}
	mux := http.NewServeMux()
	for path, file := range map[string]string{"/": "index.html", "/app.js": "app.js", "/style.css": "style.css"} {
		mux.HandleFunc("GET "+path, sv.static(file))
	}
	mux.HandleFunc("GET /api/state", sv.api(sv.state))
	mux.HandleFunc("GET /api/history", sv.api(sv.history))
	mux.HandleFunc("GET /api/thread", sv.api(sv.thread))
	mux.HandleFunc("GET /api/trace", sv.api(sv.trace))
	mux.HandleFunc("POST /api/send", sv.api(sv.send))
	mux.HandleFunc("POST /api/seen", sv.api(sv.seen))
	mux.HandleFunc("POST /api/ack", sv.api(sv.ack))
	mux.HandleFunc("POST /api/confirm", sv.api(sv.confirm))
	return sv.guard(mux)
}

func (sv *server) static(file string) http.HandlerFunc {
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
func (sv *server) guard(next http.Handler) http.Handler {
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
func (sv *server) api(fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(got), []byte(sv.token)) != 1 {
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
		v, err := fn(r.WithContext(ctx))
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

func entries(es []node.Entry, err error) (any, error) {
	out := make([]localapi.Entry, 0, len(es))
	for _, e := range es {
		out = append(out, localapi.EntryOf(e))
	}
	return map[string]any{"entries": out}, err
}

// state is everything the page shows at once. The human's unread entries
// are read without marking anything seen; the page confirms what it showed.
func (sv *server) state(r *http.Request) (any, error) {
	ctx := r.Context()
	st, err := sv.s.Status(ctx)
	if err != nil {
		return nil, err
	}
	in, err := sv.s.Inbox(ctx, sv.s.HumanID(), "", node.MaxInboxLimit, 0)
	if err != nil {
		return nil, err
	}
	as, err := sv.s.Alerts(ctx, 0)
	if err != nil {
		return nil, err
	}
	qs, err := sv.s.Requests(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"status": localapi.StatusOf(st, sv.s.Addrs(), localapi.Info{})}
	unread := make([]localapi.Entry, 0, len(in.Entries))
	for _, e := range in.Entries {
		unread = append(unread, localapi.EntryOf(e))
	}
	alerts := []localapi.Alert{}
	for _, a := range as {
		alerts = append(alerts, localapi.AlertOf(a))
	}
	requests := []localapi.Request{}
	for _, q := range qs {
		requests = append(requests, localapi.RequestOf(q))
	}
	out["unread"], out["waiting"], out["alerts"], out["requests"] = unread, len(in.Waiting), alerts, requests
	return out, nil
}

func (sv *server) history(r *http.Request) (any, error) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	return entries(sv.s.History(r.Context(), q.Get("room"), q.Get("before"), limit))
}

func (sv *server) thread(r *http.Request) (any, error) {
	return entries(sv.s.Thread(r.Context(), sv.s.HumanID(), r.URL.Query().Get("id")))
}

func (sv *server) trace(r *http.Request) (any, error) {
	t, err := sv.s.Trace(r.Context(), r.URL.Query().Get("id"))
	return localapi.TraceOf(t), err
}

func (sv *server) send(r *http.Request) (any, error) {
	var in struct {
		Room    string   `json:"room"`
		Text    string   `json:"text"`
		To      []string `json:"to"`
		ReplyTo string   `json:"reply_to"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	m, err := sv.s.Send(r.Context(), node.SendInput{Agent: sv.s.HumanID(), Room: in.Room, Text: in.Text, To: in.To, ReplyTo: in.ReplyTo})
	return localapi.SendResp{ID: m.ID, Room: m.Room, Created: localapi.Time(m.Created)}, err
}

func (sv *server) seen(r *http.Request) (any, error) {
	var in struct {
		Entries []localapi.SeenEntry `json:"entries"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if len(in.Entries) > node.MaxInboxLimit {
		return nil, &node.UserError{Msg: "too many entries"}
	}
	reads := make([]store.Read, 0, len(in.Entries))
	for _, e := range in.Entries {
		reads = append(reads, store.Read{Key: e.Key, Version: e.Version})
	}
	res, err := sv.s.Seen(r.Context(), sv.s.HumanID(), reads)
	out := []localapi.SeenResult{}
	for _, x := range res {
		out = append(out, localapi.SeenResult{Key: x.Key, OK: x.Confirmed, Reason: x.Reason})
	}
	return map[string]any{"results": out}, err
}

func (sv *server) ack(r *http.Request) (any, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	h, err := sv.s.Ack(r.Context(), sv.s.HumanID(), in.ID)
	return localapi.AckResp{Handled: h.ID}, err
}

// confirm is the person's decision on a request (P3). The page sends back
// the text it showed; a request whose text no longer matches is not
// decided, so what is approved is exactly what was named.
func (sv *server) confirm(r *http.Request) (any, error) {
	var in struct {
		ID      string `json:"id"`
		Approve bool   `json:"approve"`
		Text    string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	qs, err := sv.s.Requests(r.Context())
	if err != nil {
		return nil, err
	}
	shown := false
	for _, q := range qs {
		if q.ID == in.ID {
			if node.RequestText(q) != in.Text {
				return nil, &node.UserError{Msg: "the request changed since the page showed it; reload and decide again"}
			}
			shown = true
		}
	}
	if !shown {
		return nil, &node.UserError{Msg: "no such request is waiting"}
	}
	c, err := sv.s.Confirm(r.Context(), in.ID, in.Approve)
	return localapi.ConfirmResp{ID: c.ID, State: c.State, Reason: c.Reason, Entries: c.Entries, Effect: c.Effect}, err
}
