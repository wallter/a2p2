// Package web is the loopback page for the person who runs this machine:
// rooms, members and admins, messages with threads and their files, each
// room's files setting, alerts, and requests waiting for a person with
// approve and decline (DESIGN sections 6, 7 and 12). The person acts as
// the human agent of this machine. Every call is one call into
// node.Surface.
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

//go:embed static
var assets embed.FS

const maxBody = 64 << 10

// runHeader carries the presenting session's run on every API answer.
const runHeader = "A2P2-Run"

type server struct {
	s  *node.Surface
	ss *Sessions
}

// Handler serves the page. Every API request but the trade of a link's
// code must present a session of ss (session.go); see PROTOCOL.md
// section 7.
func Handler(s *node.Surface, ss *Sessions) http.Handler {
	sv := &server{s, ss}
	mux := http.NewServeMux()
	for path, file := range map[string]string{"/": "index.html", "/app.js": "app.js", "/style.css": "style.css"} {
		mux.HandleFunc("GET "+path, sv.static(file))
	}
	// No icon: answered empty, so the browser logs no missing file (an
	// inline data: icon would be refused by the page's own CSP).
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /api/session", sv.session)
	mux.HandleFunc("GET /api/state", sv.api(sv.state))
	mux.HandleFunc("GET /api/history", sv.api(sv.history))
	mux.HandleFunc("GET /api/thread", sv.api(sv.thread))
	mux.HandleFunc("GET /api/trace", sv.api(sv.trace))
	mux.HandleFunc("POST /api/send", sv.api(sv.send))
	mux.HandleFunc("POST /api/seen", sv.api(sv.seen))
	mux.HandleFunc("POST /api/ack", sv.api(sv.ack))
	mux.HandleFunc("POST /api/confirm", sv.api(sv.confirm))
	mux.HandleFunc("POST /api/notice-shown", sv.api(sv.noticeShown))
	mux.HandleFunc("GET /api/file", sv.raw(sv.file))
	mux.HandleFunc("POST /api/room-files", sv.api(sv.roomFiles))
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

// allowed applies the checks every API route shares: a session as the
// Bearer token, and for a POST a JSON content type and the body limit. It
// answers a refusal itself and reports whether the request may go on.
// Every answer to a session carries its run in the A2P2-Run header, so the
// page refuses any answer, a history or a file as well as a state, that
// another listener on the port gives.
func (sv *server) allowed(w http.ResponseWriter, r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	run, valid := "", false
	if ok {
		run, valid = sv.ss.runOf(got)
	}
	if !valid {
		writeJSON(w, 401, map[string]string{"error": "missing or invalid token; run `a2p2 ui` for a fresh link"})
		return false
	}
	w.Header().Set(runHeader, run)
	if r.Method == http.MethodPost {
		if mt, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(strings.ToLower(mt)) != "application/json" {
			writeJSON(w, 415, map[string]string{"error": "content-type must be application/json"})
			return false
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	}
	return true
}

// api wraps a JSON handler with token auth, content-type checks and error mapping.
func (sv *server) api(fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sv.allowed(w, r) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		v, err := fn(r.WithContext(ctx))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, v)
	}
}

// raw is api for a handler that writes its own response (a file's bytes),
// with the same checks. A handler that fails before writing anything
// returns the error, answered as api answers it.
func (sv *server) raw(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sv.allowed(w, r) {
			return
		}
		if err := fn(w, r); err != nil {
			writeErr(w, err)
		}
	}
}

