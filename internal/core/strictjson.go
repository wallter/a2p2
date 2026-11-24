package core

import (
	"encoding/base64"
	"errors"
	"strconv"
	"unicode/utf8"
)

// Strict JSON and Base64 as PROTOCOL section 0 defines them. The parser
// keeps every member in received order with its key unescaped, keeps numbers
// as their text, and refuses what encoding/json accepts silently: duplicate
// keys, invalid UTF-8, lone surrogates and trailing data.

type jkind uint8

const (
	jNull jkind = iota
	jFalse
	jTrue
	jNumber
	jString
	jArray
	jObject
)

type jvalue struct {
	kind jkind
	str  string // a string's value, or a number's text
	arr  []jvalue
	obj  []jmember
}

type jmember struct {
	key string
	val jvalue
}

var (
	errJSONUTF8   = errors.New("invalid utf-8 or lone surrogate")
	errJSONSyntax = errors.New("not strict json")
)

// parseStrict parses one JSON value that must fill b, apart from
// insignificant whitespace.
func parseStrict(b []byte) (jvalue, error) {
	if !validText(b) {
		return jvalue{}, errJSONUTF8
	}
	p := jparser{b: b}
	p.ws()
	v, err := p.value()
	if err != nil {
		return jvalue{}, err
	}
	p.ws()
	if p.i != len(b) {
		return jvalue{}, errJSONSyntax
	}
	return v, nil
}

// validText is S4: valid UTF-8 and no \u escape that forms a lone surrogate.
// It scans string escapes lexically, so it gives the same answer whatever
// else is wrong with the JSON.
func validText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	in := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !in {
			in = c == '"'
			continue
		}
		switch c {
		case '"':
			in = false
		case '\\':
			if i+1 < len(b) && b[i+1] == 'u' {
				r, ok := hex4(b, i+2)
				if !ok {
					i++
					continue
				}
				switch {
				case r >= 0xDC00 && r <= 0xDFFF:
					return false
				case r >= 0xD800 && r <= 0xDBFF:
					if i+11 >= len(b) || b[i+6] != '\\' || b[i+7] != 'u' {
						return false
					}
					lo, ok := hex4(b, i+8)
					if !ok || lo < 0xDC00 || lo > 0xDFFF {
						return false
					}
					i += 11
				default:
					i += 5
				}
				continue
			}
			i++
		}
	}
	return true
}

func hex4(b []byte, i int) (rune, bool) {
	if i+4 > len(b) {
		return 0, false
	}
	var r rune
	for _, c := range b[i : i+4] {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return r, true
}

// maxDepth bounds nesting in signed JSON (headers, entries, bodies) so a
// hostile header cannot drive deep recursion. Not in PROTOCOL yet.
const maxDepth = 16

type jparser struct {
	b     []byte
	i     int
	depth int
}

func (p *jparser) ws() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jparser) value() (jvalue, error) {
	if p.i >= len(p.b) {
		return jvalue{}, errJSONSyntax
	}
	switch c := p.b[p.i]; {
	case c == '{' || c == '[':
		if p.depth++; p.depth > maxDepth {
			return jvalue{}, errJSONSyntax
		}
		defer func() { p.depth-- }()
		if c == '{' {
			return p.object()
		}
		return p.array()
	case c == '"':
		s, err := p.string()
		return jvalue{kind: jString, str: s}, err
	case c == 't':
		return jvalue{kind: jTrue}, p.literal("true")
	case c == 'f':
		return jvalue{kind: jFalse}, p.literal("false")
	case c == 'n':
		return jvalue{kind: jNull}, p.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return jvalue{}, errJSONSyntax
}

func (p *jparser) literal(s string) error {
	if len(p.b)-p.i < len(s) || string(p.b[p.i:p.i+len(s)]) != s {
		return errJSONSyntax
	}
	p.i += len(s)
	return nil
}

func (p *jparser) object() (jvalue, error) {
	p.i++ // {
	v := jvalue{kind: jObject}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return v, nil
	}
	seen := map[string]bool{}
	for {
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return v, errJSONSyntax
		}
		k, err := p.string()
		if err != nil {
			return v, err
		}
		if seen[k] {
			return v, errJSONSyntax // duplicate key, compared after unescaping
		}
		seen[k] = true
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return v, errJSONSyntax
		}
		p.i++
		p.ws()
		m, err := p.value()
		if err != nil {
			return v, err
		}
		v.obj = append(v.obj, jmember{k, m})
		p.ws()
		if p.i >= len(p.b) {
			return v, errJSONSyntax
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return v, nil
		default:
			return v, errJSONSyntax
		}
	}
}

func (p *jparser) array() (jvalue, error) {
	p.i++ // [
	v := jvalue{kind: jArray}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return v, nil
	}
	for {
		p.ws()
		e, err := p.value()
		if err != nil {
			return v, err
		}
		v.arr = append(v.arr, e)
		p.ws()
		if p.i >= len(p.b) {
			return v, errJSONSyntax
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return v, nil
		default:
			return v, errJSONSyntax
		}
	}
}

