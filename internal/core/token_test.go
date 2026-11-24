package core

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Golden token vectors (PROTOCOL-next 5.1) under testdata/tokens.
// Regenerate after a deliberate format change with:
//
//	go test ./internal/core -run TestTokenVectors -update
const tokenDir = "testdata/tokens"

type tokenVector struct {
	Note        string   `json:"note"`
	Token       string   `json:"token"`
	Secret      string   `json:"secret,omitempty"`      // hex, when valid
	Fingerprint string   `json:"fingerprint,omitempty"` // hex, when valid
	Addrs       []string `json:"addrs,omitempty"`
	Canonical   string   `json:"canonical,omitempty"` // EncodeToken of the decoded token
	Error       string   `json:"error,omitempty"`     // the refusal, when invalid
}

var tokenErrs = map[string]error{
	"prefix": ErrTokenPrefix, "chars": ErrTokenChars, "length": ErrTokenLength,
	"pad_bits": ErrTokenPadBits, "addrs": ErrTokenAddrs, "addr": ErrTokenAddr,
}

func tokenErrName(err error) string {
	for k, v := range tokenErrs {
		if errors.Is(err, v) {
			return k
		}
	}
	return "unexpected: " + err.Error()
}

func vSecret() (s [16]byte) {
	for i := range s {
		s[i] = byte(i*17 + 3)
	}
	return s
}

// setChar replaces the n-th code character (0-based, groups ignored).
func setChar(tok string, n int, c byte) string {
	b := []byte(tok)
	seen := 0
	for i := len(TokenPrefix); i < len(b) && b[i] != '@'; i++ {
		if b[i] == '-' {
			continue
		}
		if seen == n {
			b[i] = c
			return string(b)
		}
		seen++
	}
	panic("setChar: no such character")
}

// charAt returns the n-th code character (0-based, groups ignored).
func charAt(tok string, n int) byte { return tok[len(TokenPrefix)+n+n/4] }

