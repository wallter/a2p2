// Package localapi is the daemon's API for processes on the same machine:
// JSON over HTTP over a Unix socket (PROTOCOL section 7). Access to
// the socket is the only credential; see "Trust and access" in SECURITY.md.
// Every call is one call into node.Surface.
package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// maxBody bounds a request body.
const maxBody = 256 << 10

// Time renders a time as PROTOCOL section 0 does; zero is empty.
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
		// Thread, with reply_to and no to: also address every agent that
		// has posted in the thread (the reply tool).
		Thread bool `json:"thread,omitempty"`
		// Files (1 to 8): absolute paths, or "sha256:<hex>" for a file of
		// the version replaced, kept by listing it again. With files
		// nothing is written: a request waits for this machine's person
		// and the answer is a SendFilesResp (PROTOCOL 10.6). With
		// replaces, no files means the replacement has none.
		Files []string `json:"files,omitempty"`
	}
	// SendFilesResp is the answer to a send with files: only the request
	// a person decides and its state, waiting. No size or hash of what
	// was staged is told to the agent that asked (the person sees them).
	SendFilesResp struct {
		Request string `json:"request"`
		State   string `json:"state"`
	}
	// FileInfo is one file of a send or a message.
	FileInfo struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		Type   string `json:"type,omitempty"`
	}
	// FetchReq asks for the bytes of one file of a message this agent was
	// shown (PROTOCOL 10.6). wait_ms is at most 120,000.
	FetchReq struct {
		Agent   string `json:"agent"`
		Room    string `json:"room,omitempty"`
		SHA256  string `json:"sha256"`
		Message string `json:"message,omitempty"`
		WaitMS  int64  `json:"wait_ms,omitempty"`
	}
	// FetchResp: state is here (with path), partial, coming, fetching,
	// unavailable, gone (with reason) or too_large. The file at path is
	// data from another machine; a2p2 never opens it.
	FetchResp struct {
		State   string   `json:"state"`
		Room    string   `json:"room,omitempty"`
		Message string   `json:"message,omitempty"`
		Name    string   `json:"name,omitempty"`
		Path    string   `json:"path,omitempty"`
		Size    int64    `json:"size"`
		Have    int64    `json:"have,omitempty"`
		From    string   `json:"from,omitempty"`
		Reason  string   `json:"reason,omitempty"`
		Holders []Holder `json:"holders,omitempty"`
	}
	// Holder is one member's last answer about a file, at the time this
	// machine observed it.
	Holder struct {
		Node   string `json:"node"`
		At     string `json:"at"`
		Answer string `json:"answer"`
	}
	// RoomFilesReq reads a room's files setting and, with mode (off, carry
	// or keep), first sets it: a person's decision, in no agent tool.
	RoomFilesReq struct {
		Room string `json:"room"`
		Mode string `json:"mode,omitempty"`
	}
	// RoomFiles: held is the bytes the room's files take here (held,
	// partial, staged, copied and reserved), budget the room's limit.
	RoomFiles struct {
		Room   string `json:"room"`
		Mode   string `json:"mode"`
		Held   int64  `json:"held"`
		Budget int64  `json:"budget"`
	}
	// FilesUsage is what files take on this machine, and its limit;
	// unknown counts files under blobs/ with a name a2p2 does not
	// generate, which the last start left alone.
	FilesUsage struct {
		Held    int64 `json:"held"`
		Budget  int64 `json:"budget"`
		Unknown int64 `json:"unknown,omitempty"`
	}
	// SendResp: to is the message's addressees as written, empty for
	// everyone (and for a replace or withdraw, which keep the original's).
	SendResp struct {
		ID        string   `json:"id"`
		Room      string   `json:"room"`
		RoomLabel string   `json:"room_label"`
		Created   string   `json:"created"`
		To        []string `json:"to,omitempty"`
		// ToOmitted: thread participants left out at the addressee limit.
		ToOmitted int `json:"to_omitted,omitempty"`
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
		RoomLabel     string   `json:"room_label"`
		From          string   `json:"from"`
		FromNode      string   `json:"from_node"`
		AuthorRemoved bool     `json:"author_removed,omitempty"`
		To            []string `json:"to,omitempty"`
		// AskHandled: the post asked for handled reports.
		AskHandled  bool     `json:"ask_handled,omitempty"`
		Parent      string   `json:"parent,omitempty"`
		Thread      string   `json:"thread,omitempty"`
		ParentState string   `json:"parent_state,omitempty"`
		Late        bool     `json:"late,omitempty"`
		Created     string   `json:"created"`
		Text        string   `json:"text,omitempty"`
		Choices     []string `json:"choices,omitempty"`
		// Skipped: in a history page read by an agent, its level kept
		// this message out of its inbox and it has not confirmed it.
		Skipped bool `json:"skipped,omitempty"`
		// Files are the files of the version shown, with this machine's
		// state of each (here, not_fetched, partial, coming, unavailable);
		// files_pending: it has files whose list is not here yet;
		// files_note: why the list will not come (PROTOCOL 10.2).
		Files        []EntryFile `json:"files,omitempty"`
		FilesPending bool        `json:"files_pending,omitempty"`
		FilesNote    string      `json:"files_note,omitempty"`
		// Request and State, in an entry of kind request: this agent's
		// send with files was sent (id is the message) or declined
		// (reason says why).
		Request string `json:"request,omitempty"`
		State   string `json:"state,omitempty"`
		Reason  string `json:"reason,omitempty"`
	}
	EntryFile struct {
		SHA256 string `json:"sha256"`
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		Type   string `json:"type,omitempty"`
		State  string `json:"state"`
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
	// ThreadReq: after, a message id in the thread, returns only what
	// follows it.
	ThreadReq struct {
		Agent string `json:"agent"`
		ID    string `json:"id"`
		After string `json:"after,omitempty"`
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
	// AckResp: handled is the report's id; already, that this agent had
	// reported it before, and handled names that first report.
	AckResp struct {
		Handled string `json:"handled"`
		Already bool   `json:"already,omitempty"`
	}
	// HistoryReq: the room's last limit messages before message before.
	// With agent, entries its level kept out of its inbox are marked
	// skipped, and it may report one whose text was shown handled. With
	// skipped, only agent's messages its level kept out of its inbox and it
	// has not confirmed (U3), in room or in every room when room is empty,
	// paged the same way. It marks nothing seen.
	HistoryReq struct {
		Room    string `json:"room,omitempty"`
		Before  string `json:"before,omitempty"`
		Limit   int    `json:"limit,omitempty"`
		Agent   string `json:"agent,omitempty"`
		Skipped bool   `json:"skipped,omitempty"`
	}
	// LevelReq reads agent's reading level in room (every room when room
	// is empty) and, with level, first sets it: all, addressed or none
	// (U1, U2).
	LevelReq struct {
		Agent string `json:"agent"`
		Room  string `json:"room,omitempty"`
		Level string `json:"level,omitempty"`
	}
	RoomLevel struct {
		Room      string `json:"room"`
		RoomLabel string `json:"room_label"`
		Level     string `json:"level"`
	}
	LevelResp struct {
		Levels []RoomLevel `json:"levels"`
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
		// Files are the files of the winning version, with what this
		// machine holds of each and each member's last answer.
		Files []TraceFile `json:"files,omitempty"`
	}
	TraceFile struct {
		SHA256  string   `json:"sha256"`
		Name    string   `json:"name"`
		Size    int64    `json:"size"`
		State   string   `json:"state"`
		Holders []Holder `json:"holders"`
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
		// NameFrom says who chose name: machine (the machine asking to
		// join), admin (the admin whose entry this seconds) or here.
		// RecordedName is the name the room records for the machine.
		NameFrom     string `json:"name_from,omitempty"`
		RecordedName string `json:"recorded_name,omitempty"`
		Role         string `json:"role,omitempty"`
		History      bool   `json:"history"`
		Seconding    bool   `json:"seconding,omitempty"`
		Created      string `json:"created"`
		Deadline     string `json:"deadline,omitempty"`
		// Text says exactly what approving it does.
		Text string `json:"text"`
		// For kind send_files (PROTOCOL 10.6): the agent that asked, the
		// effective recipients (to, or ["everyone"] with members, the
		// room's member count; for a replacement, its message's), the
		// parent and, for a reply to its author, that author, the message
		// and version replaced, the first line and the whole text (body,
		// each line escaped), the files with each path as resolved from
		// the file opened and escaped (10.8), and terms, the SHA-256 the
		// approval covers, which confirm must send back.
		Agent       string        `json:"agent,omitempty"`
		To          []string      `json:"to,omitempty"`
		Members     int           `json:"members,omitempty"`
		ReplyTo     string        `json:"reply_to,omitempty"`
		ReplyAuthor string        `json:"reply_author,omitempty"`
		Replaces    *Replaced     `json:"replaces,omitempty"`
		TextLine    string        `json:"text_line,omitempty"`
		Body        string        `json:"body,omitempty"`
		Files       []RequestFile `json:"files,omitempty"`
		Terms       string        `json:"terms,omitempty"`
	}
	Replaced struct {
		ID      string `json:"id"`
		Version string `json:"version,omitempty"`
		Line    string `json:"line"`
	}
	RequestFile struct {
		Path   string `json:"path"`
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		Type   string `json:"type,omitempty"`
		// Kept: kept from the version replaced (path is sha256:<hex>).
		Kept bool `json:"kept,omitempty"`
	}
	RequestsResp struct {
		Requests []Request `json:"requests"`
	}
	ConfirmReq struct {
		ID      string `json:"id"`
		Approve bool   `json:"approve"`
		// NotCaughtUpOK is the person's second approval after a first
		// answered reason not_caught_up; it is recorded with the request.
		NotCaughtUpOK bool `json:"not_caught_up_ok,omitempty"`
		// Terms is the terms digest the person was shown; an approval of
		// a send_files request needs it equal to the request's terms.
		Terms string `json:"terms,omitempty"`
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
	// PreviewResp is the previewed answer of PROTOCOL 5.3, with the
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
		// MapPort: with on, ask the router to forward the listening port
		// (a2p2 net on --map-port), a decision like the rest of it.
		MapPort bool `json:"map_port,omitempty"`
	}
	// Network is a networking decision. In status it is the latest one;
	// Applied and Reason are set only when what runs differs from it: the
	// state in effect (outbound when on cannot listen) and why. NetResp is
	// the decision recorded; the daemon applies it at once, without a
	// restart.
	Network struct {
		State   string `json:"state"`
		Listen  string `json:"listen,omitempty"`
		Request string `json:"request,omitempty"`
		MapPort bool   `json:"map_port,omitempty"`
		// IPv4Only: a decision from before IPv6 (DESIGN 11): IPv4 only until decided again.
		IPv4Only bool   `json:"ipv4_only,omitempty"`
		Applied  string `json:"applied,omitempty"`
		Reason   string `json:"reason,omitempty"`
	}
	NetResp struct {
		Network
		Text    string `json:"text"`
		Applies string `json:"applies"`
	}
	StatusReq struct {
		// ShowNotice: a person is shown this status, so a one-time notice
		// still to be shown is returned (not recorded as shown).
		ShowNotice bool `json:"show_notice,omitempty"`
		// NoticeShown: the caller printed or drew the notice; it is
		// recorded as shown before the status is read.
		NoticeShown bool `json:"notice_shown,omitempty"`
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
		// MayBeRemoved: every other member has kept refusing this machine
		// as not a member (node.MaybeRemovedAfter); a display rule only.
		MayBeRemoved bool `json:"may_be_removed,omitempty"`
		// Agents are the agents of members seen writing here, newest
		// first (status only).
		Agents []Writer `json:"agents,omitempty"`
		// Files is the room's files setting and use (status only).
		Files *RoomFiles `json:"files,omitempty"`
	}
	Writer struct {
		Agent    string `json:"agent"`
		LastSeen string `json:"last_seen"`
	}
	RoomsResp struct {
		Rooms []Room `json:"rooms"`
	}
	// UnreadReq is what a harness hook knows about itself; Lines asks for
	// first lines too (U4).
	// An agent's tools name the agent instead.
	UnreadReq struct {
		Harness string `json:"harness"`
		Cwd     string `json:"cwd"`
		Lines   bool   `json:"lines,omitempty"`
		Agent   string `json:"agent,omitempty"`
		// Room: only this room (label or id).
		Room string `json:"room,omitempty"`
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
		// Levels are the local agents' reading levels other than the
		// default: an agent and room not listed is at "all".
		Levels []store.AgentLevel `json:"levels"`
		// Reach is whether other members can reach this machine (yes, no
		// or unknown), checked by asking them to dial back (PROTOCOL 4.7).
		Reach node.Reach `json:"reach"`
		// Mapping is the router's port mapping, when a person asked for one.
		Mapping *node.PortMapping `json:"mapping,omitempty"`
		// Files is what attachments take on this machine (each room's is
		// in its files).
		Files FilesUsage `json:"files"`
		// Quarantined are the rooms set aside because their stored log
		// does not load in this version: not served or pulled, nothing
		// deleted. Absent when there are none.
		Quarantined []Quarantined `json:"quarantined,omitempty"`
	}
	// Quarantined is one room set aside (node.Quarantine).
	Quarantined struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Reason string `json:"reason"`
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

// apiError is every refusal: the sentence, a code from node.ErrorCodes and
// whether repeating the same call later may succeed (PROTOCOL 7).
type apiError struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

func refusal(msg, code string) apiError { return apiError{msg, code, node.Retryable(code)} }

func handle[Req, Resp any](mux *http.ServeMux, name string, fn func(context.Context, Req) (Resp, error)) {
	mux.HandleFunc("POST /v1/"+name, func(w http.ResponseWriter, r *http.Request) {
		var req Req
		body := http.MaxBytesReader(w, r.Body, maxBody)
		if err := json.NewDecoder(body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, refusal("bad request: "+err.Error(), node.ErrorInvalid))
			return
		}
		resp, err := fn(r.Context(), req)
		var ue *node.UserError
		switch {
		case errors.As(err, &ue):
			writeJSON(w, http.StatusBadRequest, refusal(ue.Msg, ue.CodeOf()))
		case err != nil:
			msg := scrub(err)
			slog.Error("local api", "call", name, "err", msg)
			writeJSON(w, http.StatusInternalServerError, refusal("internal error: "+msg, node.ErrorInternal))
		default:
			writeJSON(w, http.StatusOK, resp)
		}
	})
}

// scrub is an internal error as the log and the answer give it: a file
// system error is only its operation and its cause (as "open: permission
// denied"), never the path it was about, which may name a person's file.
func scrub(err error) string {
	var pe *fs.PathError
	var le *os.LinkError
	var se *os.SyscallError
	switch {
	case errors.As(err, &pe):
		return pe.Op + ": " + pe.Err.Error()
	case errors.As(err, &le):
		return le.Op + ": " + le.Err.Error()
	case errors.As(err, &se):
		return se.Syscall + ": " + se.Err.Error()
	}
	return err.Error()
}

// writeJSON writes v as one line of JSON. Text is written as it is: '<',
// '>' and '&' are not escaped, since no reader treats it as markup.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func userErr(msg string) error { return &node.UserError{Msg: msg} }

// waitOf checks a wait in milliseconds (at most 120,000).
func waitOf(ms int64) (time.Duration, error) {
	if ms < 0 || ms > node.MaxWait.Milliseconds() {
		return 0, userErr("wait_ms is 0 to 120000")
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// EntryOf renders a node entry (PROTOCOL section 7, Entry).
func EntryOf(e node.Entry) Entry {
	out := Entry{Key: e.Key, Version: e.Version, Kind: e.Kind, ID: e.ID, Room: e.Room, RoomLabel: e.RoomLabel,
		From: e.From, FromNode: e.FromNode, AuthorRemoved: e.AuthorRemoved, Parent: e.Parent, Thread: e.Thread,
		ParentState: e.ParentState, Late: e.Late, Created: Time(e.Created), Text: e.Text, Choices: e.Choices,
		AskHandled: e.AskHandled, Skipped: e.Skipped, FilesPending: e.FilesPending, FilesNote: e.FilesNote,
		Request: e.Request, State: e.RequestState, Reason: e.RequestReason}
	for _, a := range e.To {
		out.To = append(out.To, node.AgentID(a))
	}
	for _, f := range e.Files {
		out.Files = append(out.Files, EntryFile{SHA256: f.SHA256, Name: f.Name, Size: f.Size, Type: f.Type, State: f.State})
	}
	return out
}

// FetchOf renders a fetch's answer.
func FetchOf(r node.FetchResult) FetchResp {
	out := FetchResp{State: r.State, Room: r.Room, Message: r.Message, Name: r.Name, Path: r.Path, Size: r.Size,
		Have: r.Have, From: r.From, Reason: r.Reason}
	for _, h := range r.Holders {
		out.Holders = append(out.Holders, Holder{Node: h.Node, At: h.At, Answer: h.Answer})
	}
	return out
}

// RoomFilesOf renders a room's files setting.
func RoomFilesOf(v node.RoomFilesView) RoomFiles {
	return RoomFiles{Room: v.Room, Mode: v.Mode, Held: v.Held, Budget: v.Budget}
}

// SendFilesOf renders the answer to a send with files.
func SendFilesOf(r node.FileSent) SendFilesResp {
	return SendFilesResp{Request: r.Request, State: r.State}
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
		NoEffect: []NoEffect{}, Forks: []Fork{}, Limits: append([]string{}, r.Limits...), Waiting: len(r.Requests),
		MayBeRemoved: r.MayBeRemoved}
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
	out := Request{ID: q.ID, Kind: q.Kind, Action: q.Action, Room: q.Room, RoomLabel: q.RoomLabel, Node: q.Machine,
		Fingerprint: q.Fingerprint, Name: q.Name, NameFrom: q.NameFrom, RecordedName: q.RecordedName, Role: q.Role, History: q.History, Seconding: q.Seconding,
		Created: Time(q.Created), Deadline: Time(q.Deadline), Text: q.Text}
	if s := q.Send; s != nil {
		out.Agent, out.To, out.ReplyTo, out.TextLine, out.Terms = s.Agent, s.To, s.Parent, s.TextLine, s.Terms
		out.ReplyAuthor, out.Body = s.ParentAuthor, s.Body
		if s.Everyone {
			out.To, out.Members = []string{node.Everyone}, s.Members
		}
		if s.Replaces != "" {
			out.Replaces = &Replaced{ID: s.Replaces, Version: s.Base, Line: s.ReplacesLine}
		}
		for _, f := range s.Files {
			out.Files = append(out.Files, RequestFile{Path: f.Path, Name: f.Name, Size: f.Size, SHA256: f.SHA256, Type: f.Type, Kept: f.Kept})
		}
	}
	return out
}

// NetworkOf renders a networking state.
func NetworkOf(n node.Network) Network {
	return Network{State: n.State, Listen: n.Listen, Request: n.Request, MapPort: n.MapPort, IPv4Only: n.IPv4Only}
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
	for _, f := range t.Files {
		tf := TraceFile{SHA256: f.SHA256, Name: f.Name, Size: f.Size, State: f.State, Holders: []Holder{}}
		for _, h := range f.Holders {
			tf.Holders = append(tf.Holders, Holder{Node: h.Node, At: h.At, Answer: h.Answer})
		}
		out.Files = append(out.Files, tf)
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
		Network: statusNetwork(st), Waiting: st.Waiting, Levels: st.Levels, Reach: st.Reach,
		Mapping: st.Mapping}
	if out.Levels == nil {
		out.Levels = []store.AgentLevel{}
	}
	out.Files = FilesUsage{Held: st.Files.Held, Budget: st.Files.Budget, Unknown: st.Files.Unknown}
	for _, q := range st.Quarantined {
		out.Quarantined = append(out.Quarantined, Quarantined{ID: q.Room, Label: q.Label, Reason: q.Reason})
	}
	for i := range out.Rooms {
		for _, w := range st.Writers[out.Rooms[i].ID] {
			out.Rooms[i].Agents = append(out.Rooms[i].Agents, Writer{Agent: w.Agent, LastSeen: Time(w.Last)})
		}
		if f, ok := st.RoomFiles[out.Rooms[i].ID]; ok {
			rf := RoomFilesOf(f)
			out.Rooms[i].Files = &rf
		}
	}
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

// statusNetwork is the latest decision, with what is applied when it
// differs.
func statusNetwork(st node.StatusView) Network {
	n := NetworkOf(st.Network)
	switch {
	case st.Decided.State != "" && st.Decided.Request != st.Network.Request:
		n = NetworkOf(st.Decided)
		n.Applied, n.Reason = st.Network.State, "being applied"
	case st.Fault != "" && st.Network.State == node.NetworkOn:
		n.Applied, n.Reason = node.NetworkOutbound, st.Fault
	}
	return n
}

// Handler serves the local API over s.
func Handler(s *node.Surface, info Info) http.Handler {
	mux := http.NewServeMux()
	handle(mux, "register", func(ctx context.Context, r RegisterReq) (store.Agent, error) {
		return s.Register(ctx, r.Harness, r.Label, r.Cwd)
	})
	handle(mux, "send", func(ctx context.Context, r SendReq) (any, error) {
		in := node.SendInput{Agent: r.Agent, Room: r.Room, Text: r.Text, To: r.To, ReplyTo: r.ReplyTo,
			AskHandled: r.AskHandled, Replaces: r.Replaces, Choices: r.Choices, Thread: r.Thread}
		if len(r.Files) > 0 {
			f, err := s.SendFiles(ctx, in, r.Files)
			return SendFilesOf(f), err
		}
		m, err := s.Send(ctx, in)
		out := SendResp{ID: m.ID, Room: m.Room, RoomLabel: m.Label, Created: Time(m.Created), ToOmitted: m.ToOmitted}
		for _, a := range m.To {
			out.To = append(out.To, node.AgentID(a))
		}
		return out, err
	})
	// fetch is an agent's request for one file's bytes, copied for it
	// under the daemon's home and never opened (PROTOCOL 10.6).
	handle(mux, "fetch", func(ctx context.Context, r FetchReq) (FetchResp, error) {
		if r.WaitMS < 0 || r.WaitMS > core.MaxFetchWaitMS {
			return FetchResp{}, userErr("wait_ms is 0 to 120000")
		}
		res, err := s.Fetch(ctx, r.Agent, r.Room, r.SHA256, r.Message, time.Duration(r.WaitMS)*time.Millisecond)
		return FetchOf(res), err
	})
	// room/files is a person's setting for a room's files: off, carry or
	// keep. Like net, no agent tool or hook calls it.
	handle(mux, "room/files", func(ctx context.Context, r RoomFilesReq) (RoomFiles, error) {
		v, err := s.RoomFiles(ctx, r.Room, r.Mode)
		return RoomFilesOf(v), err
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
		es, err := s.ThreadAfter(ctx, r.Agent, r.ID, r.After)
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
		return AckResp{Handled: h.ID, Already: h.Already}, err
	})
	handle(mux, "history", func(ctx context.Context, r HistoryReq) (Entries, error) {
		if r.Skipped {
			es, err := s.Skipped(ctx, r.Agent, r.Room, r.Before, r.Limit)
			return Entries{entriesOf(es)}, err
		}
		es, err := s.History(ctx, r.Agent, r.Room, r.Before, r.Limit)
		return Entries{entriesOf(es)}, err
	})
	handle(mux, "level", func(ctx context.Context, r LevelReq) (LevelResp, error) {
		ls, err := s.Levels(ctx, r.Agent, r.Room, r.Level)
		out := LevelResp{Levels: []RoomLevel{}}
		for _, l := range ls {
			out.Levels = append(out.Levels, RoomLevel{Room: l.Room, RoomLabel: l.Label, Level: l.Level})
		}
		return out, err
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
		c, err := s.Confirm(ctx, r.ID, r.Approve, r.NotCaughtUpOK, r.Terms)
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
			code := node.ErrorInvalid
			if ref.Code == node.CodeRateLimited {
				code = node.ErrorRateLimited
			}
			err = &node.UserError{Msg: ref.Error(), Code: code}
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
		id := r.Agent
		if id == "" {
			a, ok, err := s.AgentFor(ctx, core.CleanLabel(r.Harness), r.Cwd)
			if err != nil || !ok {
				return Unread{}, err
			}
			id = a.ID
		}
		u, err := s.Unread(ctx, id, r.Room, r.Lines)
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
		if r.NoticeShown {
			if err := s.NoticeShown(ctx); err != nil {
				return Status{}, err
			}
		}
		st, err := s.Status(ctx, r.ShowNotice)
		return StatusOf(st, s.Addrs(), info), err
	})
	// net records a person's networking decision, with the authority of
	// confirm: the CLI asks for it at a terminal and no agent tool or hook
	// calls it. That keeps it out of the agents' tools and recorded; it
	// does not prove that a person made it, since any program running as
	// this user can call this socket.
	handle(mux, "net", func(ctx context.Context, r NetReq) (NetResp, error) {
		n, err := s.SetNetwork(ctx, node.Network{State: r.State, Listen: r.Listen, MapPort: r.MapPort})
		return NetResp{Network: NetworkOf(n), Text: node.NetworkText(n), Applies: "now: the daemon applies it at once"}, err
	})
	// net/check tries every member's addresses now and asks those reached
	// to dial back (a2p2 net check); it dials nothing while networking is
	// off and decides nothing.
	handle(mux, "net/check", func(ctx context.Context, _ Empty) (node.NetCheck, error) {
		return s.NetCheck(ctx)
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
	hc  *http.Client
	key func() string
}

// KeyHeader carries the local key, on Windows: the daemon writes a fresh
// key into its home at each start and refuses a request without it.
const KeyHeader = "A2P2-Local-Key"

// WithKey makes c present key() on every call, when it is not empty.
func (c *Client) WithKey(key func() string) *Client {
	c.key = key
	return c
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
// asked for something invalid (as opposed to the daemon breaking). Code is
// one of node.ErrorCodes and Retryable whether the same call may succeed
// later; a daemon that sent no code gets invalid_request for a 400 and
// internal otherwise.
type Error struct {
	Msg       string
	User      bool
	Code      string
	Retryable bool
}

func (e *Error) Error() string { return e.Msg }

// Refusal is how a client reports a failed call: the sentence, its code and
// whether retrying can help. An error that is not an *Error means the
// daemon was not reached (unavailable, retryable).
type Refusal struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

// RefusalOf describes err as a Refusal.
func RefusalOf(err error) Refusal {
	var e *Error
	if errors.As(err, &e) {
		return Refusal{e.Msg, e.Code, e.Retryable}
	}
	return Refusal{"the a2p2 daemon is unavailable: " + err.Error(), node.ErrorUnavailable, true}
}

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
	if c.key != nil {
		if k := c.key(); k != "" {
			hr.Header.Set(KeyHeader, k)
		}
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
		user := res.StatusCode == http.StatusBadRequest
		if ae.Code == "" { // a daemon from before the codes
			ae.Code = node.ErrorInternal
			if user {
				ae.Code = node.ErrorInvalid
			}
			ae.Retryable = node.Retryable(ae.Code)
		}
		return nil, &Error{Msg: ae.Error, User: user, Code: ae.Code, Retryable: ae.Retryable}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil || resp == nil {
		return raw, err
	}
	return raw, json.Unmarshal(raw, resp)
}
