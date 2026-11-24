package core

import (
	"errors"
	"strings"
)

// ErrNotStrict is CheckStrictObject's refusal; it never quotes the input.
var ErrNotStrict = errors.New("not strict json")

// CheckStrictObject checks a peer request or response body against
// PROTOCOL-next section 0 before encoding/json decodes it: one strict JSON
// object (valid UTF-8, no lone surrogate, no duplicate key after
// unescaping, nothing after it, at most 16 levels deep), no null at any
// depth, every field named in known present with a non-zero, non-empty,
// non-false value, and no other key that differs from a known one only in
// case (encoding/json would match it). Other unknown fields may hold any
// value but null.
func CheckStrictObject(b []byte, known ...string) error {
	v, err := parseStrict(b)
	if err != nil || v.kind != jObject {
		return ErrNotStrict
	}
	for _, m := range v.obj {
		isKnown := false
		for _, k := range known {
			if k == m.key {
				isKnown = true
			} else if strings.EqualFold(k, m.key) {
				return ErrNotStrict
			}
		}
		if isKnown && !m.val.present() || !m.val.noNull() {
			return ErrNotStrict
		}
	}
	return nil
}
