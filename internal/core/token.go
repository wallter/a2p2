package core

// The pairing token of PROTOCOL-next section 5.1. It lives in core because
// it is a pure format (no I/O) that the admin side, the joiner side and the
// surfaces all read and write, like ids and records.

import (
	"crypto/sha256"
	"errors"
	"net"
	"strconv"
	"strings"
)

// TokenPrefix starts every token.
const TokenPrefix = "a2p2:"

// Token limits (PROTOCOL-next 5.1).
const (
	TokenSecretBytes = 16 // 128-bit secret
	TokenFPBytes     = 16 // first 16 bytes of SHA-256 of the admin's key
	TokenMaxAddrs    = 8
	TokenMaxAddr     = 255 // bytes in one host:port
	tokenFieldChars  = 26  // 128 bits and 2 zero pad bits in Crockford base32
	tokenChars       = 2 * tokenFieldChars
)

// Token is a decoded invite token: the secret, the fingerprint of the
// admin key that must answer, and where to reach it. The secret is a
// credential: never log it, put it in an error or store it.
type Token struct {
	Secret      [TokenSecretBytes]byte
	Fingerprint [TokenFPBytes]byte
	Addrs       []string
}

// Errors of DecodeToken. None of them carries any part of the token.
var (
	ErrTokenPrefix  = errors.New("token: does not start with a2p2:")
	ErrTokenChars   = errors.New("token: a character is not Crockford base32")
	ErrTokenLength  = errors.New("token: wrong length")
	ErrTokenPadBits = errors.New("token: pad bits are not zero")
	ErrTokenAddrs   = errors.New("token: needs 1 to 8 comma-separated host:port addresses")
	ErrTokenAddr    = errors.New("token: an address is not host:port")
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// crockValue maps a character to its value: case is ignored, I and L read
// as 1, O as 0; -1 for anything else (U included).
func crockValue(c byte) int {
	if c >= 'a' && c <= 'z' {
		c -= 'a' - 'A'
	}
	switch c {
	case 'I', 'L':
		return 1
	case 'O':
		return 0
	}
	return strings.IndexByte(crockford, c)
}

// KeyFingerprint is the first 16 bytes of SHA-256 of a node id's public
// key (PROTOCOL-next section 0). ok is false for a malformed node id.
func KeyFingerprint(node string) (fp [TokenFPBytes]byte, ok bool) {
	if !isNode(node) {
		return fp, false
	}
	var pub [32]byte
	for i := range pub {
		pub[i] = hexNibble(node[2*i])<<4 | hexNibble(node[2*i+1])
	}
	sum := sha256.Sum256(pub[:])
	copy(fp[:], sum[:TokenFPBytes])
	return fp, true
}

func hexNibble(c byte) byte {
	if c >= 'a' {
		return c - 'a' + 10
	}
	return c - '0'
}

// encodeField writes 16 bytes as 26 base32 characters, the last 2 bits zero.
func encodeField(b [16]byte) string {
	var out [tokenFieldChars]byte
	acc, n, j := uint32(0), 0, 0
	for _, x := range b {
		acc = acc<<8 | uint32(x)
		n += 8
		for n >= 5 {
			n -= 5
			out[j] = crockford[(acc>>n)&31]
			j++
		}
	}
	out[j] = crockford[(acc<<(5-n))&31] // n is 3: 3 bits and 2 zero pad bits
	return string(out[:])
}

// decodeField reads 26 values back into 16 bytes, refusing non-zero pad bits.
func decodeField(v []int) ([16]byte, error) {
	var out [16]byte
	acc, n, j := uint32(0), 0, 0
	for _, x := range v {
		acc = acc<<5 | uint32(x)
		n += 5
		if n >= 8 {
			n -= 8
			if j < len(out) {
				out[j] = byte(acc >> n)
			}
			j++
		}
	}
	if n != 2 || acc&3 != 0 {
		return out, ErrTokenPadBits
	}
	return out, nil
}

// EncodeToken writes t as PROTOCOL-next 5.1 shows it: the prefix, 52
// upper-case characters in groups of four, then @ and the addresses.
func EncodeToken(t Token) (string, error) {
	if err := checkAddrs(t.Addrs); err != nil {
		return "", err
	}
	code := encodeField(t.Secret) + encodeField(t.Fingerprint)
	var b strings.Builder
	b.WriteString(TokenPrefix)
	for i := 0; i < len(code); i += 4 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(code[i : i+4])
	}
	b.WriteByte('@')
	b.WriteString(strings.Join(t.Addrs, ","))
	return b.String(), nil
}

