package node

// The pull exchange's wire types and its two-level summary (PROTOCOL-next
// sections 4.1 to 4.3, rules [R3], [R5] of internal/model/delivery). A
// summary is, per author, a digest of that author's detail; the detail of
// an author is, per stream, its contiguous position, the positions held
// without text and those whose text is wanted. Equal digests stand for
// equal summaries.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/store"
)

// PullVersion is the protocol version of a pull request and response.
const PullVersion = 2

// Limits of PROTOCOL-next 4.1, 4.2 and 4.6.
const (
	MaxPullAuthors    = 1024
	MaxDetailAuthors  = 16
	MaxLogHeads       = 64
	MaxLogExtra       = 256
	MaxLogEntries     = 256
	MaxPageRecords    = 512
	MaxOfferRecords   = 512
	MaxAddrs          = 8
	MaxAddrBytes      = 255
	MaxWaitMS         = 60000
	MaxRequestBytes   = 1 << 20
	MaxResponseBytes  = 4 << 20
	MaxPeersListed    = 64
	maxErrorDetail    = 200
	digestDomain      = "a2p2 digest v2"
	sumDomain         = "a2p2 sum v2"
	pullRoundsPerPull = 32 // a safety bound on rounds in one exchange
)

// The whole-message limits in force, and the extra a machine lists; tests
// lower them.
var (
	maxRequestBytes  = MaxRequestBytes
	maxResponseBytes = MaxResponseBytes
	maxLogExtra      = MaxLogExtra
)

// PullRequest is the body of POST /p/v2/pull (PROTOCOL-next 4.1): the
// opening request of a round carries sum (and seen), a continuing request
// snap and detail. Fields whose value would be zero or empty are omitted,
// as section 0 requires.
type PullRequest struct {
	V         int                             `json:"v"`
	Room      string                          `json:"room"`
	Sum       string                          `json:"sum,omitempty"`
	Seen      string                          `json:"seen,omitempty"`
	Snap      string                          `json:"snap,omitempty"`
	Detail    map[string][]store.StreamDetail `json:"detail,omitempty"`
	Log       PullLog                         `json:"log"`
	Offer     []json.RawMessage               `json:"offer,omitempty"`
	OfferMore bool                            `json:"offer_more,omitempty"`
	Addrs     []string                        `json:"addrs,omitempty"`
	WaitMS    int64                           `json:"wait_ms,omitempty"`
	// Peers asks the server to list other members' addresses in its
	// answer (opening requests only).
	Peers bool `json:"peers,omitempty"`
}

// PullLog is the log part of a request or response.
type PullLog struct {
	Heads   []string          `json:"heads,omitempty"`
	Extra   []string          `json:"extra,omitempty"`
	Entries []json.RawMessage `json:"entries,omitempty"`
	More    bool              `json:"more,omitempty"` // response only
}

// PullResponse is the 200 response (PROTOCOL-next 4.2): to an opening
// request same, or the server's authors, digests and sum; to a continuing
// one its detail and page.
type PullResponse struct {
	V       int                             `json:"v"`
	Same    bool                            `json:"same,omitempty"`
	Authors []string                        `json:"authors,omitempty"`
	Digests map[string]string               `json:"digests,omitempty"`
	Sum     string                          `json:"sum,omitempty"`
	Detail  map[string][]store.StreamDetail `json:"detail,omitempty"`
	Log     PullLog                         `json:"log"`
	Page    []json.RawMessage               `json:"page,omitempty"`
	More    bool                            `json:"more,omitempty"`
	Peers   []PullPeer                      `json:"peers,omitempty"`
}

// PullPeer is a member with addresses and when it was last heard from.
type PullPeer struct {
	Node  string   `json:"node"`
	Addrs []string `json:"addrs,omitempty"`
	Seen  string   `json:"seen,omitempty"`
}

// PullError is a pull refused with a status (PROTOCOL-next 4.5). Detail
// never carries a body; not_member carries nothing else at all.
type PullError struct {
	Status int    `json:"-"`
	Code   string `json:"error"`
	Detail string `json:"detail,omitempty"`
	// RetryAfter is the wait a 429 or 503 asks for (its Retry-After).
	RetryAfter time.Duration `json:"-"`
}

func (e *PullError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("pull refused: %d %s: %s", e.Status, e.Code, e.Detail)
	}
	return fmt.Sprintf("pull refused: %d %s", e.Status, e.Code)
}

// Status codes of PROTOCOL-next 4.5.
const (
	CodeBadRequest  = "bad_request"
	CodeNotMember   = "not_member"
	CodeTooLarge    = "too_large"
	CodeUnavailable = "unavailable"
)

func pullErr(status int, code, detail string) *PullError {
	if len(detail) > maxErrorDetail {
		detail = detail[:maxErrorDetail]
	}
	return &PullError{Status: status, Code: code, Detail: detail}
}

func badRequest(format string, a ...any) *PullError {
	return pullErr(400, CodeBadRequest, fmt.Sprintf(format, a...))
}

// notMember is the one not_member refusal: the same bytes for a room not
// carried here, a peer never a member and one removed (4.4), so a
// non-member learns nothing of the room or its log.
func notMember() *PullError { return pullErr(403, CodeNotMember, "") }

var errNotMember = notMember()

