package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// leave stops carrying a room here and deletes its data on this machine,
// which nothing undoes: joining again needs a new invite. So it is a
// person's decision, asked at a terminal as room files and net are, and
// it says when this machine is the room's only admin or only member. The
// room is named to the daemon by the id the prompt showed, never by the
// label typed, so a room that appears under that label while the prompt
// waits is not the one left.
func leave(c *cmd, room string) error {
	if !terminal() {
		return errors.New("run this room leave command in a terminal: it deletes the room's data on this machine")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	var st localapi.Status
	if err := cl.Call(c.ctx, "status", localapi.StatusReq{}, &st); err != nil {
		return err
	}
	var target string
	if q, ok, err := setAside(st, room); err != nil {
		return err
	} else if ok {
		// A room set aside does not list, and leaving is how it is mended.
		target = q.ID
		fmt.Fprintln(os.Stderr, approvalText(fmt.Sprintf("Leave room %s (%s), set aside because its log does not load here? "+
			"Its messages and files on this machine are deleted. Joining again needs a new invite.",
			node.Isolate(node.EscapeDisplay(q.Label)), node.EscapeDisplay(q.ID)), "  "))
	} else {
		var rs localapi.RoomsResp
		if err := cl.Call(c.ctx, "room/list", localapi.RoomListReq{Room: room}, &rs); err != nil {
			return err
		}
		if len(rs.Rooms) != 1 {
			return fmt.Errorf("room %s: the daemon listed %d rooms; name one by its id", node.EscapeDisplay(room), len(rs.Rooms))
		}
		r := rs.Rooms[0]
		if r.ID == st.Local {
			return errors.New("the local room cannot be left")
		}
		target = r.ID
		leaveWarning(os.Stderr, r, st.Node)
	}
	if ask("Leave it? Type yes: ") != "yes" {
		return errors.New("nothing changed")
	}
	return c.call("room/leave", localapi.RoomReq{Room: target}, &localapi.Empty{}, func() {
		fmt.Println("left; its data is deleted here. Other members list this machine until an admin removes it.")
	})
}

// setAside is the room set aside that name (an id or a label) names. A
// label that names a set-aside room and another room is refused, so the
// person names the one meant by its id.
func setAside(st localapi.Status, name string) (localapi.Quarantined, bool, error) {
	qs := st.Quarantined
	for _, q := range qs {
		if q.ID == name {
			return q, true, nil
		}
	}
	var hit []localapi.Quarantined
	for _, q := range qs {
		if q.Label == name {
			hit = append(hit, q)
		}
	}
	live := slices.ContainsFunc(st.Rooms, func(r localapi.Room) bool { return r.Label == name })
	if len(hit) == 1 && !live {
		return hit[0], true, nil
	}
	if len(hit) > 0 {
		return localapi.Quarantined{}, false, fmt.Errorf("several rooms are labelled %s, one of them set aside: name one by its id", node.EscapeDisplay(name))
	}
	return localapi.Quarantined{}, false, nil
}

// leaveWarning says what leaving room r costs, for machine self.
func leaveWarning(w io.Writer, r localapi.Room, self string) {
	fmt.Fprintln(w, approvalText(fmt.Sprintf("Leave room %s (%s)? Its messages and files on this machine are deleted. Joining again needs a new invite.",
		node.Isolate(node.EscapeDisplay(r.Label)), node.EscapeDisplay(r.ID)), "  "))
	switch {
	case len(r.Members) <= 1:
		fmt.Fprintln(w, "This machine is the room's only member: its messages are lost for good.")
	case len(r.Admins) == 1 && slices.Contains(r.Admins, self):
		fmt.Fprintln(w, "This machine is the room's only admin. After it leaves, nobody can add or remove members, "+
			"and nobody can invite this machine back. Make another member an admin first with a2p2 room admin.")
	}
}
