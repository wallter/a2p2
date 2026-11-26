package node

import (
	"context"
	"slices"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// RequestView is a request as a person is shown it before deciding: each
// term as its own field, the machine by its full fingerprint, and Text,
// the sentence that says exactly what approving does.
type RequestView struct {
	ID          string
	Kind        string // add, remove, admin or network
	Action      string // admit, admit_admin, promote, remove, rejoin; or off, outbound, on
	Room        string // empty for networking
	RoomLabel   string
	Machine     string // the target's node id, in full
	Fingerprint string // Machine grouped for reading
	Name        string // the target's name; for networking, the address listened on
	// NameFrom says who chose Name: NameFromMachine (the machine asking
	// to join), NameFromAdmin (the admin whose entry this request
	// seconds) or NameFromHere; empty without a name.
	NameFrom string
	// RecordedName is the name the room records for the target, when it
	// is a member: compare it with Name.
	RecordedName string
	Role         string
	History      bool
	Seconding    bool // matches an entry another admin already signed
	Created      time.Time
	Deadline     time.Time // the invite's expiry, for a request an invite raised
	Text         string
	// Send is a send with files (kind store.RequestSendFiles), every term
	// as the person approves it.
	Send *SendView
}

// Describe renders q for a person.
func (s *Surface) Describe(ctx context.Context, q store.Request) (RequestView, error) {
	label := ""
	var members []MemberInfo
	if q.Room != "" {
		lr, err := s.m.st.LogRoom(ctx, q.Room)
		if err != nil {
			return RequestView{}, err
		}
		label = shownLabel(s.m.ID, q.Room, lr.Label) // as Alerts shows it, and ConfirmWith compares it
		if s.r != nil && q.Kind != store.RequestSendFiles {
			members = s.r.memberNames(q.Room)
		}
	}
	// Name is kept as stored, for programs, and RoomLabel as this machine
	// shows it (ShownLabel); Text, which a person approves, escapes them
	// (requestText).
	v := RequestView{ID: q.ID, Kind: q.Kind, Action: q.Action, Room: q.Room, RoomLabel: label, Machine: q.Node,
		Fingerprint: Fingerprint(q.Node), Name: q.Name, NameFrom: nameFrom(q), RecordedName: recordedName(members, q.Node),
		Role: q.Role, History: q.History, Seconding: q.Seconds != "",
		Created: q.Created, Deadline: q.Deadline, Text: requestText(q, label, members)}
	if q.Kind == store.RequestSendFiles {
		v.Machine, v.Fingerprint = "", ""
		v.Deadline = q.Created.Add(core.StagingLife)
		if err := s.describeSend(ctx, q, &v); err != nil {
			return RequestView{}, err
		}
	}
	return v, nil
}

// Waiting lists the requests waiting for this machine's person, rendered.
func (s *Surface) Waiting(ctx context.Context) ([]RequestView, error) {
	qs, err := s.r.Requests(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RequestView, 0, len(qs))
	for _, q := range qs {
		v, err := s.Describe(ctx, q)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Questions lists what agents asked this machine's person and the person
// has not yet confirmed: unread messages addressed to the human by name, or
// carrying choices; a replacement of one is a question too, with its new
// text. Unlike the inbox it is not capped by older mail, so a
// new question is never buried; the page confirms one only after it is
// answered or dismissed.
func (s *Surface) Questions(ctx context.Context) ([]Entry, error) {
	rs, err := s.rooms(ctx)
	if err != nil {
		return nil, err
	}
	human := core.Addressee{Node: s.m.ID, Agent: HumanLabel}
	var out []Entry
	for _, r := range rs {
		v, err := s.m.Inbox(ctx, r.ID, HumanLabel)
		if err != nil {
			return nil, err
		}
		for _, e := range v.Entries {
			asked := e.Kind == EntryMessage || e.Kind == EntryReplaced
			if asked && e.From != s.HumanID() && (len(e.Choices) > 0 || slices.Contains(e.To, human)) {
				out = append(out, e)
			}
		}
	}
	sortEntries(out)
	return out, nil
}