func writeErr(w http.ResponseWriter, err error) {
	var ue *node.UserError
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &ue):
		writeJSON(w, 400, map[string]string{"error": ue.Msg})
	case errors.As(err, &mbe):
		writeJSON(w, 413, map[string]string{"error": "request too large"})
	default:
		writeJSON(w, 500, map[string]string{"error": "internal error"})
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
// and the questions waiting for the person are read without marking
// anything seen, and without keeping a receipt: the page polls this in
// the background and before each change, and may not draw what it gets
// (seen keeps the receipts of what it drew). A one-time notice comes back
// until the page reports it drew it (notice-shown).
func (sv *server) state(r *http.Request) (any, error) {
	ctx := r.Context()
	st, err := sv.s.Status(ctx, true)
	if err != nil {
		return nil, err
	}
	in, err := sv.s.InboxPeek(ctx, sv.s.HumanID(), node.MaxInboxLimit)
	if err != nil {
		return nil, err
	}
	as, err := sv.s.Alerts(ctx, 0)
	if err != nil {
		return nil, err
	}
	qs, err := sv.s.Waiting(ctx)
	if err != nil {
		return nil, err
	}
	questions, err := sv.s.Questions(ctx)
	if err != nil {
		return nil, err
	}
	// run is the presenting session's: the page refuses a state with another.
	run, _ := sv.ss.runOf(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	out := map[string]any{"status": localapi.StatusOf(st, sv.s.Addrs(), localapi.Info{}), "run": run}
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
	asked := make([]localapi.Entry, 0, len(questions))
	for _, e := range questions {
		asked = append(asked, localapi.EntryOf(e))
	}
	out["questions"] = asked
	return out, nil
}

// noticeShown records that the page drew the one-time notice.
func (sv *server) noticeShown(r *http.Request) (any, error) {
	return map[string]any{}, sv.s.NoticeShown(r.Context())
}

func (sv *server) history(r *http.Request) (any, error) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	return entries(sv.s.History(r.Context(), sv.s.HumanID(), q.Get("room"), q.Get("before"), limit))
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
		// ReplyVersion: the version of reply_to's text the page drew; a
		// reply is refused if the message has newer text since.
		ReplyVersion string `json:"reply_version"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	m, err := sv.s.Send(r.Context(), node.SendInput{Agent: sv.s.HumanID(), Room: in.Room, Text: in.Text, To: in.To, ReplyTo: in.ReplyTo,
		ReplyVersion: in.ReplyVersion})
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
	if err := sv.drawn(r.Context(), reads); err != nil {
		return nil, err
	}
	res, err := sv.s.Seen(r.Context(), sv.s.HumanID(), reads)
	out := []localapi.SeenResult{}
	for _, x := range res {
		out = append(out, localapi.SeenResult{Key: x.Key, OK: x.Confirmed, Reason: x.Reason})
	}
	return map[string]any{"results": out}, err
}

// drawn keeps a receipt of each entry the page says it drew, with the
// key and version it drew, that the person's inbox or questions list so
// now: the only receipts the page leaves, so that Seen, and a handled
// report after it, rest on text the page drew (state keeps none).
func (sv *server) drawn(ctx context.Context, reads []store.Read) error {
	want := map[string]string{}
	for _, rd := range reads {
		want[rd.Key] = rd.Version
	}
	in, err := sv.s.InboxPeek(ctx, sv.s.HumanID(), node.MaxInboxLimit)
	if err != nil {
		return err
	}
	qs, err := sv.s.Questions(ctx)
	if err != nil {
		return err
	}
	var es []node.Entry
	kept := map[string]bool{}
	for _, e := range append(in.Entries, qs...) {
		if v, ok := want[e.Key]; ok && v == e.Version && e.Kind != node.EntryRequest && !kept[e.Key] {
			kept[e.Key] = true
			es = append(es, e)
		}
	}
	return sv.s.Listed(ctx, sv.s.HumanID(), es)
}

func (sv *server) ack(r *http.Request) (any, error) {
	var in struct {
		ID string `json:"id"`
		// Version: the version of the message's text the page drew; the
		// report is refused if the message has newer text since.
		Version string `json:"version"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	h, err := sv.s.AckShown(r.Context(), sv.s.HumanID(), in.ID, in.Version)
	return localapi.AckResp{Handled: h.ID}, err
}

