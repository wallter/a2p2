package core

// Attachments (PROTOCOL section 10, DESIGN section 12): the limits, the
// manifest a record's `files` header field commits to, and the rules for
// file names and media types. No I/O here.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"
)

// The limits of attachments, in one place (PROTOCOL 10.2 to 10.5, DESIGN
// 12 item 9). Tests lower some of them through hooks in the packages that
// use them, never here.
const (
	MaxFiles          = 8         // files in one manifest, and paths in one send
	MaxFileSize       = 256 << 20 // bytes of one file
	MaxManifestBytes  = 16384     // bytes of one manifest
	MaxFileName       = 255       // bytes of a file's name in a manifest
	MaxFileType       = 100       // bytes of a media type
	RoomFileBudget    = 1 << 30   // held, partial, staged and copied bytes per room
	MachineFileBudget = 4 << 30   // the same, for the whole machine
	FileChunk         = 8 << 20   // bytes in one response or upload
	MaxWants          = 16        // entries of want in one pull answer
	WantTries         = 3         // answers to the author's pulls without the bytes, then a want lapses
	ListTries         = 8         // failed answers for a missing manifest, then it is unavailable
	FileTransfers     = 8         // transfers at once, in each direction, for the machine
	MaxFetchWaitMS    = 120000    // the local API's fetch wait
	ToolFetchWaitMS   = 30000     // what the tool asks for
	MaxRangeDigits    = 15        // digits of a Range or Content-Range offset
	FileMinRate       = 64 << 10  // bytes a second a transfer must keep up
	saltBytes         = 16
)

// Durations of attachments.
const (
	WantLife        = 24 * time.Hour    // a want or a carry job lapses after this
	GaveUpLife      = 24 * time.Hour    // a lapsed carry is answered "gave up" for this long
	StagingLife     = 24 * time.Hour    // a send waiting for a person expires after this
	OldVersionLife  = 24 * time.Hour    // a peer that answered as 0.1.0 is skipped this long
	FileResponseMax = 120 * time.Second // one response or upload, in all
	FileRateWindow  = 30 * time.Second  // FileMinRate is kept over any window this long
	WantRepeat      = time.Minute       // a want is repeated at most this often
	ListRetryFirst  = time.Minute       // backoff for a missing manifest, doubling
	ListRetryMax    = time.Hour
)

// ValidSHA256 reports whether s is 64 lowercase hex.
func ValidSHA256(s string) bool { return isHex(s, 64) }

// SHA256Hex is the lowercase hex SHA-256 of b.
func SHA256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// FileRef is one file of a manifest.
type FileRef struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Type   string `json:"type,omitempty"`
}

// Manifest is a parsed manifest: its salt and its files, in order.
type Manifest struct {
	Salt  string
	Files []FileRef
}

// Why a manifest is not valid (PROTOCOL 10.2). A manifest whose bytes
// match the header's hash and break a rule is invalid for good.
var (
	ErrManifestJSON    = errors.New("file list: not strict json")
	ErrManifestVersion = errors.New("file list: unsupported version")
	ErrManifestField   = errors.New("file list: a field breaks its rule")
)

// ValidFileName is the rule for a manifest's name: 1 to 255 bytes of
// UTF-8, no C0 or C1 control, no '/' or '\'. Display only, never a path.
func ValidFileName(s string) bool {
	if s == "" || len(s) > MaxFileName || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

// ValidFileType is the rule for a media type: 1 to 100 bytes, token/token
// of lowercase ASCII letters, digits and !#$&^_.+- .
func ValidFileType(s string) bool {
	if s == "" || len(s) > MaxFileType {
		return false
	}
	slash := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '/':
			if slash >= 0 {
				return false
			}
			slash = i
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '!' || c == '#' || c == '$' || c == '&' || c == '^' || c == '_' || c == '.' || c == '+' || c == '-':
		default:
			return false
		}
	}
	return slash > 0 && slash < len(s)-1
}

