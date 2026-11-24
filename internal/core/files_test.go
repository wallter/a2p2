package core

import (
	"strings"
	"testing"
)

func testManifest(t *testing.T, files ...FileRef) []byte {
	t.Helper()
	b, err := EncodeManifest(Manifest{Salt: strings.Repeat("ab", 16), Files: files})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestARecordWithFilesCarriesOnlyTheManifestHash: the files field is the
// manifest's SHA-256 and nothing else, costs 75 header bytes, and parses
// back into Fields; a writer refuses it on another kind.
// Requirements: L1, T3
func TestARecordWithFilesCarriesOnlyTheManifestHash(t *testing.T) {
	man := testManifest(t, FileRef{Name: "build.log", Size: 14, SHA256: SHA256Hex([]byte("build log text")), Type: "text/plain"})
	f := RecordFields{Room: vRoom, Epoch: vEpoch, Seq: 1, Kind: KindMessage, Agent: "claude", Created: vCreated, Text: "log"}
	plain := mustRecord(keyA, f)
	f.Files = SHA256Hex(man)
	with := mustRecord(keyA, f)
	if d := len(with.Header()) - len(plain.Header()); d != 75 {
		t.Fatalf("files costs %d header bytes, want 75", d)
	}
	for _, s := range []string{"build.log", "text/plain", SHA256Hex([]byte("build log text"))} {
		if strings.Contains(string(with.Header()), s) {
			t.Fatalf("header names %q: %s", s, with.Header())
		}
	}
	r, err := ParseRecord(with.Wire())
	if err != nil || r.Fields().Files != SHA256Hex(man) || r.FilesMalformed() {
		t.Fatalf("parsed %+v %v", r, err)
	}
	h := f
	h.Kind, h.Ref, h.Text = KindHandled, fakeID("x"), ""
	if _, err := NewRecord(keyA, h); RefusalCode(err) != CodeBadField {
		t.Fatalf("files on handled: %v", err)
	}
	bad := f
	bad.Files = strings.ToUpper(bad.Files)
	if _, err := NewRecord(keyA, bad); RefusalCode(err) != CodeBadField {
		t.Fatalf("upper-case files: %v", err)
	}
}

// TestAMalformedFilesFieldIsStoredAndReadAsNoFiles: as 0.1.0 does, the
// record is valid with the field kept in its bytes and id; it has no files.
// Requirements: L1, D27
func TestAMalformedFilesFieldIsStoredAndReadAsNoFiles(t *testing.T) {
	f := RecordFields{Room: vRoom, Epoch: vEpoch, Seq: 1, Kind: KindMessage, Agent: "claude", Created: vCreated, Text: "log"}
	r := mustRecord(keyA, f)
	for _, v := range []string{`"files":"abc"`, `"files":["x"]`, `"files":1`, `"files":"` + strings.Repeat("A", 64) + `"`} {
		h := insert(r.Header(), v)
		got, err := ParseRecord([]byte(signWire(keyA, h, r.Body())))
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if got.Fields().Files != "" || !got.FilesMalformed() || got.ID() != RecordID(h) {
			t.Fatalf("%s: files %q malformed %v", v, got.Fields().Files, got.FilesMalformed())
		}
	}
	if _, err := ParseRecord([]byte(signWire(keyA, insert(r.Header(), `"files":null`), r.Body()))); RefusalCode(err) != CodeBadField {
		t.Fatalf("null files: %v", err)
	}
}

// TestManifestRules: strict parsing of PROTOCOL 10.2.
// Requirements: L1
func TestManifestRules(t *testing.T) {
	ok := FileRef{Name: "a.txt", Size: 1, SHA256: SHA256Hex([]byte("a"))}
	b := testManifest(t, ok)
	m, err := ParseManifest(b)
	if err != nil || len(m.Files) != 1 || m.Files[0] != ok || m.Salt != strings.Repeat("ab", 16) {
		t.Fatalf("%+v %v", m, err)
	}
	bad := []FileRef{
		{Name: "", Size: 1, SHA256: ok.SHA256},
		{Name: "a/b", Size: 1, SHA256: ok.SHA256},
		{Name: `a\b`, Size: 1, SHA256: ok.SHA256},
		{Name: "a\x00", Size: 1, SHA256: ok.SHA256},
		{Name: "a\u0085", Size: 1, SHA256: ok.SHA256},
		{Name: strings.Repeat("a", 256), Size: 1, SHA256: ok.SHA256},
		{Name: "a", Size: MaxFileSize + 1, SHA256: ok.SHA256},
		{Name: "a", Size: 1, SHA256: strings.ToUpper(ok.SHA256)},
		{Name: "a", Size: 1, SHA256: ok.SHA256, Type: "Text/Plain"},
		{Name: "a", Size: 1, SHA256: ok.SHA256, Type: "text"},
	}
	for _, f := range bad {
		if _, err := EncodeManifest(Manifest{Salt: strings.Repeat("ab", 16), Files: []FileRef{f}}); err == nil {
			t.Errorf("accepted %+v", f)
		}
	}
	nine := make([]FileRef, 9)
	for i := range nine {
		nine[i] = FileRef{Name: "f", Size: 1, SHA256: SHA256Hex([]byte{byte(i)})}
	}
	if _, err := EncodeManifest(Manifest{Salt: strings.Repeat("ab", 16), Files: nine}); err == nil {
		t.Error("accepted nine files")
	}
	if _, err := EncodeManifest(Manifest{Salt: "ab", Files: []FileRef{ok}}); err == nil {
		t.Error("accepted a short salt")
	}
	if _, err := ParseManifest(append(b, ' ', '{', '}')); err == nil {
		t.Error("accepted trailing bytes")
	}
	if _, err := ParseManifest([]byte(strings.Replace(string(b), `"v":1`, `"v":null`, 1))); err == nil {
		t.Error("accepted null")
	}
}

// TestAnOffsetPastItsDigitsIsRefusedBeforeArithmetic: offsets are digits
// only, at most 15, so no arithmetic on them can overflow.
// Requirements: L1
func TestAnOffsetPastItsDigitsIsRefusedBeforeArithmetic(t *testing.T) {
	for _, s := range []string{"", "-1", "+1", "01", "1e3", " 1", "1234567890123456", "99999999999999999999"} {
		if _, ok := ParseOffset(s); ok {
			t.Errorf("accepted %q", s)
		}
	}
	for s, want := range map[string]int64{"0": 0, "8388608": 8 << 20, "999999999999999": 999999999999999} {
		if n, ok := ParseOffset(s); !ok || n != want {
			t.Errorf("%q: %d %v", s, n, ok)
		}
	}
}
