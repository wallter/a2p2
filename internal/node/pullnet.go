package node

// Bodies of POST /p/v2/pull as the HTTP layer (internal/peer) carries
// them: strict JSON (PROTOCOL section 0) checked here, like the join
// exchange's, so the rules of the wire live with the exchange.

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// The top-level fields of a request and a response (PROTOCOL 4.1,
// 4.2): each, if present, must hold a non-zero value at every depth. All
// but detail: section 4.3 gives detail its own form, in which an author
// held nothing of is [] and a floor x of 0 is written (checkDetail checks
// it), so for detail strictness is no null, no key differing from it only
// in case, and a non-empty object.
var (
	pullRequestFields  = []string{"v", "room", "sum", "seen", "snap", "log", "offer", "offer_more", "addrs", "wait_ms", "peers"}
	pullResponseFields = []string{"v", "same", "authors", "digests", "sum", "log", "page", "more", "peers", "want"}
	pullErrorFields    = []string{"error", "detail"}
)

// shape is the keys an object of the wire knows, each with the shape of
// its value (nil: no further shape); "*" stands for any key, "[]" for the
// elements of an array.
type shape map[string]shape

var (
	streamShape  = shape{"e": nil, "c": nil, "x": nil, "nb": nil, "w": nil}
	detailShape  = shape{"*": shape{"[]": streamShape}}
	logShape     = shape{"heads": nil, "extra": nil, "entries": nil, "more": nil}
	recordsShape = shape{"[]": shape{"h": nil, "s": nil, "b": nil}}
	requestShape = shape{"v": nil, "room": nil, "sum": nil, "seen": nil, "snap": nil, "detail": detailShape,
		"log": logShape, "offer": recordsShape, "offer_more": nil, "addrs": nil, "wait_ms": nil, "peers": nil}
	responseShape = shape{"v": nil, "same": nil, "authors": nil, "digests": shape{"*": nil}, "sum": nil,
		"detail": detailShape, "log": logShape, "page": recordsShape, "more": nil,
		"peers": shape{"[]": shape{"node": nil, "addrs": nil, "seen": nil}},
		"want":  shape{"[]": shape{"m": nil, "v": nil, "h": nil}}}
)

// exactAt checks a value decoded with UseNumber against sh: every number
// an Integer of section 0 (no sign, fraction or exponent, at most
// 2^53-1), no key of a known object that differs from a known one only in
// case (encoding/json would bind it), and in a stream of detail no empty
// nb or w (they are omitted when empty).
func exactAt(v any, sh shape) bool {
	switch v := v.(type) {
	case json.Number:
		s := v.String()
		if s == "" || len(s) > 16 || len(s) > 1 && s[0] == '0' {
			return false
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				return false
			}
		}
		n, err := v.Int64()
		return err == nil && n <= 1<<53-1
	case []any:
		var el shape
		if sh != nil {
			el = sh["[]"]
		}
		for _, e := range v {
			if !exactAt(e, el) {
				return false
			}
		}
	case map[string]any:
		for k, val := range v {
			var sub shape
			if sh != nil {
				if s, ok := sh[k]; ok {
					sub = s
				} else if s, ok := sh["*"]; ok {
					sub = s
				} else {
					for known := range sh {
						if strings.EqualFold(known, k) {
							return false
						}
					}
				}
			}
			if isStream(sh) && (k == "nb" || k == "w") {
				if a, ok := val.([]any); ok && len(a) == 0 {
					return false
				}
			}
			if !exactAt(val, sub) {
				return false
			}
		}
	}
	return true
}

func isStream(sh shape) bool {
	_, e := sh["e"]
	_, nb := sh["nb"]
	return e && nb
}

// exact is exactAt over a whole body.
func exact(body []byte, sh shape) bool {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var v any
	return d.Decode(&v) == nil && exactAt(v, sh)
}

// strictPull is CheckStrictObject with detail checked as above, then
// exact over the whole body.
func strictPull(body []byte, known []string, sh shape) bool {
	if core.CheckStrictObject(body, known...) != nil || !exact(body, sh) {
		return false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return false
	}
	for k, v := range top {
		switch {
		case k == "detail":
			var d map[string]json.RawMessage
			if json.Unmarshal(v, &d) != nil || len(d) == 0 {
				return false
			}
		case strings.EqualFold(k, "detail"):
			return false
		}
	}
	return true
}

// DecodePullRequest is step 2 of PROTOCOL 4.4 for a body already
// within the size limit: strict JSON, then the version and field limits.
// Its refusal is a 400.
func DecodePullRequest(body []byte) (*PullRequest, *PullError) {
	var q PullRequest
	if !strictPull(body, pullRequestFields, requestShape) || json.Unmarshal(body, &q) != nil {
		return nil, badRequest("not strict json")
	}
	if perr := q.validate(); perr != nil {
		return nil, perr
	}
	return &q, nil
}

// DecodePullResponse decodes a 200 body; Exchange.Pull checks its shape
// against its request.
func DecodePullResponse(body []byte) (*PullResponse, error) {
	var resp PullResponse
	if !strictPull(body, pullResponseFields, responseShape) || json.Unmarshal(body, &resp) != nil {
		return nil, errNotStrictResponse
	}
	return &resp, nil
}

var errNotStrictResponse = &UserError{Msg: "pull: the response is not strict json"}

// DecodePullError maps a refusal's status and body (PROTOCOL 4.5) to
// a *PullError. A code that does not fit the status, or a body that is not
// an error body, gives the status's own code; retry is its Retry-After.
func DecodePullError(status int, body []byte, retry time.Duration) *PullError {
	var e PullError
	if core.CheckStrictObject(body, pullErrorFields...) != nil || json.Unmarshal(body, &e) != nil {
		e = PullError{}
	}
	want := map[int]string{400: CodeBadRequest, 403: CodeNotMember, 413: CodeTooLarge, 429: CodeRateLimited,
		503: CodeUnavailable}[status]
	if want == "" {
		want = CodeUnavailable // anything else: transient as far as the requester knows
	}
	if e.Code != want {
		e.Code, e.Detail = want, ""
	}
	out := pullErr(status, e.Code, e.Detail)
	if status == 403 {
		out.Detail = ""
	}
	out.RetryAfter = retry
	return out
}

// PullErrorBody is the body of a refusal: {"error":…,"detail":…}.
func PullErrorBody(e *PullError) []byte {
	b, _ := json.Marshal(pullErr(e.Status, e.Code, e.Detail))
	return b
}
