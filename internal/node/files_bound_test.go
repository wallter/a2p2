package node

import (
	"context"
	"slices"
	"testing"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// Tests for the second review of the attachments mechanism (fix-files-a2),
// ruling B1: a manifest is bound to the reference it was verified for.

// TestACopiedCommitmentGetsNoListNoWantAndNoBytes (B1): a member admitted
// without history copies a hidden header's commitment into a message of
// its own. No machine takes the earlier message's list for it, sends it a
// want naming the hidden files, or serves it the bytes.
// Requirements: L1, T3
func TestACopiedCommitmentGetsNoListNoWantAndNoBytes(t *testing.T) {
	ns := filesNet(t, "a", "b", "c")
	a, b, c := ns[0], ns[1], ns[2]
	ctx := context.Background()
	room := fileRoom(t, a, b)
	if err := b.st.SetFileMode(ctx, room, store.FilesKeep, *b.clock); err != nil {
		t.Fatal(err)
	}
	m1, res := a.sendFiles(room, "alice", "before c", writeSource(t, "secret.txt", "hidden from c"))
	hidden := res.Files[0].SHA256
	b.pullAll(room, a)
	b.tickFiles() // b has m1's list and, keeping, its bytes
	if !listHere(t, b, room, m1) {
		t.Fatal("b has no list for m1")
	}
	h := b.manifestOf(room, m1)

	// c joins without history, cannot be dialled, and copies m1's commitment
	a.change(room, Change{Kind: core.EntryAdd, Target: c.id, Name: "c"})
	a.net.noInbound[c.id] = true
	c.pin(room, a.lnode)
	c.pull(room, a.lnode)
	c.register("agent-c")
	b.pullAll(room, a) // b learns c is a member
	c.tick()
	s, err := c.m.write(ctx, room, "agent-c", func(v *roomView, f *core.RecordFields) error {
		f.Kind, f.Text, f.Files = core.KindMessage, "mine", h
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m2 := s.ID
	var wants [][2]string // (to, hash) of every want sent
	a.net.tamper = func(from, to string, resp *PullResponse) {
		for _, w := range resp.Want {
			wants = append(wants, [2]string{to, w.H})
		}
	}
	c.pullAll(room, b) // a summary
	c.pullAll(room, b) // the offer brings m2 to b
	for i := 0; i < 3; i++ {
		b.tickFiles()
		a.tickFiles()
		c.pullAll(room, b)
		c.pullAll(room, a)
	}

	for _, w := range wants {
		if w[1] == hidden {
			t.Fatalf("a want naming a hidden file went to %s", w[0][:8])
		}
	}
	if listHere(t, b, room, m2) {
		t.Fatal("b took m1's list as m2's")
	}
	for _, srv := range []*fnode{a, b} {
		for _, msg := range []string{m1, m2} {
			for _, sha := range []string{hidden, h} {
				fs, err := srv.f.ServeGet(ctx, c.id, FileGet{Room: room, Msg: msg, SHA256: sha})
				if err == nil {
					if fs.Body != nil {
						fs.Body.Close()
					}
					if fs.Status != 202 || sha == hidden {
						t.Fatalf("%s answered c %d for %s of %s", srv.name, fs.Status, sha[:8], msg[:8])
					}
				}
			}
		}
	}
	for _, sv := range b.fn.served {
		if sv[1] == c.id {
			t.Fatalf("c was served %s by %s", sv[2][:8], sv[0][:8])
		}
	}
	// and b wants nothing of c but m2's own manifest
	_ = b.st.View(ctx, func(rd *store.Reader) error {
		ws, err := rd.FileWants(room)
		for _, w := range ws {
			if w.Author == c.id && w.SHA256 != h {
				t.Errorf("b wants %s of c", w.SHA256[:8])
			}
		}
		return err
	})
	if slices.ContainsFunc(wants, func(w [2]string) bool { return w[0] == c.id && w[1] != h }) {
		t.Fatal("a want for something other than m2's manifest went to c")
	}
}
