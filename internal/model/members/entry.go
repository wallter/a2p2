// Package members is an executable model of a room's member list when the
// room has several admins. It is not used by the daemon yet. It exists so
// that candidate merge rules can be attacked with exhaustive small
// scenarios before the real design is written (REQUIREMENTS section 9,
// step 0).
//
// Keys are short strings and signatures are abstracted away: an entry's
// author is taken to be authentic. Everything is plain deterministic data
// and functions: no I/O, no clocks, no goroutines.
package members

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// Kind is what an entry does. There are four and no more: creating the
// room, adding a machine, removing a machine, and making a machine an admin.
type Kind string

const (
	Create    Kind = "create"
	Add       Kind = "add"
	Remove    Kind = "remove"
	MakeAdmin Kind = "admin"
)

// Entry is one signed change to a room's member list.
//
// One author's entries form a chain: Seq counts them from 1 and Prev is the
// id of the author's previous entry, which must also be among Parents. So an
// author cannot claim not to have seen its own earlier entries. Parents are
// the ids of the room entries the author had seen. ID is a hash of all the
// other fields.
type Entry struct {
	ID      string
	Author  string
	Seq     int
	Prev    string
	Parents []string
	Kind    Kind
	Target  string
}

// New returns e with its parents sorted and its ID filled in.
func New(e Entry) Entry {
	e.Parents = append([]string(nil), e.Parents...)
	sort.Strings(e.Parents)
	e.ID = e.hash()
	return e
}

func (e Entry) hash() string {
	var b strings.Builder
	for _, s := range []string{e.Author, strconv.Itoa(e.Seq), e.Prev, strings.Join(e.Parents, ","), string(e.Kind), e.Target} {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}
