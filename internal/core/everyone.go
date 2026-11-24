package core

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// EveryoneMark begins the text of a room-wide post marked for everyone.
const EveryoneMark = "@everyone"

// MarkedForEveryone reports whether a room-wide post's text marks it for
// everyone, so that agents at reading level "addressed" receive it
// (D-reading): the text begins with "@everyone" (ASCII, any case),
// followed by whitespace, punctuation or the end of the text. The mark is
// part of the signed text; no field of the record carries it. The rule is
// the delivery model's (internal/model/delivery/levels.go).
func MarkedForEveryone(text string) bool {
	n := len(EveryoneMark)
	if len(text) < n || !strings.EqualFold(text[:n], EveryoneMark) {
		return false
	}
	if len(text) == n {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[n:])
	return unicode.IsSpace(r) || unicode.IsPunct(r)
}
