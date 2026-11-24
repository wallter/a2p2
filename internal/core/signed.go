package core

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"
)

// WireVersion is the protocol version of records and log entries (field v).
const WireVersion = 2

// Refusal codes, PROTOCOL-next sections 1 to 3. A refusal is counted per peer
// and shown locally; nothing is sent back.
const (
	CodeBadWire           = "bad_wire"
	CodeBadBase64         = "bad_base64"
	CodeTooLarge          = "too_large"
	CodeBadUTF8           = "bad_utf8"
	CodeBadJSON           = "bad_json"
	CodeBadField          = "bad_field"
	CodeBadBody           = "bad_body"
	CodeBadVersion        = "bad_version"
	CodeWrongRoom         = "wrong_room"
	CodeAuthorNotAdmitted = "author_not_admitted"
	CodeBadSignature      = "bad_signature"
	CodeSecondCreate      = "second_create"
	CodeTooManyHeads      = "too_many_heads" // writer only
)

// Refusal is the error every parse, verify and construct function returns.
// Check is the check that failed (S1 to S9, V1, G1 from PROTOCOL-next
// sections 2 and 3; empty for a writer's refusal). Field is a known field
// name or a fixed reason, never a string a peer chose or a value.
type Refusal struct {
	Code  string
	Check string
	Field string
}

func (r *Refusal) Error() string {
	s := r.Code
	if r.Check != "" {
		s += " (" + r.Check + ")"
	}
	if r.Field != "" {
		s += ": " + r.Field
	}
	return s
}

