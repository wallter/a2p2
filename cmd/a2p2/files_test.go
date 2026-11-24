package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

// DESIGN 12 item 8: confirm shows a send with files with every term: the
// full path as the daemon escaped it, size, SHA-256, room, recipients and
// the message replaced.
func TestConfirmShowsEveryTermOfASendWithFiles(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	q := localapi.Request{ID: "q_1", Kind: store.RequestSendFiles, Room: "r_1", RoomLabel: "team", Agent: "bot@1a2b3c4d",
		To: []string{"everyone"}, Members: 3, Replaces: &localapi.Replaced{ID: "m1", Line: "old \\u{202e}line"},
		TextLine: "new text", Terms: strings.Repeat("cd", 32), Deadline: "2026-11-25T12:00:00Z",
		Text: "replace message m1 in room team (r_1), by agent bot@1a2b3c4d, with new text and 2 files (2062 bytes)",
		Files: []localapi.RequestFile{
			{Path: `/home/me/report\u{202e}txt.exe`, Name: `report\u{202e}txt.exe`, Size: 2048, SHA256: sha},
			{Path: "sha256:" + sha, Name: "kept.log", Size: 14, SHA256: sha, Kept: true},
		}}
	var out bytes.Buffer
	printTerms(&out, q)
	got := out.String()
	for _, want := range []string{q.Text, "bot@1a2b3c4d", "team (r_1)", "everyone in the room (3 members)",
		`replaces  m1: "old \u{202e}line"`, "only the files listed", `"new text"`,
		`file 1    /home/me/report\u{202e}txt.exe`, "2.0 KiB (2048 bytes)", "sha256 " + sha, "14 bytes",
		"kept from the message replaced", "terms     " + q.Terms, "declined unless approved"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.ContainsRune(got, 0x202e) || strings.Contains(got, "Invite expires") {
		t.Fatalf("terms shown: %q", got)
	}
}

func TestInboxShowsFilesAndWhatBecameOfASendWithFiles(t *testing.T) {
	var out bytes.Buffer
	printEntry(&out, localapi.Entry{Kind: "message", ID: "m1", From: "bot@1a2b3c4d", Created: "2026-11-24T10:00:00.000Z",
		Text: "the log", Files: []localapi.EntryFile{{Name: "a\u202eb.log", Size: 14, SHA256: "ff", State: node.FileStateNotFetched}},
		FilesNote: node.FilesNoteInvalid})
	printEntry(&out, localapi.Entry{Kind: "message", ID: "m2", From: "bot@1a2b3c4d", FilesPending: true})
	printEntry(&out, localapi.Entry{Kind: node.EntryRequest, ID: "m3", Request: "q_1", State: node.RequestSent})
	printEntry(&out, localapi.Entry{Kind: node.EntryRequest, ID: "q_2", Request: "q_2", State: node.RequestDeclined, Reason: "expired"})
	got := out.String()
	for _, want := range []string{`file a\u{202e}b.log  14 bytes  not fetched`, "sha256 ff", "files: invalid file list",
		"their list is not here yet", "request q_1) was approved and sent", "request q_2) was declined: expired"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFetchPrintsThePathAloneAndSaysItWasNotOpened(t *testing.T) {
	var out, notes bytes.Buffer
	printFetch(&out, &notes, localapi.FetchResp{State: node.FetchHere, Path: "/h/files/r/a.log", Size: 3})
	if out.String() != "/h/files/r/a.log\n" || !strings.Contains(notes.String(), "not opened") {
		t.Fatalf("here: %q, %q", out.String(), notes.String())
	}
	out.Reset()
	printFetch(&out, &notes, localapi.FetchResp{State: node.FetchUnavailable, Reason: "not_carried",
		Holders: []localapi.Holder{{Node: "0123456789abcdef", At: "2026-11-24T10:00:00Z", Answer: "gave_up"}}})
	if !strings.Contains(out.String(), "unavailable: not_carried") || !strings.Contains(out.String(), "01234567 answered gave_up") {
		t.Fatalf("unavailable: %q", out.String())
	}
}

func TestFetchFlagsWorkAfterTheHash(t *testing.T) {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	room := fs.String("room", "", "")
	wait := fs.Int("wait", 0, "")
	if err := fs.Parse(permute(fs, []string{"abc", "--room", "team", "--wait", "5"})); err != nil {
		t.Fatal(err)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "abc" || *room != "team" || *wait != 5 {
		t.Fatalf("args %v room %q wait %d", fs.Args(), *room, *wait)
	}
}

func TestSizeText(t *testing.T) {
	for n, want := range map[int64]string{0: "0 bytes", 1023: "1023 bytes", 1536: "1.5 KiB (1536 bytes)",
		256 << 20: "256.0 MiB (268435456 bytes)", 4 << 30: "4.0 GiB (4294967296 bytes)"} {
		if got := sizeText(n); got != want {
			t.Errorf("sizeText(%d) = %q, want %q", n, got, want)
		}
	}
}
