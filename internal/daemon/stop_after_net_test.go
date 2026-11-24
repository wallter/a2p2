package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// A stop that arrives while the daemon is applying a networking decision is
// not lost: the daemon exits. (Found when a test stopped a daemon right
// after `net on` and waited for ever.)
//
// Requirements: D6
func TestStopRightAfterANetworkingDecisionStillStopsTheDaemon(t *testing.T) {
	for i := 0; i < 5; i++ {
		m := newMachine(t, "racer")
		var res localapi.NetResp
		if err := m.c.Call(context.Background(), "net",
			localapi.NetReq{State: node.NetworkOn, Listen: m.ln.ln.Addr().String()}, &res); err != nil {
			t.Fatal(err)
		}
		done := m.done
		_ = m.c.Call(context.Background(), "stop", nil, nil)
		select {
		case <-done:
			m.done = nil
		case <-time.After(15 * time.Second):
			t.Fatalf("round %d: the daemon did not stop after net on followed at once by stop", i)
		}
	}
}
