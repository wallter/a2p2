package node

// Step 5b fix round, item 11: the wire is checked at every level before
// anything binds.

import (
	"fmt"
	"strings"
	"testing"
)

var (
	wRoom = "r_" + strings.Repeat("ab", 16)
	wSum  = strings.Repeat("0", 64)
	wHead = strings.Repeat("1", 32)
	wNode = strings.Repeat("c", 64)
)

func openingBody(extra string) string {
	return fmt.Sprintf(`{"v":2,"room":%q,"sum":%q,"log":{"heads":[%q]}%s}`, wRoom, wSum, wHead, extra)
}

func detailBody(stream string, extra string) string {
	return fmt.Sprintf(`{"v":2,"room":%q,"snap":%q,"detail":{%q:[%s]},"log":{"heads":[%q]}%s}`,
		wRoom, wSum, wNode, stream, wHead, extra)
}

// [item 11] Requests refused for what encoding/json would let bind or
// pass: a nested key in another case, a non-integer token, an empty nb or
// w, a repeated w, and wait_ms or peers on a continuing request.
func TestPullRequestsAreCheckedAtEveryLevel(t *testing.T) {
	good := []string{
		openingBody(""),
		openingBody(`,"wait_ms":100,"peers":true`),
		detailBody(`{"e":"0123456789abcdef","c":3,"x":0}`, ""),
		detailBody(`{"e":"0123456789abcdef","c":3,"x":0,"nb":[2,3],"w":[3]}`, ""),
		fmt.Sprintf(`{"v":2,"room":%q,"snap":%q,"detail":{%q:[]},"log":{"heads":[%q]}}`, wRoom, wSum, wNode, wHead),
	}
	for _, b := range good {
		if _, perr := DecodePullRequest([]byte(b)); perr != nil {
			t.Errorf("refused %s: %v", b, perr)
		}
	}
	bad := map[string]string{
		"log.Heads":        fmt.Sprintf(`{"v":2,"room":%q,"sum":%q,"log":{"Heads":[%q]}}`, wRoom, wSum, wHead),
		"x -0":             detailBody(`{"e":"0123456789abcdef","c":3,"x":-0}`, ""),
		"c 3.0":            detailBody(`{"e":"0123456789abcdef","c":3.0,"x":0}`, ""),
		"c 3e0":            detailBody(`{"e":"0123456789abcdef","c":3e0,"x":0}`, ""),
		"empty nb":         detailBody(`{"e":"0123456789abcdef","c":3,"x":0,"nb":[]}`, ""),
		"empty w":          detailBody(`{"e":"0123456789abcdef","c":3,"x":0,"nb":[3],"w":[]}`, ""),
		"repeated w":       detailBody(`{"e":"0123456789abcdef","c":3,"x":0,"nb":[2,3],"w":[3,3]}`, ""),
		"detail E":         detailBody(`{"E":"0123456789abcdef","c":3,"x":0}`, ""),
		"wait_ms continue": detailBody(`{"e":"0123456789abcdef","c":3,"x":0}`, `,"wait_ms":5`),
		"peers continue":   detailBody(`{"e":"0123456789abcdef","c":3,"x":0}`, `,"peers":true`),
		"v -0":             strings.Replace(openingBody(""), `"v":2`, `"v":-0`, 1),
	}
	for name, b := range bad {
		if _, perr := DecodePullRequest([]byte(b)); perr == nil || perr.Status != 400 {
			t.Errorf("%s: accepted", name)
		}
	}
}

// [item 11] Responses: log heads required, peers only when asked and
// well formed, nested keys exact.
func TestPullResponsesAreCheckedAgainstTheirRequest(t *testing.T) {
	q := &PullRequest{V: PullVersion, Room: wRoom, Sum: wSum, Log: PullLog{Heads: []string{wHead}}}
	asked := *q
	asked.Peers = true
	peer := fmt.Sprintf(`"peers":[{"node":%q,"addrs":["192.0.2.1:7443"],"seen":"2026-04-01T09:00:00.000Z"}]`, wNode)
	cases := []struct {
		name string
		q    *PullRequest
		body string
		ok   bool
	}{
		{"same", q, fmt.Sprintf(`{"v":2,"same":true,"log":{"heads":[%q]}}`, wHead), true},
		{"no heads", q, `{"v":2,"same":true,"log":{"extra":["` + wHead + `"]}}`, false},
		{"log.Heads", q, fmt.Sprintf(`{"v":2,"same":true,"log":{"Heads":[%q]}}`, wHead), false},
		{"peers asked", &asked, fmt.Sprintf(`{"v":2,"same":true,"log":{"heads":[%q]},%s}`, wHead, peer), true},
		{"peers unasked", q, fmt.Sprintf(`{"v":2,"same":true,"log":{"heads":[%q]},%s}`, wHead, peer), false},
		{"peer bad node", &asked, fmt.Sprintf(`{"v":2,"same":true,"log":{"heads":[%q]},"peers":[{"node":"x","addrs":["a:1"]}]}`, wHead), false},
		{"peer no addrs", &asked, fmt.Sprintf(`{"v":2,"same":true,"log":{"heads":[%q]},"peers":[{"node":%q}]}`, wHead, wNode), false},
		{"peer bad seen", &asked, fmt.Sprintf(`{"v":2,"same":true,"log":{"heads":[%q]},"peers":[{"node":%q,"addrs":["a:1"],"seen":"yesterday"}]}`, wHead, wNode), false},
		{"peer Node", &asked, fmt.Sprintf(`{"v":2,"same":true,"log":{"heads":[%q]},"peers":[{"Node":%q,"addrs":["a:1"]}]}`, wHead, wNode), false},
	}
	for _, c := range cases {
		resp, err := DecodePullResponse([]byte(c.body))
		if err == nil {
			err = checkResponse(resp, c.q)
		}
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