// NewSalt is a manifest salt from 16 random bytes.
func NewSalt(random []byte) (string, error) {
	if len(random) != saltBytes {
		return "", errors.New("file list: salt needs 16 bytes")
	}
	return hex.EncodeToString(random), nil
}

// EncodeManifest writes a manifest's bytes: compact strict JSON in a fixed
// order. It refuses what ParseManifest would refuse.
func EncodeManifest(m Manifest) ([]byte, error) {
	var w jwriter
	w.int("v", 1)
	w.str("salt", m.Salt)
	b := []byte{'['}
	for i, f := range m.Files {
		if i > 0 {
			b = append(b, ',')
		}
		var fw jwriter
		fw.str("name", f.Name)
		fw.int("size", f.Size)
		fw.str("sha256", f.SHA256)
		fw.str("type", f.Type)
		b = append(b, fw.end()...)
	}
	w.raw("files", append(b, ']'))
	out := w.end()
	if _, err := ParseManifest(out); err != nil {
		return nil, err
	}
	return out, nil
}

// ParseManifest checks a manifest's bytes against PROTOCOL 10.2: strict
// JSON of at most 16,384 bytes; v 1; a salt of 32 lowercase hex; 1 to 8
// files, each with a valid name, a size of 1 to 256 MiB, a SHA-256 unique
// in the list and an optional valid type. Unknown keys are ignored.
func ParseManifest(b []byte) (Manifest, error) {
	if len(b) > MaxManifestBytes {
		return Manifest{}, ErrManifestField
	}
	v, err := parseStrict(b)
	if err != nil || v.kind != jObject || !v.noNull() {
		return Manifest{}, ErrManifestJSON
	}
	var m Manifest
	var ver int64
	var files *jvalue
	for i := range v.obj {
		k, x := v.obj[i].key, v.obj[i].val
		switch k {
		case "v":
			n, ok := x.integer()
			if !ok {
				return Manifest{}, ErrManifestField
			}
			ver = n
		case "salt":
			if x.kind != jString || !isHex(x.str, 2*saltBytes) {
				return Manifest{}, ErrManifestField
			}
			m.Salt = x.str
		case "files":
			files = &v.obj[i].val
		}
	}
	switch {
	case ver != 1:
		return Manifest{}, ErrManifestVersion
	case m.Salt == "" || files == nil || files.kind != jArray || len(files.arr) < 1 || len(files.arr) > MaxFiles:
		return Manifest{}, ErrManifestField
	}
	seen := map[string]bool{}
	for _, e := range files.arr {
		if e.kind != jObject {
			return Manifest{}, ErrManifestField
		}
		var f FileRef
		var hasSize bool
		for _, fm := range e.obj {
			x := fm.val
			switch fm.key {
			case "name":
				if x.kind != jString || !ValidFileName(x.str) {
					return Manifest{}, ErrManifestField
				}
				f.Name = x.str
			case "size":
				n, ok := x.integer()
				if !ok || n < 1 || n > MaxFileSize {
					return Manifest{}, ErrManifestField
				}
				f.Size, hasSize = n, true
			case "sha256":
				if x.kind != jString || !isHex(x.str, 64) {
					return Manifest{}, ErrManifestField
				}
				f.SHA256 = x.str
			case "type":
				if x.kind != jString || !ValidFileType(x.str) {
					return Manifest{}, ErrManifestField
				}
				f.Type = x.str
			}
		}
		if f.Name == "" || !hasSize || f.SHA256 == "" || seen[f.SHA256] {
			return Manifest{}, ErrManifestField
		}
		seen[f.SHA256] = true
		m.Files = append(m.Files, f)
	}
	return m, nil
}

// FormatOffset is a Range offset as the wire writes it.
func FormatOffset(n int64) string { return strconv.FormatInt(n, 10) }

// ParseOffset reads a Range or Content-Range offset: ASCII decimal digits
// only, at most 15, no sign or leading zero (but "0"). It never overflows.
func ParseOffset(s string) (int64, bool) {
	if s == "" || len(s) > MaxRangeDigits || len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}
