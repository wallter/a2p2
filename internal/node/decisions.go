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
	Role        string
	History     bool
	Seconding   bool // matches an entry another admin already signed
	Created     time.Time
	Deadline    time.Time // the invite's expiry, for a request an invite raised
	Text        string
}

// Describe renders q for a person.
func (s *Surface) Describe(ctx context.Context, q store.Request) (RequestView, error) {
	label := ""
	if q.Room != "" {
		lr, err := s.m.st.LogRoom(ctx, q.Room)
		if err != nil {
			return RequestView{}, err
		}
		label = lr.Label
	}
	return RequestView{ID: q.ID, Kind: q.Kind, Action: q.Action, Room: q.Room, RoomLabel: label, Machine: q.Node,
		Fingerprint: Fingerprint(q.Node), Name: q.Name, Role: q.Role, History: q.History, Seconding: q.Seconds != "",
		Created: q.Created, Deadline: q.Deadline, Text: requestText(q, label)}, nil
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
// carrying choices. Unlike the inbox it is not capped by older mail, so a
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
			if e.Kind == EntryMessage && e.From != s.HumanID() && (len(e.Choices) > 0 || slices.Contains(e.To, human)) {
				out = append(out, e)
			}
		}
	}
	sortEntries(out)
	return out, nil
}
