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

// withOutboundStatus answers the local API's status with the network
// outbound only and no addresses listening.
func withOutboundStatus() func(http.Handler) http.Handler {
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
			st.Network.State = node.NetworkOutbound
			st.Network.Applied = ""
			st.Listen = nil
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(st)
		})
	}
}

// doctor says an outbound-only machine is outbound only, not off.
func TestDoctorSaysAnOutboundOnlyMachineIsOutboundOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("state directory modes are a Unix rule")
	}
	d := newDaemonForCLIWith(t, withOutboundStatus())
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
		f := strings.Fields(l)
		if len(f) >= 2 && f[1] == "network" {
			line = l
		}
	}
	if !strings.Contains(line, "outbound only") {
		t.Fatalf("doctor's network line: %q\n%s", line, buf.String())
	}
	if strings.Contains(line, "off") {
		t.Fatalf("doctor's network line: %q\n%s", line, buf.String())
	}
}
