package node

// Bodies of POST /p/v2/pull as the HTTP layer (internal/peer) carries
// them: strict JSON (PROTOCOL-next section 0) checked here, like the join
// exchange's, so the rules of the wire live with the exchange.

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/wallter/a2p2/internal/core"
)

// The top-level fields of a request and a response (PROTOCOL-next 4.1,
// 4.2): each, if present, must hold a non-zero value at every depth. All
// but detail: section 4.3 gives detail its own form, in which an author
// held nothing of is [] and a floor x of 0 is written (checkDetail checks
// it), so for detail strictness is no null, no key differing from it only
// in case, and a non-empty object.
var (
	pullRequestFields  = []string{"v", "room", "sum", "seen", "snap", "log", "offer", "offer_more", "addrs", "wait_ms"}
	pullResponseFields = []string{"v", "same", "authors", "digests", "sum", "log", "page", "more", "peers"}
	pullErrorFields    = []string{"error", "detail"}
)

// strictPull is CheckStrictObject with detail checked as above.
func strictPull(body []byte, known []string) bool {
	if core.CheckStrictObject(body, known...) != nil {
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

// DecodePullRequest is step 2 of PROTOCOL-next 4.4 for a body already
// within the size limit: strict JSON, then the version and field limits.
// Its refusal is a 400.
func DecodePullRequest(body []byte) (*PullRequest, *PullError) {
	var q PullRequest
	if !strictPull(body, pullRequestFields) || json.Unmarshal(body, &q) != nil {
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
	if !strictPull(body, pullResponseFields) || json.Unmarshal(body, &resp) != nil {
		return nil, errNotStrictResponse
	}
	return &resp, nil
}

var errNotStrictResponse = &UserError{"pull: the response is not strict json"}

// DecodePullError maps a refusal's status and body (PROTOCOL-next 4.5) to
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
