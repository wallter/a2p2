package localapi

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/node"
)

// shownBytes is what s takes in a result once escaped for one line and
// quoted by JSON, as ToolLine and ToolJSON write it.
func shownBytes(s string) int {
	b, _ := json.Marshal(node.EscapeDisplay(s))
	return len(b) - 2
}

// longestName is the file name, valid for a manifest, that a result shows
// longest: a name is shown clipped to 128 characters, so it is the 128
// characters of at most 255 bytes, then one more, that are written
// longest, found among every character a name may hold.
func longestName() string {
	best := map[int]rune{} // by UTF-8 width, the character written longest
	for r := rune(0x20); r <= utf8.MaxRune; r++ {
		if !utf8.ValidRune(r) || !core.ValidFileName(string(r)) {
			continue
		}
		w := utf8.RuneLen(r)
		if b, ok := best[w]; !ok || shownBytes(string(r)) > shownBytes(string(b)) {
			best[w] = r
		}
	}
	// gain[n][b]: the most shown bytes of n characters in b bytes.
	const runes, bytes = 128, core.MaxFileName - 1 // one byte left for the 129th
	gain := make([][]int, runes+1)
	pick := make([][]int, runes+1)
	for n := range gain {
		gain[n], pick[n] = make([]int, bytes+1), make([]int, bytes+1)
		for b := range gain[n] {
			gain[n][b] = -1
		}
	}
	gain[0][0] = 0
	for n := 1; n <= runes; n++ {
		for b := 0; b <= bytes; b++ {
			for w, r := range best {
				if b >= w && gain[n-1][b-w] >= 0 {
					if g := gain[n-1][b-w] + shownBytes(string(r)); g > gain[n][b] {
						gain[n][b], pick[n][b] = g, w
					}
				}
			}
		}
	}
	top := 0
	for b := range gain[runes] {
		if gain[runes][b] > gain[runes][top] {
			top = b
		}
	}
	var name []rune
	for n, b := runes, top; n > 0; n-- {
		w := pick[n][b]
		name = append(name, best[w])
		b -= w
	}
	return string(name) + "a" // the 129th: the name is clipped and shown with "..."
}

// [U4, D72] Every field of an inbox message but its text, each at the
// largest a received record, manifest and room log admit, and written
// with the escapes that grow most, fits the ceiling with a text_cut
// sentence and more at their longest. So a text cut to nothing always
// fits, the cut at the ceiling never fails, and no entry can hold a
// reader's queue. The values go through the same checks as received
// ones: the record is signed and parsed, the manifest encoded and
// parsed, the labels checked.
func TestAnEntryAtItsLargestBesidesItsTextFitsTheCeiling(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	full := hex.EncodeToString(key.Public().(ed25519.PublicKey))
	label := strings.Repeat("a", core.MaxLabel)
	if !core.ValidLabel(label) {
		t.Fatal("the longest label is refused")
	}
	id := func(s string) string { return strings.Repeat(s, 32) }
	f := core.RecordFields{Room: "r_" + id("0"), Epoch: strings.Repeat("1", 16), Seq: 1, Kind: core.KindMessage, Agent: label,
		Parent: id("2"), Thread: id("3"), Created: time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC), Text: "t", Files: strings.Repeat("4", 64)}
	for i := range 32 { // core.MaxTo
		f.To = append(f.To, core.Addressee{Node: full, Agent: label[:len(label)-2] + string(rune('a'+i/26)) + string(rune('a'+i%26))})
	}
	for i := range 8 { // eight distinct choices of 200 bytes, DEL but the last: \u{7f}, eight bytes shown for one
		f.Choices = append(f.Choices, strings.Repeat("\x7f", 199)+string(rune('a'+i)))
	}
	rec, err := core.NewRecord(key, f)
	if err != nil {
		t.Fatal(err)
	}
	if rec, err = core.ParseRecord(rec.Wire()); err != nil {
		t.Fatal(err)
	}
	got := rec.Fields()

	name := longestName()
	m := core.Manifest{Salt: strings.Repeat("5", 32)}
	for i := range core.MaxFiles {
		m.Files = append(m.Files, core.FileRef{Name: name, Size: 1 << 28, SHA256: strings.Repeat(string("89abcdef"[i]), 64), Type: strings.Repeat("a", 50) + "/" + strings.Repeat("b", 49)})
	}
	b, err := core.EncodeManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if m, err = core.ParseManifest(b); err != nil {
		t.Fatal(err)
	}

	// A room label is any UTF-8 of up to 64 bytes on receipt (core,
	// entryFields); a label shown in two rooms gains '#' and the room id.
	room := strings.Repeat("\x7f", 64)
	shownRoom := room + "#" + strings.TrimPrefix(got.Room, "r_")
	author := label + "@" + full // id_shared: written with the full key

	e := Entry{Key: rec.ID(), Version: rec.ID(), ID: rec.ID(), Kind: "message", Edited: true, Room: got.Room, RoomLabel: shownRoom,
		From: author, ChangedBy: label[:63] + "b@" + full, IDShared: true, AuthorRemoved: true, AskHandled: true,
		Parent: got.Parent, Thread: got.Thread, ParentState: "withdrawn", Late: true, Created: "2026-11-27T00:00:00.000000000Z",
		TextCut: 1 << 30, Choices: got.Choices}
	for _, a := range got.To {
		e.To = append(e.To, a.Agent+"@"+a.Node)
	}
	for _, fr := range m.Files {
		e.Files = append(e.Files, EntryFile{SHA256: fr.SHA256, Name: fr.Name, Size: fr.Size, Type: fr.Type, State: node.FileStateNotFetched})
	}
	// No levels: a read whose levels do not fit is told to set them in
	// a call of their own, and reads without them.
	size := ToolInboxSize([]Entry{e}, 1<<30, 1<<30, nil, "")
	if size > node.MaxInboxBytes-1000 {
		t.Fatalf("a message without its text takes %d bytes, within 1,000 of the ceiling %d", size, node.MaxInboxBytes)
	}
	t.Logf("%d bytes without text, of %d", size, node.MaxInboxBytes)
}
