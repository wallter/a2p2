package members

import (
	"reflect"
	"sort"
	"testing"
)

// room builds histories for scenario tests. Entries are created with the
// author's next sequence number and previous entry filled in, and with the
// parents the test names ("what the author had seen").
type room struct {
	t    testing.TB
	byID map[string]Entry
	last map[string]Entry
	root string
}

func newRoom(t testing.TB, creator string) *room {
	r := &room{t: t, byID: map[string]Entry{}, last: map[string]Entry{}}
	r.root = r.put(Entry{Author: creator, Seq: 1, Kind: Create})
	return r
}

func (r *room) put(e Entry) string {
	e = New(e)
	r.byID[e.ID] = e
	if cur, ok := r.last[e.Author]; !ok || e.Seq > cur.Seq {
		r.last[e.Author] = e
	}
	return e.ID
}

// next signs the author's next entry, having seen parents (plus its own
// previous entry, which is always added).
func (r *room) next(author string, kind Kind, target string, parents ...string) string {
	e := Entry{Author: author, Kind: kind, Target: target, Seq: 1}
	if p, ok := r.last[author]; ok {
		e.Seq, e.Prev = p.Seq+1, p.ID
		parents = append(parents, p.ID)
	}
	if len(parents) == 0 {
		parents = []string{r.root}
	}
	e.Parents = uniq(parents)
	return r.put(e)
}

func (r *room) add(author, target string, seen ...string) string {
	return r.next(author, Add, target, seen...)
}

func (r *room) admin(author, target string, seen ...string) string {
	return r.next(author, MakeAdmin, target, seen...)
}

func (r *room) remove(author, target string, seen ...string) string {
	return r.next(author, Remove, target, seen...)
}

func (r *room) closure(ids []string) map[string]bool {
	out := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if id == "" || out[id] {
			return
		}
		out[id] = true
		for _, p := range r.byID[id].Parents {
			walk(p)
		}
	}
	for _, id := range ids {
		walk(id)
	}
	return out
}

// all returns every entry id so far: "the author had seen everything".
func (r *room) all() []string {
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *room) entries(ids ...string) []Entry {
	if ids == nil {
		ids = r.all()
	}
	var out []Entry
	for id := range r.closure(ids) {
		out = append(out, r.byID[id])
	}
	return out
}

func (r *room) state(rules Rules, ids ...string) State { return rules.Resolve(r.entries(ids...)) }

func uniq(ids []string) []string {
	m := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !m[id] {
			m[id] = true
			out = append(out, id)
		}
	}
	return out
}

func wantList(t testing.TB, what string, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got == nil {
		got = []string{}
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func wantReason(t testing.TB, st State, id, reason string) {
	t.Helper()
	if ef := st.Effects[id]; ef.Applied || ef.Reason != reason {
		t.Fatalf("entry %s: got %+v, want reason %q", id, ef, reason)
	}
}

func wantApplied(t testing.TB, st State, id string) {
	t.Helper()
	if ef := st.Effects[id]; !ef.Applied {
		t.Fatalf("entry %s: got %+v, want applied", id, ef)
	}
}

// threeAdmins builds the usual start: A creates, makes B an admin alone
// (it is the only admin), then A and B make C an admin. P is a member.
func threeAdmins(t testing.TB) (*room, []string) {
	r := newRoom(t, "A")
	r.admin("A", "B")
	r.admin("A", "C", r.all()...)
	r.admin("B", "C", r.all()...)
	r.add("A", "P", r.all()...)
	st := r.state(Final)
	wantList(t, "admins", st.Admins, "A", "B", "C")
	wantList(t, "members", st.Members, "A", "B", "C", "P")
	return r, r.all()
}
