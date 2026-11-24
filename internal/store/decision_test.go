package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The latest networking decision is the one recorded last, whatever the
// clock said (it went back between them) and whatever the ids sort to.
func TestTheLatestDecisionIsTheOneCommittedLast(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "a2p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for _, q := range []Request{
		{ID: "q_ffff", Kind: RequestNetwork, Action: "on", Name: "0.0.0.0:7423", Created: at, DecidedAt: at},
		{ID: "q_0000", Kind: RequestNetwork, Action: "off", Created: at.Add(-time.Hour), DecidedAt: at.Add(-time.Hour)},
		{ID: "q_8888", Kind: RequestNetwork, Action: "outbound", Created: at.Add(-time.Hour), DecidedAt: at.Add(-time.Hour)},
	} {
		if err := s.RecordDecision(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	q, ok, err := s.LatestDecision(ctx, RequestNetwork)
	if err != nil || !ok || q.ID != "q_8888" || q.Action != "outbound" {
		t.Fatalf("latest: %+v %v %v", q, ok, err)
	}
}