// confirm decides a request from the page. The page sends back the text
// it showed, which names every term and the full fingerprint; a request
// whose text no longer matches is not decided, so what is approved is
// exactly what was named. The request must carry this page's exact
// Origin, so that another site the browser opens cannot decide. This
// keeps decisions out of the agents' tools and recorded; it does not prove
// that a person clicked, since a program running as this user can get a
// one-time code from status and trade it for a session.
func (sv *server) confirm(r *http.Request) (any, error) {
	if r.Header.Get("Origin") != "http://"+r.Host {
		return nil, &node.UserError{Msg: "a decision must come from the page itself (its exact Origin)"}
	}
	var in struct {
		ID      string `json:"id"`
		Approve bool   `json:"approve"`
		Text    string `json:"text"`
		// The second approval after a first answered not_caught_up.
		NotCaughtUpOK bool `json:"not_caught_up_ok"`
		// Terms is the digest the page showed, for a send with files.
		Terms string `json:"terms"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	qs, err := sv.s.Waiting(r.Context())
	if err != nil {
		return nil, err
	}
	shown := false
	for _, q := range qs {
		if q.ID == in.ID {
			if q.Text != in.Text {
				return nil, &node.UserError{Msg: node.RequestChangedText}
			}
			shown = true
		}
	}
	if !shown {
		return nil, &node.UserError{Msg: "no such request is waiting"}
	}
	// The text is compared again under the decision lock: a pull between
	// this check and the decision can change the members it names.
	c, err := sv.s.ConfirmShown(r.Context(), in.ID, in.Approve, in.NotCaughtUpOK, in.Terms, in.Text)
	return localapi.ConfirmResp{ID: c.ID, State: c.State, Reason: c.Reason, Entries: c.Entries, Effect: c.Effect}, err
}

// roomFiles reads a room's files setting and, with mode (off, carry or
// keep), first sets it. It is a person's decision, so, as confirm, it
// must carry the page's exact Origin.
func (sv *server) roomFiles(r *http.Request) (any, error) {
	var in struct {
		Room string `json:"room"`
		Mode string `json:"mode"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if in.Mode != "" && r.Header.Get("Origin") != "http://"+r.Host {
		return nil, &node.UserError{Msg: "a files setting must come from the page itself (its exact Origin)"}
	}
	v, err := sv.s.RoomFiles(r.Context(), in.Room, in.Mode)
	return localapi.RoomFilesOf(v), err
}

// fileWait bounds the fetch that file asks for: the bytes of a file not
// here are asked for, and the page is told the state.
const fileWait = 10 * time.Second

// file hands the person one file of a message, fetched as the human agent
// (node.Surface.Fetch, which checks that the human was shown the version
// listing it). Only a file whose state is here is sent, as an attachment
// the browser saves and never renders: application/octet-stream, nosniff,
// a sandboxing CSP and Content-Disposition attachment with the name
// sanitised. Any other state is answered 409 with the fetch's JSON
// (localapi.FetchResp without a path), so the page can say why. What is
// served is a private snapshot of the copy (snapshot), checked against the
// file's size and SHA-256 before a byte is sent, since a copy under files/
// is one a person or program may change; a copy that fails is answered 404.
func (sv *server) file(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	sha := q.Get("sha")
	ctx, cancel := context.WithTimeout(r.Context(), fileWait)
	res, err := sv.s.Fetch(ctx, sv.s.HumanID(), q.Get("room"), sha, q.Get("msg"), 0)
	cancel()
	if err != nil {
		return err
	}
	if res.State != node.FetchHere {
		out := localapi.FetchOf(res)
		out.Path = ""
		writeJSON(w, http.StatusConflict, out)
		return nil
	}
	fileHook("fetched", res.Path)
	snap, n, ok := snapshot(res.Path, res.Size, sha, sv.s.SnapshotFile)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"state": "changed",
			"error": "this machine's copy of the file is not the file verified; fetch it again"})
		return nil
	}
	defer func() {
		snap.Close()
		os.Remove(snap.Name())
	}()
	fileHook("verified", res.Path)
	hd := w.Header()
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("Content-Disposition", "attachment; filename*=UTF-8''"+percentName(safeName(res.Name)))
	hd.Set("Content-Length", strconv.FormatInt(n, 10))
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, snap) // the status is sent; a cut copy is the browser's to discard
	return nil
}

// fileHook lets a test change the copy at a stage of file: "fetched"
// (before it is opened), "opened" (opened, before it is read) and
// "verified" (the snapshot checked, before it is served).
var fileHook = func(stage, path string) {}

// snapshot takes a private copy of the file at path to serve: path is
// opened once, without following a link and without blocking on a FIFO;
// it must be a regular file of exactly size bytes (checked on the open
// handle); at most size+1 bytes are copied into a new private file made by
// create, while they are hashed. Only a copy of size bytes whose SHA-256
// is sha is returned, rewound; anything else is removed. Since the bytes
// served are the snapshot's, a change to path after this cannot reach the
// response. A snapshot never outlives the next start: create puts it
// under the home with a temporary file's name, which a start removes, and
// never in the system's temporary directory.
func snapshot(path string, size int64, sha string, create func() (*os.File, error)) (*os.File, int64, bool) {
	f, err := openFile(path)
	if err != nil {
		return nil, 0, false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() != size {
		return nil, 0, false
	}
	fileHook("opened", path)
	tmp, err := create()
	if err != nil {
		return nil, 0, false
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(f, size+1))
	if err == nil && n == size && hex.EncodeToString(h.Sum(nil)) == sha {
		if _, err = tmp.Seek(0, io.SeekStart); err == nil {
			return tmp, n, true
		}
	}
	tmp.Close()
	os.Remove(tmp.Name())
	return nil, 0, false
}

// safeName is a file's name as a download offers it: no controls, format
// characters (bidirectional marks among them), quotes, slashes or
// backslashes, no leading or trailing dots or spaces, at most 200 bytes.
func safeName(name string) string {
	var b strings.Builder
	for _, c := range name {
		switch {
		case c == utf8.RuneError, unicode.IsControl(c), unicode.Is(unicode.Cf, c),
			strings.ContainsRune("\"'/\\:*?<>|", c):
			b.WriteByte('_')
		default:
			b.WriteRune(c)
		}
	}
	out := strings.Trim(b.String(), ". ")
	for len(out) > 200 {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size]
	}
	if out == "" {
		return "file"
	}
	return out
}

// percentName encodes name for filename* (RFC 8187): every byte outside
// the attr-char set is written %XX.
func percentName(name string) string {
	const attr = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte(attr, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