// digestOf is an author's digest: SHA-256 of the domain, a zero byte and
// one line per stream, in epoch order (PROTOCOL-next 4.3).
func digestOf(d []store.StreamDetail) string {
	h := sha256.New()
	h.Write([]byte(digestDomain))
	h.Write([]byte{0})
	for _, s := range d {
		fmt.Fprintf(h, "%s:%d:%d:%s:%s\n", s.E, s.C, s.X, joinInts(s.NB), joinInts(s.W))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sumOf is a summary's sum (and a round's snap) over the digests of the
// authors held anything of (4.3).
func sumOf(digests map[string]string) string {
	h := sha256.New()
	h.Write([]byte(sumDomain))
	h.Write([]byte{0})
	for _, a := range sortedIDs(digests) {
		fmt.Fprintf(h, "%s:%s\n", a, digests[a])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func joinInts(xs []int64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.FormatInt(x, 10)
	}
	return strings.Join(parts, ",")
}

// differ lists, sorted, every author in takes whose digest in a and b
// differs (one holding nothing of it has none).
func differ(takes []string, a, b map[string]string) []string {
	var out []string
	for _, k := range takes {
		if a[k] != b[k] {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// checkDetail checks one author's detail: streams sorted by epoch, each
// position at least 1, 0 <= x <= c, nb strictly ascending within (x, c],
// w a subset of nb.
func checkDetail(d []store.StreamDetail) error {
	prev := ""
	for _, s := range d {
		if !validEpoch(s.E) || s.E <= prev {
			return fmt.Errorf("detail epochs")
		}
		prev = s.E
		if s.C < 1 || s.C > 1<<53-1 {
			return fmt.Errorf("detail position")
		}
		if s.X < 0 || s.X > s.C {
			return fmt.Errorf("detail floor")
		}
		last := s.X
		for _, n := range s.NB {
			if n <= last || n > s.C {
				return fmt.Errorf("detail nb")
			}
			last = n
		}
		for _, n := range s.W {
			if !slices.Contains(s.NB, n) {
				return fmt.Errorf("detail w")
			}
		}
		for i := 1; i < len(s.W); i++ {
			if s.W[i] <= s.W[i-1] {
				return fmt.Errorf("detail w order")
			}
		}
	}
	return nil
}

func validEpoch(e string) bool {
	if len(e) != 16 || e == "0000000000000000" {
		return false
	}
	for _, c := range e {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func uniqueNodes(xs []string) bool {
	seen := map[string]bool{}
	for _, x := range xs {
		if !core.ValidNode(x) || seen[x] {
			return false
		}
		seen[x] = true
	}
	return true
}

func uniqueIDs(xs []string) bool {
	seen := map[string]bool{}
	for _, x := range xs {
		if len(x) != 32 || !isLowerHex(x) || seen[x] {
			return false
		}
		seen[x] = true
	}
	return true
}

// validate is step 2 of PROTOCOL-next 4.4 for what survives decoding:
// version and field limits. Strict JSON (duplicate keys, case of keys) is
// the HTTP decoder's job (step 5b).
func (q *PullRequest) validate() *PullError {
	switch {
	case q.V != PullVersion:
		return badRequest("v")
	case len(q.Room) != 34 || q.Room[:2] != "r_" || !isLowerHex(q.Room[2:]):
		return badRequest("room")
	case (q.Sum == "") == (q.Snap == ""):
		return badRequest("exactly one of sum or snap")
	case !hash(q.Sum) || !hash(q.Snap) || !hash(q.Seen):
		return badRequest("sum, seen or snap")
	case q.Seen != "" && q.Sum == "":
		return badRequest("seen goes with sum")
	case (q.Snap == "") != (q.Detail == nil):
		return badRequest("snap and detail go together")
	case len(q.Detail) > MaxDetailAuthors:
		return badRequest("detail")
	case len(q.Log.Heads) < 1 || len(q.Log.Heads) > MaxLogHeads || !uniqueIDs(q.Log.Heads):
		return badRequest("log heads")
	case len(q.Log.Extra) > MaxLogExtra || !uniqueIDs(q.Log.Extra):
		return badRequest("log extra")
	case len(q.Log.Entries) > MaxLogEntries:
		return badRequest("log entries")
	case q.Log.More:
		return badRequest("log more")
	case len(q.Offer) > MaxOfferRecords:
		return badRequest("offer")
	case q.OfferMore && len(q.Offer) == 0:
		return badRequest("offer_more")
	case len(q.Addrs) > MaxAddrs:
		return badRequest("addrs")
	case q.WaitMS < 0 || q.WaitMS > MaxWaitMS:
		return badRequest("wait_ms")
	case q.Sum == "" && (q.WaitMS != 0 || q.Peers):
		return badRequest("wait_ms and peers go with sum")
	}
	for a, d := range q.Detail {
		if !core.ValidNode(a) || d == nil || checkDetail(d) != nil {
			return badRequest("detail")
		}
	}
	for _, a := range q.Addrs {
		if a == "" || len(a) > MaxAddrBytes {
			return badRequest("addr")
		}
	}
	return nil
}

// hash reports whether s is empty or a SHA-256 in lowercase hex.
func hash(s string) bool { return s == "" || len(s) == 64 && isLowerHex(s) }

func isLowerHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
