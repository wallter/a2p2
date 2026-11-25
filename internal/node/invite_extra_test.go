package node

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// inviteAddrsOf opens a surface advertising adv, with extra as the
// addresses an invite carries beyond them, and returns the addresses of
// the first invite it issues.
func inviteAddrsOf(t *testing.T, adv, extra []string) []string {
	t.Helper()
	e := newSurf(t)
	e.net = Network{State: NetworkOn, Listen: "127.0.0.1:7423"}
	e.addrs = adv
	e.open()
	if len(extra) > 0 {
		e.s.extra = func() []string { return extra }
	}
	ctx := context.Background()
	if _, err := e.s.CreateRoom(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	inv, err := e.s.Invite(ctx, "team", "member", false)
	if err != nil {
		t.Fatal(err)
	}
	return mustToken(t, inv.Token).Addrs
}

func TestAFirstInviteCarriesTheMappedAddressBeforeAMemberReachedIt(t *testing.T) {
	lan := []string{"192.168.1.5:7423", "10.0.0.5:7423"}
	got := inviteAddrsOf(t, lan, []string{"203.0.113.9:7423"})
	want := []string{"203.0.113.9:7423", "192.168.1.5:7423", "10.0.0.5:7423"}
	if !slices.Equal(got, want) {
		t.Fatalf("invite addresses %v, want %v", got, want)
	}
}

func TestTheMappedAddressKeepsItsSlotInAnInviteWhenAdvertisedAddressesFillTheCap(t *testing.T) {
	var adv []string
	for i := 1; i <= MaxAddrs; i++ {
		adv = append(adv, fmt.Sprintf("192.168.1.%d:7423", i))
	}
	got := inviteAddrsOf(t, adv, []string{"203.0.113.9:7423"})
	if len(got) != MaxAddrs || got[0] != "203.0.113.9:7423" || got[MaxAddrs-1] != adv[MaxAddrs-2] {
		t.Fatalf("invite addresses %v", got)
	}
}

func TestAnInviteIsUnchangedWithoutAMappingAndRepeatsNothingAlreadyAdvertised(t *testing.T) {
	lan := []string{"192.168.1.5:7423", "10.0.0.5:7423"}
	if got := inviteAddrsOf(t, lan, nil); !slices.Equal(got, lan) {
		t.Fatalf("without a mapping: %v", got)
	}
	both := []string{"203.0.113.9:7423", "192.168.1.5:7423"}
	if got := inviteAddrsOf(t, both, []string{"203.0.113.9:7423"}); !slices.Equal(got, both) {
		t.Fatalf("with the mapping already advertised: %v", got)
	}
}