// DecodeToken reads a token. Surrounding white space is ignored (a pasted
// token often has some); in the code, so are '-' and case, and I, L and O
// read as 1, 1 and 0. It refuses non-zero pad bits, a code that is not 52
// characters, and anything but 1 to 8 addresses.
func DecodeToken(s string) (Token, error) {
	s = strings.TrimSpace(s)
	if len(s) < len(TokenPrefix) || !strings.EqualFold(s[:len(TokenPrefix)], TokenPrefix) {
		return Token{}, ErrTokenPrefix
	}
	s = s[len(TokenPrefix):]
	at := strings.IndexByte(s, '@')
	if at < 0 {
		return Token{}, ErrTokenAddrs
	}
	code, addrs := s[:at], s[at+1:]
	vals := make([]int, 0, tokenChars)
	for i := 0; i < len(code); i++ {
		if code[i] == '-' {
			continue
		}
		v := crockValue(code[i])
		if v < 0 {
			return Token{}, ErrTokenChars
		}
		if len(vals) == tokenChars {
			return Token{}, ErrTokenLength
		}
		vals = append(vals, v)
	}
	if len(vals) != tokenChars {
		return Token{}, ErrTokenLength
	}
	var t Token
	var err error
	if t.Secret, err = decodeField(vals[:tokenFieldChars]); err != nil {
		return Token{}, err
	}
	if t.Fingerprint, err = decodeField(vals[tokenFieldChars:]); err != nil {
		return Token{}, err
	}
	t.Addrs = strings.Split(addrs, ",")
	if err := checkAddrs(t.Addrs); err != nil {
		return Token{}, err
	}
	return t, nil
}

// SecretText is the secret as a join request carries it: its 26 base32
// characters (PROTOCOL-next 5.2).
func (t Token) SecretText() string { return encodeField(t.Secret) }

// DecodeSecret reads a join request's secret, with DecodeToken's rules.
func DecodeSecret(s string) ([TokenSecretBytes]byte, error) {
	if len(s) != tokenFieldChars {
		return [16]byte{}, ErrTokenLength
	}
	vals := make([]int, tokenFieldChars)
	for i := 0; i < len(s); i++ {
		if vals[i] = crockValue(s[i]); vals[i] < 0 {
			return [16]byte{}, ErrTokenChars
		}
	}
	return decodeField(vals)
}

func checkAddrs(addrs []string) error {
	if len(addrs) < 1 || len(addrs) > TokenMaxAddrs {
		return ErrTokenAddrs
	}
	for _, a := range addrs {
		if !ValidAddr(a) {
			return ErrTokenAddr
		}
	}
	return nil
}

// ValidAddr reports whether a is host:port as a token and a join request
// carry it: at most 255 bytes; the host a DNS name, an IPv4 address or an
// IPv6 address in brackets; the port 1 to 65535 in decimal without leading
// zeros.
func ValidAddr(a string) bool {
	if a == "" || len(a) > TokenMaxAddr {
		return false
	}
	host, port, err := net.SplitHostPort(a)
	if err != nil || host == "" {
		return false
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 || strconv.Itoa(p) != port {
		return false
	}
	if strings.HasPrefix(a, "[") {
		ip := net.ParseIP(host)
		return ip != nil && strings.Contains(host, ":") && !strings.Contains(host, "%")
	}
	if strings.Contains(host, ":") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.To4() != nil
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