// RefusalCode returns err's refusal code, or "" if err is not a Refusal.
func RefusalCode(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

var checkCode = map[string]string{
	"S1": CodeBadWire, "S2": CodeBadBase64, "S3": CodeTooLarge, "S4": CodeBadUTF8, "S5": CodeBadJSON,
	"S6": CodeBadField, "S7": CodeBadField, "S8": CodeBadBody, "S9": CodeBadBody,
	"V1": CodeBadVersion, "G1": CodeBadSignature,
}

// reasons are the only strings a Refusal's Field may hold besides known
// field names.
var reasons = map[string]bool{
	"": true, "h": true, "s": true, "b": true, "json": true, "unknown key": true, "missing key": true,
	"signature": true, "header": true, "header size": true, "body": true, "not empty": true,
	"size": true, "hash": true, "content": true, "empty": true, "text": true, "choices": true,
}

func refuse(check, field string) *Refusal {
	return &Refusal{Code: checkCode[check], Check: check, Field: fieldName(field)}
}

// refuseWrite is a writer's refusal of its own input (no receive check).
func refuseWrite(code, field string) *Refusal {
	return &Refusal{Code: code, Field: fieldName(field)}
}

// fieldName keeps refusal text free of anything a peer chose.
func fieldName(k string) string {
	switch k {
	case "v", "room", "author", "epoch", "seq", "prev", "links", "version", "kind", "agent",
		"to", "ask_handled", "ref", "parent", "thread", "created", "size",
		"parents", "target", "label", "name", "history", "from":
		return k
	}
	if reasons[k] {
		return k
	}
	return "unknown field"
}

// Limits from PROTOCOL-next sections 1 and 3.
const (
	maxHeader      = 4096
	maxFirstHeader = 8192 // a record with seq 1
	maxBody        = 16384
	maxEntry       = 16384
	sigSize        = ed25519.SignatureSize
	maxTo          = 32
	maxLinks       = 100
	maxParents     = 64
	maxFromAuthors = 64
	maxFromHeads   = 16
	maxChoices     = 8
	maxChoiceBytes = 200
	maxRoomLabel   = 64
)

const (
	recordDomain = "a2p2 record v2"
	entryDomain  = "a2p2 entry v2"
	timeLayout   = "2006-01-02T15:04:05.000Z"
)

// emptyHash is the SHA-256 of zero bytes, the body field of handled and
// withdraw records.
const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// RecordID and EntryID are the ids of section 0: the first 16 bytes of
// SHA-256(domain, one zero byte, the signed bytes exactly as received).
func RecordID(header []byte) string { return signedID(recordDomain, header) }
func EntryID(header []byte) string  { return signedID(entryDomain, header) }

func signedID(domain string, signed []byte) string {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte{0})
	h.Write(signed)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// verifySig is G1. A node id is the hex Ed25519 public key (section 0), so
// the author field is the key; no lookup is needed.
func verifySig(author string, signed, sig []byte) error {
	pub, err := hex.DecodeString(author)
	if err != nil || len(pub) != ed25519.PublicKeySize || len(sig) != sigSize ||
		!ed25519.Verify(ed25519.PublicKey(pub), signed, sig) {
		return refuse("G1", "")
	}
	return nil
}

// wireParts is S1 to S3 shared by records and entries, over the wire object
// {"h","s"[,"b"]}. The wire object's own size is bounded by the request it
// arrived in, not here. Nothing is decoded until every part has passed S2
// and the decoded sizes have passed S3, and the scan builds no tree, so
// work and memory are linear in the input and the decoded parts are at most
// the section 1 and 3 limits. maxH is the largest header that may follow.
func wireParts(wire []byte, allowBody bool, maxH int) (h, s, b []byte, hasB bool, err *Refusal) {
	hs, ss, bs, hasB, ok := scanWire(wire, allowBody)
	if !ok {
		return nil, nil, nil, false, refuse("S1", "")
	}
	hn, okH := base64Len(hs)
	sn, okS := base64Len(ss)
	bn, okB := base64Len(bs)
	switch {
	case !okH:
		return nil, nil, nil, false, refuse("S2", "h")
	case !okS:
		return nil, nil, nil, false, refuse("S2", "s")
	case !okB:
		return nil, nil, nil, false, refuse("S2", "b")
	case sn != sigSize:
		return nil, nil, nil, false, refuse("S3", "signature")
	case hn > maxH:
		return nil, nil, nil, false, refuse("S3", "header")
	case bn > maxBody:
		return nil, nil, nil, false, refuse("S3", "body")
	}
	h, _ = decodeBase64(hs)
	s, _ = decodeBase64(ss)
	if bn > 0 {
		b, _ = decodeBase64(bs)
	}
	return h, s, b, hasB, nil
}

// scanWire is S1: strict JSON, one object, keys h, s and (if allowed) b only,
// each a string, h and s non-empty. A wire value that is not a string ends
// the scan at once, so nesting never builds up.
func scanWire(wire []byte, allowBody bool) (h, s, b []byte, hasB, ok bool) {
	if !utf8.Valid(wire) {
		return
	}
	p := jparser{b: wire}
	p.ws()
	if p.i >= len(wire) || wire[p.i] != '{' {
		return
	}
	p.i++
	var hasH, hasS bool
	for first := true; ; first = false {
		p.ws()
		if first && p.i < len(wire) && wire[p.i] == '}' {
			break
		}
		if p.i >= len(wire) || wire[p.i] != '"' {
			return
		}
		k, err := p.rawString()
		if err != nil {
			return
		}
		p.ws()
		if p.i >= len(wire) || wire[p.i] != ':' {
			return
		}
		p.i++
		p.ws()
		if p.i >= len(wire) || wire[p.i] != '"' {
			return
		}
		v, err := p.rawString()
		if err != nil {
			return
		}
		switch {
		case string(k) == "h" && !hasH:
			h, hasH = v, true
		case string(k) == "s" && !hasS:
			s, hasS = v, true
		case string(k) == "b" && allowBody && !hasB:
			b, hasB = v, true // S8 allows an empty b
		default:
			return // unknown or duplicate key
		}
		p.ws()
		if p.i < len(wire) && wire[p.i] == ',' {
			p.i++
			continue
		}
		if p.i >= len(wire) || wire[p.i] != '}' {
			return
		}
		p.i++
		break
	}
	p.ws()
	return h, s, b, hasB, p.i == len(wire) && len(h) > 0 && len(s) > 0
}

// headerObject is S4 and S5: valid text, then one strict JSON object.
func headerObject(h []byte) (jvalue, *Refusal) {
	if !validText(h) {
		return jvalue{}, refuse("S4", "")
	}
	v, err := parseStrict(h)
	if err != nil || v.kind != jObject {
		return jvalue{}, refuse("S5", "")
	}
	return v, nil
}

func encodeWire(h, s, b []byte) []byte {
	out := []byte(`{"h":"`)
	out = base64.StdEncoding.AppendEncode(out, h)
	out = append(out, `","s":"`...)
	out = base64.StdEncoding.AppendEncode(out, s)
	if len(b) > 0 {
		out = append(out, `","b":"`...)
		out = base64.StdEncoding.AppendEncode(out, b)
	}
	return append(out, `"}`...)
}

// Field formats, section 0.

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < n; i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isNode(s string) bool { return isHex(s, 64) }
func isID(s string) bool   { return isHex(s, 32) }
func isRoom(s string) bool { return len(s) == 34 && s[:2] == "r_" && isHex(s[2:], 32) }
func isEpoch(s string) bool {
	return isHex(s, 16) && s != "0000000000000000"
}

func parseTime(s string) (time.Time, bool) {
	if len(s) != len(timeLayout) {
		return time.Time{}, false
	}
	t, err := time.Parse(timeLayout, s)
	if err != nil || t.Format(timeLayout) != s {
		return time.Time{}, false
	}
	return t, true
}

// String-valued field readers used by both decoders. Each returns false if
// the value has the wrong type or format.

func strOf(v jvalue, ok func(string) bool) (string, bool) {
	if v.kind != jString || !ok(v.str) {
		return "", false
	}
	return v.str, true
}

func posInt(v jvalue) (int64, bool) {
	n, ok := v.integer()
	return n, ok && n >= 1
}

// strictlySorted reports whether ids are in increasing order, so unique.
func strictlySorted(ids []string) bool {
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			return false
		}
	}
	return true
}

