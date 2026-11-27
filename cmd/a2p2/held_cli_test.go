package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/daemon"
	"github.com/wallter/a2p2/internal/harness"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
)

// withHeldPeer answers the local API's status with one more machine, for
// which held addresses are reported (as a daemon whose book loaded them
// does: TestAStoredAddressTheRuleRefusesIsShownAsHeldInStatus).
func withHeldPeer(p localapi.Peer) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/status") {
				h.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			var st localapi.Status
			if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			st.Peers = append(st.Peers, p)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(st)
		})
	}
}

// D71, from the local API to the terminal: the held addresses the local
// API reports for a machine are printed by status, under that machine,
// and by doctor, on that machine's line, with why and what to do.
func TestStatusAndDoctorPrintTheHeldAddressesTheLocalAPIReports(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("state directory modes are a Unix rule")
	}
	held := "127.0.0.1:6379"
	p := localapi.Peer{Node: strings.Repeat("c", 64), Name: "gamma", Addrs: []string{held, "198.51.100.7:7423"},
		Held: []node.HeldAddr{{Addr: held, Why: node.AddrRefusal(held)}}}
	d := newDaemonForCLIWith(t, withHeldPeer(p))
	want := heldText(p.Held)
	if !strings.Contains(want, "a2p2 join") || !strings.Contains(want, "loopback") {
		t.Fatalf("held line: %q", want)
	}
	out, _, err := d.run(t, false, "", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "gamma") || !strings.Contains(out, "\n    "+want+"\n") {
		t.Fatalf("status:\n%s", out)
	}

	connectDaemon = func(context.Context, string) (*localapi.Client, error) {
		return nil, errors.New("the test's client is set")
	}
	t.Cleanup(func() { connectDaemon = daemon.Connect })
	c := newCmd(context.Background(), "doctor")
	c.home = t.TempDir()
	if err := os.Chmod(c.home, 0o700); err != nil {
		t.Fatal(err)
	}
	c.cl = d.cl
	var buf bytes.Buffer
	o := harness.Options{HomeDir: t.TempDir(), StateDir: t.TempDir(), Binary: "/opt/a2p2", OS: "linux",
		LookPath: func(string) (string, error) { return "", errors.New("not on PATH") }}
	_ = runDoctor(c, &buf, o)
	line := ""
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "gamma") {
			line = l
		}
	}
	if !strings.Contains(line, "; "+want) {
		t.Fatalf("doctor's line for the machine: %q\n%s", line, buf.String())
	}
}
