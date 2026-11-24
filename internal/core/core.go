// Package core defines the signed formats (records, log entries, the
// invite token), their limits and the labels and ids every other package
// checks. It does no I/O.
package core

import (
	"regexp"
	"strings"
)

// Limits shared with the surfaces.
const (
	MaxBodyBytes = maxBody // a record's encoded body: what one message's text may take
	MaxLabel     = 64
)

// Kind is a record's kind (PROTOCOL-next section 1).
type Kind string

// KindMessage is ordinary text; the other kinds are in record.go.
const KindMessage Kind = "message"

var (
	nodeRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	roomRe  = regexp.MustCompile(`^(local|r_[0-9a-f]{32})$`)
	labelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// ShortNode is the 8-character fingerprint used in agent ids and displays.
func ShortNode(node string) string {
	if len(node) < 8 {
		return node
	}
	return node[:8]
}

// AgentID joins a label and the node it lives on.
func AgentID(label, node string) string { return label + "@" + ShortNode(node) }

// ValidNode, ValidRoom and ValidLabel are used at API boundaries.
func ValidNode(s string) bool  { return nodeRe.MatchString(s) }
func ValidRoom(s string) bool  { return roomRe.MatchString(s) }
func ValidLabel(s string) bool { return labelRe.MatchString(s) }

// CleanLabel turns arbitrary text (a directory name, a hostname) into a label.
func CleanLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-._")
	if len(out) > MaxLabel-8 { // leave room for a "-N" uniqueness suffix
		out = out[:MaxLabel-8]
	}
	if out == "" {
		out = "agent"
	}
	return out
}