func (p *jparser) string() (string, error) {
	p.i++ // opening quote
	var out []byte
	for p.i < len(p.b) {
		c := p.b[p.i]
		switch {
		case c == '"':
			p.i++
			return string(out), nil
		case c < 0x20:
			return "", errJSONSyntax
		case c != '\\':
			out = append(out, c)
			p.i++
			continue
		}
		if p.i+1 >= len(p.b) {
			return "", errJSONSyntax
		}
		e := p.b[p.i+1]
		p.i += 2
		switch e {
		case '"', '\\', '/':
			out = append(out, e)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, ok := hex4(p.b, p.i)
			if !ok {
				return "", errJSONSyntax
			}
			p.i += 4
			if r >= 0xDC00 && r <= 0xDFFF {
				return "", errJSONUTF8
			}
			if r >= 0xD800 && r <= 0xDBFF {
				if p.i+6 > len(p.b) || p.b[p.i] != '\\' || p.b[p.i+1] != 'u' {
					return "", errJSONUTF8
				}
				lo, ok := hex4(p.b, p.i+2)
				if !ok || lo < 0xDC00 || lo > 0xDFFF {
					return "", errJSONUTF8
				}
				p.i += 6
				r = 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00)
			}
			out = utf8.AppendRune(out, r)
		default:
			return "", errJSONSyntax
		}
	}
	return "", errJSONSyntax
}

// number accepts the RFC 8259 grammar; whether it is a valid Integer is a
// field check (S6), not a syntax check.
func (p *jparser) number() (jvalue, error) {
	start := p.i
	if p.b[p.i] == '-' {
		p.i++
	}
	switch {
	case p.i < len(p.b) && p.b[p.i] == '0':
		p.i++
	case p.i < len(p.b) && p.b[p.i] >= '1' && p.b[p.i] <= '9':
		p.digits()
	default:
		return jvalue{}, errJSONSyntax
	}
	if p.i < len(p.b) && p.b[p.i] == '.' {
		p.i++
		if p.digits() == 0 {
			return jvalue{}, errJSONSyntax
		}
	}
	if p.i < len(p.b) && (p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		p.i++
		if p.i < len(p.b) && (p.b[p.i] == '+' || p.b[p.i] == '-') {
			p.i++
		}
		if p.digits() == 0 {
			return jvalue{}, errJSONSyntax
		}
	}
	return jvalue{kind: jNumber, str: string(p.b[start:p.i])}, nil
}

func (p *jparser) digits() int {
	n := 0
	for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
		p.i++
		n++
	}
	return n
}

// maxInt is the largest Integer (section 0): 2^53 − 1.
const maxInt = 1<<53 - 1

// integer returns v as an Integer: matches 0|[1-9][0-9]*, at most 2^53−1.
func (v jvalue) integer() (int64, bool) {
	if v.kind != jNumber || v.str == "" || len(v.str) > 16 {
		return 0, false
	}
	if v.str[0] == '0' && len(v.str) > 1 {
		return 0, false
	}
	for i := 0; i < len(v.str); i++ {
		if v.str[i] < '0' || v.str[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(v.str, 10, 64)
	if err != nil || n > maxInt {
		return 0, false
	}
	return n, true
}

// present reports whether v is a value a writer may emit in a known field:
// not null, false, zero, an empty string, an empty array or an empty
// object, and every number an Integer, at every depth inside the field.
func (v jvalue) present() bool {
	switch v.kind {
	case jNull, jFalse:
		return false
	case jNumber:
		n, ok := v.integer()
		return ok && n != 0
	case jString:
		return v.str != ""
	case jArray:
		if len(v.arr) == 0 {
			return false
		}
		for _, e := range v.arr {
			if !e.present() {
				return false
			}
		}
	case jObject:
		if len(v.obj) == 0 {
			return false
		}
		for _, m := range v.obj {
			if !m.val.present() {
				return false
			}
		}
	}
	return true
}

// noNull is the only rule for an unknown field's value (section 1): any
// JSON value with no null at any depth.
func (v jvalue) noNull() bool {
	switch v.kind {
	case jNull:
		return false
	case jArray:
		for _, e := range v.arr {
			if !e.noNull() {
				return false
			}
		}
	case jObject:
		for _, m := range v.obj {
			if !m.val.noNull() {
				return false
			}
		}
	}
	return true
}

// base64Len checks s against section 0 without allocating: the RFC 4648
// section 4 alphabet only (encoding/base64 would skip CR and LF), length a
// multiple of 4, padding only at the end, pad bits zero. It returns the
// decoded length.
func base64Len(s []byte) (int, bool) {
	if len(s)%4 != 0 {
		return 0, false
	}
	pad := 0
	for i, c := range s {
		if c == '=' {
			if i < len(s)-2 {
				return 0, false
			}
			pad++
			continue
		}
		if pad > 0 || b64Value(c) < 0 {
			return 0, false
		}
	}
	if pad > 0 {
		last := b64Value(s[len(s)-pad-1])
		if pad == 2 && last&0x0f != 0 || pad == 1 && last&0x03 != 0 {
			return 0, false
		}
	}
	return len(s)/4*3 - pad, true
}

func b64Value(c byte) int {
	switch {
	case c >= 'A' && c <= 'Z':
		return int(c - 'A')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 26
	case c >= '0' && c <= '9':
		return int(c-'0') + 52
	case c == '+':
		return 62
	case c == '/':
		return 63
	}
	return -1
}

// decodeBase64 decodes s, already accepted by base64Len.
func decodeBase64(s []byte) ([]byte, bool) {
	n, ok := base64Len(s)
	if !ok {
		return nil, false
	}
	out := make([]byte, n)
	m, err := base64.StdEncoding.Decode(out, s)
	return out[:m], err == nil && m == n
}

// rawString reads a JSON string at p.i and returns its value; when it has
// no escapes the result aliases the input instead of copying it.
func (p *jparser) rawString() ([]byte, error) {
	for j := p.i + 1; j < len(p.b); j++ {
		switch c := p.b[j]; {
		case c == '"':
			out := p.b[p.i+1 : j]
			p.i = j + 1
			return out, nil
		case c < 0x20:
			return nil, errJSONSyntax
		case c == '\\':
			s, err := p.string()
			return []byte(s), err
		}
	}
	return nil, errJSONSyntax
}
