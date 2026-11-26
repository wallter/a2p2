package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/localapi"
)

// statusWith answers status with the networking state n; every other
// call goes to the daemon.
func statusWith(n localapi.Network) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/status" {
				_ = json.NewEncoder(w).Encode(localapi.Status{Network: n})
				return
			}
			h.ServeHTTP(w, r)
		})
	}
}

func cliHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("A2P2_HOME", home)
}

// D68: port mapping that an earlier version recorded is not applied, and
// net on says so before the person answers: what this decision does with
// it, and how to turn it on again.
func TestNetOnSaysThatPortMappingFromAnEarlierVersionIsOff(t *testing.T) {
	cliHome(t)
	d := newDaemonForCLIWith(t, statusWith(localapi.Network{State: "on", Listen: "0.0.0.0:7423", MapPortEarlier: true}))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"on", "127.0.0.1:0"}, "keeps it off. Add --map-port to turn it on again."},
		{[]string{"on", "127.0.0.1:0", "--map-port"}, "this decision turns it on again."},
		{[]string{"off"}, "this decision keeps it off."},
	} {
		_, serr, err := d.run(t, true, "no\n", "net", tc.args...)
		if err == nil || !strings.Contains(serr, "turned on by an earlier version") || !strings.Contains(serr, tc.want) ||
			strings.Index(serr, tc.want) > strings.Index(serr, "Apply this?") {
			t.Fatalf("net %v: %q %v", tc.args, serr, err)
		}
	}
}

// The honest case: with no such decision, net on says nothing of it.
func TestNetOnSaysNothingOfAnEarlierPortMappingWhenThereIsNone(t *testing.T) {
	cliHome(t)
	d := newDaemonForCLIWith(t, statusWith(localapi.Network{State: "on", Listen: "0.0.0.0:7423", MapPort: true}))
	_, serr, err := d.run(t, true, "no\n", "net", "on", "127.0.0.1:0", "--map-port")
	if err == nil || strings.Contains(serr, "earlier version") || !strings.Contains(serr, "forward the listening port") {
		t.Fatalf("net on: %q %v", serr, err)
	}
}
