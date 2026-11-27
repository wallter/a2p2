package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/daemon"
	"github.com/wallter/a2p2/internal/harness"
	"github.com/wallter/a2p2/internal/localapi"
)

func TestDaemonVersionNoteSaysOlderNewerOrDifferent(t *testing.T) {
	for _, c := range []struct {
		daemon, binary string
		cmp            int
		has            string
	}{
		{"v0.3.0", "v0.3.0", 0, ""},
		{"", "v0.3.0", 0, ""},
		{"v0.2.3", "v0.3.0", -1, "the daemon is older. Run a2p2 stop"},
		{"v0.3.1", "v0.3.0", 1, "the daemon is newer"},
		{"v0.10.0", "v0.9.0", 1, "the daemon is newer"},
		{"dev", "v0.3.0", 0, "they differ. Run a2p2 stop"},
	} {
		cmp, line := daemonVersionNote(c.daemon, c.binary)
		if cmp != c.cmp || (c.has == "") != (line == "") || !strings.Contains(line, c.has) {
			t.Errorf("%q vs %q: %d %q", c.daemon, c.binary, cmp, line)
		}
	}
}

// A daemon of another version than the binary: status prints one line
// saying so, and doctor fails for an older daemon (naming a2p2 stop) and
// only warns for a newer one.
func TestStatusAndDoctorSayWhenTheDaemonIsAnotherVersionThanTheBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("state directory modes are a Unix rule")
	}
	old := version
	version = "v0.3.0"
	t.Cleanup(func() { version = old })
	connectDaemon = func(context.Context, string) (*localapi.Client, error) {
		return nil, errors.New("the test's client is set")
	}
	t.Cleanup(func() { connectDaemon = daemon.Connect })
	for _, c := range []struct {
		daemon, word string
		fails        bool
	}{{"v0.2.3", "older", true}, {"v0.4.0", "newer", false}, {"v0.3.0", "", false}} {
		d := newDaemonForCLIInfo(t, nil, localapi.Info{Version: c.daemon})
		out, _, err := d.run(t, false, "", "status")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(out, "the daemon is "+c.word); got != (c.word != "") {
			t.Errorf("daemon %s, status:\n%s", c.daemon, out)
		}
		cm := newCmd(context.Background(), "doctor")
		cm.home = t.TempDir()
		if err := os.Chmod(cm.home, 0o700); err != nil {
			t.Fatal(err)
		}
		cm.cl = d.cl
		var buf bytes.Buffer
		o := harness.Options{HomeDir: t.TempDir(), StateDir: t.TempDir(), Binary: "/opt/a2p2", OS: "linux",
			LookPath: func(string) (string, error) { return "", errors.New("not on PATH") }}
		_ = runDoctor(cm, &buf, o)
		line := ""
		for _, l := range strings.Split(buf.String(), "\n") {
			if strings.Contains(l, "daemon version") {
				line += l + "\n"
			}
		}
		switch {
		case c.word == "":
			if line != "" {
				t.Errorf("same version, doctor:\n%s", buf.String())
			}
		case c.fails:
			if !strings.HasPrefix(line, "FAIL") || !strings.Contains(buf.String(), "fix: a2p2 stop") {
				t.Errorf("older daemon, doctor:\n%s", buf.String())
			}
		default:
			if !strings.HasPrefix(line, "warn") {
				t.Errorf("newer daemon, doctor:\n%s", buf.String())
			}
		}
	}
}

func TestSendNamesUnregisteredAddresseesInOneSentence(t *testing.T) {
	const tail = "the message is unread for"
	one := notRegisteredLine([]string{"a@1"})
	if !strings.HasPrefix(one, "a@1 has not registered on this machine yet; "+tail+" it when it registers") {
		t.Fatalf("one: %q", one)
	}
	if got := notRegisteredLine([]string{"a@1", "b@1", "c@1"}); !strings.HasPrefix(got, "a@1, b@1, c@1 have not registered") || strings.Count(got, "registered") != 1 {
		t.Fatalf("three: %q", got)
	}
	five := notRegisteredLine([]string{"a@1", "b@1", "c@1", "d@1", "e@1"})
	if !strings.HasPrefix(five, "a@1, b@1, c@1 and 2 more have not registered") || strings.Contains(five, "d@1") || strings.Count(five, tail) != 1 {
		t.Fatalf("five: %q", five)
	}
	if notRegisteredLine(nil) != "" {
		t.Fatal("none must print nothing")
	}
}
