package localapi

import (
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/node"
)

// [U4, D72] Every field of an inbox entry but its text, each at the
// largest a message or notice can carry and written with the escape that
// grows most (DEL, \u{7f} with its backslash escaped: eight bytes for
// one), fits the ceiling with a text_cut sentence and more at their
// longest. So an entry whose text is cut to nothing still fits, the cut
// at the ceiling never fails, and no entry can hold a reader's queue.
// Some fields here never come together, so this is above any real entry.
func TestAnEntryAtItsLargestBesidesItsTextFitsTheCeiling(t *testing.T) {
	del := func(n int) string { return strings.Repeat("\x7f", n) }
	key := strings.Repeat("f", 64)
	id := strings.Repeat("a", 64) + "@" + key // the longest label, with the full key
	e := Entry{Key: key, Version: key, ID: key, Room: "r_" + strings.Repeat("0", 32), RoomLabel: del(64),
		From: id, ChangedBy: id, Parent: key, Thread: key, ParentState: "withdrawn", Kind: "replaced", Edited: true,
		IDShared: true, AuthorRemoved: true, AskHandled: true, Late: true, Skipped: true, Created: "2026-11-27T00:00:00.000000000Z",
		FilesPending: true, FilesNote: node.FilesNoteUnavailable, Request: key, State: "declined", Reason: del(255),
		TextCut: 1 << 30}
	for range 32 { // core.MaxTo
		e.To = append(e.To, id)
	}
	for range 8 { // choices: 8 of 200 bytes
		e.Choices = append(e.Choices, del(200))
	}
	for range 8 { // core.MaxFiles, names of core.MaxFileName, types of core.MaxFileType
		e.Files = append(e.Files, EntryFile{SHA256: key, Name: del(255), Size: 1 << 40, Type: strings.Repeat("a", 100), State: node.FileStateNotFetched})
	}
	// No levels: a read whose levels do not fit is told to set them in
	// a call of their own, and reads without them.
	got := ToolInboxSize([]Entry{e}, 1<<30, 1<<30, nil, "")
	if got > node.MaxInboxBytes {
		t.Fatalf("an entry without its text takes %d bytes, over the ceiling %d", got, node.MaxInboxBytes)
	}
	t.Logf("%d bytes without text, of %d", got, node.MaxInboxBytes)
}