func tokenVectors(t *testing.T) map[string]string {
	fp, _ := KeyFingerprint(nodeA)
	enc := func(addrs ...string) string {
		s, err := EncodeToken(Token{Secret: vSecret(), Fingerprint: fp, Addrs: addrs})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	one := enc("203.0.113.7:7443")
	code := one[len(TokenPrefix):strings.IndexByte(one, '@')]
	folded := strings.NewReplacer("-", "", "1", "l", "0", "O").Replace(strings.ToLower(code))
	folded = strings.Replace(folded, "l", "I", 1)
	nine := strings.Split(strings.Repeat("a.example:1,", 9), ",")
	nine = nine[:9]
	return map[string]string{
		"valid-one-address":        one,
		"valid-protocol-example":   enc("203.0.113.7:7443", "[2001:db8::7]:7443"),
		"valid-eight-addresses":    enc("a.example:1", "b.example:2", "10.0.0.1:65535", "[::1]:3", "e:4", "f-g.h:5", "i:6", "j:7"),
		"valid-folded":             TokenPrefix + folded + "@203.0.113.7:7443",
		"valid-whitespace-around":  "  " + one + "\n",
		"bad-prefix":               "a2p3:" + one[len(TokenPrefix):],
		"bad-char-u":               setChar(one, 3, 'U'),
		"bad-char-symbol":          setChar(one, 30, '*'),
		"bad-length-short":         strings.Replace(one, code, code[:len(code)-1], 1),
		"bad-length-long":          strings.Replace(one, code, code+"0", 1),
		"bad-pad-bits-secret":      setChar(one, 25, crockford[crockValue(charAt(one, 25))|1]),
		"bad-pad-bits-fingerprint": setChar(one, 51, crockford[crockValue(charAt(one, 51))|2]),
		"bad-no-at":                one[:strings.IndexByte(one, '@')],
		"bad-no-addresses":         one[:strings.IndexByte(one, '@')+1],
		"bad-nine-addresses":       one[:strings.IndexByte(one, '@')+1] + strings.Join(nine, ","),
		"bad-empty-address":        one + ",",
		"bad-ipv6-unbracketed":     one[:strings.IndexByte(one, '@')+1] + "2001:db8::7:7443",
		"bad-port-zero":            one[:strings.IndexByte(one, '@')+1] + "h:0",
		"bad-port-leading-zero":    one[:strings.IndexByte(one, '@')+1] + "h:07443",
		"bad-no-port":              one[:strings.IndexByte(one, '@')+1] + "203.0.113.7",
	}
}

func vectorOf(name, tok string) tokenVector {
	v := tokenVector{Note: name, Token: tok}
	got, err := DecodeToken(tok)
	if err != nil {
		v.Error = tokenErrName(err)
		return v
	}
	v.Secret, v.Fingerprint, v.Addrs = hex.EncodeToString(got.Secret[:]), hex.EncodeToString(got.Fingerprint[:]), got.Addrs
	v.Canonical, _ = EncodeToken(got)
	return v
}

// Golden vectors: each file states what the decoder gives for a token,
// valid or refused, and the valid ones re-encode to their canonical form.
//
// Requirements: T3
func TestTokenVectors(t *testing.T) {
	gen := tokenVectors(t)
	if *update {
		_ = os.RemoveAll(tokenDir)
		if err := os.MkdirAll(tokenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, tok := range gen {
			b, _ := json.MarshalIndent(vectorOf(name, tok), "", "  ")
			if err := os.WriteFile(filepath.Join(tokenDir, name+".json"), append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	files, err := filepath.Glob(filepath.Join(tokenDir, "*.json"))
	if err != nil || len(files) != len(gen) {
		t.Fatalf("token vectors: %d files, %d generated (run with -update)", len(files), len(gen))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var want tokenVector
		if err := json.Unmarshal(b, &want); err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if got := vectorOf(name, want.Token); !equalVector(got, want) {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
		if strings.HasPrefix(name, "valid-") == (want.Error != "") {
			t.Errorf("%s: name and outcome disagree: %q", name, want.Error)
		}
	}
}

func equalVector(a, b tokenVector) bool {
	return a.Token == b.Token && a.Secret == b.Secret && a.Fingerprint == b.Fingerprint &&
		slices.Equal(a.Addrs, b.Addrs) && a.Canonical == b.Canonical && a.Error == b.Error
}

// Every refusal of PROTOCOL-next 5.1, and the folding of I, L, O and case.
//
// Requirements: T3
func TestTokenRefusalsAndFolding(t *testing.T) {
	gen := tokenVectors(t)
	want := map[string]string{
		"bad-prefix": "prefix", "bad-char-u": "chars", "bad-char-symbol": "chars",
		"bad-length-short": "length", "bad-length-long": "length",
		"bad-pad-bits-secret": "pad_bits", "bad-pad-bits-fingerprint": "pad_bits",
		"bad-no-at": "addrs", "bad-no-addresses": "addr", "bad-nine-addresses": "addrs",
		"bad-empty-address": "addr", "bad-ipv6-unbracketed": "addr", "bad-port-zero": "addr",
		"bad-port-leading-zero": "addr", "bad-no-port": "addr",
	}
	for name, code := range want {
		_, err := DecodeToken(gen[name])
		if err == nil || tokenErrName(err) != code {
			t.Errorf("%s: got %v, want %s", name, err, code)
		}
		if err != nil && strings.Contains(err.Error(), gen[name][len(TokenPrefix):len(TokenPrefix)+8]) {
			t.Errorf("%s: the error quotes the token", name)
		}
	}
	a, err := DecodeToken(gen["valid-one-address"])
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeToken(gen["valid-folded"])
	if err != nil || a.Secret != b.Secret || a.Fingerprint != b.Fingerprint {
		t.Fatalf("folded token decodes differently: %v", err)
	}
	if fp, _ := KeyFingerprint(nodeA); a.Fingerprint != fp || a.Secret != vSecret() {
		t.Fatal("fields do not round-trip")
	}
	s, err := DecodeSecret(strings.ToLower(strings.ReplaceAll(a.SecretText(), "1", "L")))
	if err != nil || s != a.Secret {
		t.Fatalf("secret text: %v", err)
	}
	if _, err := DecodeSecret(a.SecretText()[:25]); !errors.Is(err, ErrTokenLength) {
		t.Fatalf("short secret: %v", err)
	}
	if _, err := EncodeToken(Token{}); !errors.Is(err, ErrTokenAddrs) {
		t.Fatalf("no addresses: %v", err)
	}
}

// The decoder never panics, and whatever it accepts re-encodes to a token
// that decodes to the same thing.
func FuzzDecodeToken(f *testing.F) {
	for _, tok := range tokenVectors(&testing.T{}) {
		f.Add(tok)
	}
	f.Fuzz(func(t *testing.T, s string) {
		tok, err := DecodeToken(s)
		if err != nil {
			return
		}
		enc, err := EncodeToken(tok)
		if err != nil {
			t.Fatalf("accepted token does not encode: %v", err)
		}
		again, err := DecodeToken(enc)
		if err != nil || again.Secret != tok.Secret || again.Fingerprint != tok.Fingerprint || !slices.Equal(again.Addrs, tok.Addrs) {
			t.Fatalf("round trip: %v", err)
		}
		if s2, err := DecodeSecret(tok.SecretText()); err != nil || s2 != tok.Secret {
			t.Fatalf("secret round trip: %v", err)
		}
	})
}
