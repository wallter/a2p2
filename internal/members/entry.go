// Package members decides a room's member list: who is a member, who is
// an admin, what each entry did and why, and each member's history
// permission, from the set of the room's log entries (docs/DESIGN-next.md
// section 3). It began as an executable model (FINDINGS.md); the daemon
// uses it through the adapter in adapter.go, which takes entries already
// parsed and signature-checked by internal/core.
//
// The tests drive the same code with model entries: short string keys, an
// abstract hash as id, authorship taken as authentic. Everything is plain
// deterministic data and functions: no I/O, no clocks, no goroutines.
package members

import (
	"crypto/sha256"

	"encoding/hex"
	"github.com/wallter/a2p2/internal/core"
	"sort"
	"strconv"
	"strings"
)

// entryKind is what an entry does. There are four and no more: creating the
// room, adding a machine, removing a machine, and making a machine an admin.
type entryKind string

const (
	kCreate entryKind = "create"
	kAdd    entryKind = "add"
	kRemove entryKind = "remove"
	kAdmin  entryKind = "admin"
)

// entry is one signed change to a room's member list.
//
// One author's entries form a chain: Seq counts them from 1 and Prev is the
// id of the author's previous entry, which must also be among Parents. So an
// author cannot claim not to have seen its own earlier entries. Parents are
// the ids of the room entries the author had seen. ID is a hash of all the
// other fields.
type entry struct {
	ID      string
	Author  string
	Seq     int64
	Prev    string
	Parents []string
	Kind    entryKind
	Target  string
	// History and From are a grant's history permission (PROTOCOL
	// section 3): the grantee may read earlier history, or, without
	// History, the signer's heads per author bound what it may read.
	History bool
	From    map[string][]string
	// src is the verified core entry this was made from, nil in the model
	// tests. Its id is the entry's id; nothing is hashed again.
	src  *core.VerifiedEntry
	room string // the signed room, empty in the model tests
	size int    // the signed header's size, 0 in the model tests
}

// New returns e with its parents sorted and its ID filled in.
func newEntry(e entry) entry {
	e.Parents = append([]string(nil), e.Parents...)
	sort.Strings(e.Parents)
	e.ID = e.hash()
	return e
}

func (e entry) hash() string {
	var b strings.Builder
	fields := []string{e.Author, strconv.FormatInt(e.Seq, 10), e.Prev, strings.Join(e.Parents, ","), string(e.Kind), e.Target}
	if e.History || len(e.From) > 0 {
		fields = append(fields, strconv.FormatBool(e.History))
		for _, a := range sortedKeys(e.From) {
			fields = append(fields, a, strings.Join(e.From[a], ","))
		}
	}
	for _, s := range fields {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}
