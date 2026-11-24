package node

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/wallter/a2p2/internal/core"
)

// memNet is the in-memory transport of the pull exchange: every request
// and response is encoded to JSON and decoded again, as the HTTP endpoint
// will (step 5b), and the requester's node id is passed to the server as
// the transport authenticated it. It injects faults: machines that cannot
// be reached (asleep) or take no connections, a lost response (the server
// acted, the requester never hears), and a relay that alters what it
// sends.
type memNet struct {
	mu        sync.Mutex
	nodes     map[string]*Exchange
	down      map[string]bool
	noInbound map[string]bool
	// lose, if set, says whether the response to req is lost.
	lose func(from, to string, req *PullRequest, resp *PullResponse) bool
	// tamper, if set, rewrites a response before the requester decodes it.
	tamper func(from, to string, resp *PullResponse)
	// sizes of every request and response, in bytes, in order
	reqBytes, respBytes []int
	// texts sent: ids of records whose text went in a page, or in an offer
	pageText, offerText []string
}

var errUnreachable = errors.New("peer unreachable")
var errLost = errors.New("response lost")

func newMemNet() *memNet {
	return &memNet{nodes: map[string]*Exchange{}, down: map[string]bool{}, noInbound: map[string]bool{}}
}

func (n *memNet) attach(id string, x *Exchange) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nodes[id] = x
}

func (n *memNet) link(from string) PullTransport { return memLink{n, from} }

type memLink struct {
	net  *memNet
	from string
}

func (l memLink) Pull(ctx context.Context, peer string, req *PullRequest) (*PullResponse, error) {
	n := l.net
	n.mu.Lock()
	x := n.nodes[peer]
	unreachable := x == nil || n.down[peer] || n.down[l.from] || n.noInbound[peer]
	n.mu.Unlock()
	if unreachable {
		return nil, errUnreachable
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.reqBytes = append(n.reqBytes, len(b))
	n.mu.Unlock()
	if len(b) > maxRequestBytes {
		return nil, pullErr(413, CodeTooLarge, "")
	}
	var q PullRequest
	if err := json.Unmarshal(b, &q); err != nil {
		return nil, pullErr(400, CodeBadRequest, "json")
	}
	n.mu.Lock()
	n.offerText = append(n.offerText, withText(q.Offer)...)
	n.mu.Unlock()
	resp, err := x.Serve(ctx, l.from, &q)
	if err != nil {
		var pe *PullError
		if errors.As(err, &pe) {
			c := *pe
			return nil, &c
		}
		return nil, err
	}
	rb, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	n.respBytes = append(n.respBytes, len(rb))
	n.pageText = append(n.pageText, withText(resp.Page)...)
	lose, tamper := n.lose, n.tamper
	n.mu.Unlock()
	var out PullResponse
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, err
	}
	if lose != nil && lose(l.from, peer, &q, &out) {
		return nil, errLost
	}
	if tamper != nil {
		tamper(l.from, peer, &out)
	}
	return &out, nil
}

// withText returns the ids of the records in wires sent with text.
func withText(wires []json.RawMessage) []string {
	var out []string
	for _, w := range wires {
		if r, err := core.ParseRecord(w); err == nil && len(r.Body()) > 0 {
			out = append(out, r.ID())
		}
	}
	return out
}