// Writer helpers: compact JSON, fields in a fixed order, zero values omitted.

type jwriter struct{ b []byte }

func (w *jwriter) key(k string) {
	if len(w.b) == 0 {
		w.b = append(w.b, '{')
	} else {
		w.b = append(w.b, ',')
	}
	w.b = appendJSONString(w.b, k)
	w.b = append(w.b, ':')
}

func (w *jwriter) str(k, v string) {
	if v != "" {
		w.key(k)
		w.b = appendJSONString(w.b, v)
	}
}

func (w *jwriter) int(k string, v int64) {
	if v != 0 {
		w.key(k)
		w.b = strconv.AppendInt(w.b, v, 10)
	}
}

func (w *jwriter) bool(k string, v bool) {
	if v {
		w.key(k)
		w.b = append(w.b, "true"...)
	}
}

func (w *jwriter) strs(k string, vs []string) {
	if len(vs) == 0 {
		return
	}
	w.key(k)
	w.b = appendStrings(w.b, vs)
}

func (w *jwriter) raw(k string, v []byte) {
	w.key(k)
	w.b = append(w.b, v...)
}

func (w *jwriter) end() []byte {
	if len(w.b) == 0 {
		return []byte("{}")
	}
	return append(w.b, '}')
}

func appendStrings(b []byte, vs []string) []byte {
	b = append(b, '[')
	for i, v := range vs {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendJSONString(b, v)
	}
	return append(b, ']')
}

// appendJSONString writes s as a JSON string. Callers check UTF-8 first:
// encoding/json would replace invalid bytes rather than fail.
func appendJSONString(b []byte, s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return append(b, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))...)
}

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }
