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

// doctorOutput runs the real doctor path on a state directory with the given
// mode, with no daemon to start and with LookPath finding the named commands.
func doctorOutput(t *testing.T, mode os.FileMode, onPath ...string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("state directory modes are a Unix rule")
	}
	connectDaemon = func(context.Context, string) (*localapi.Client, error) {
		return nil, errors.New("no daemon in this test")
	}
	t.Cleanup(func() { connectDaemon = daemon.Connect })
	home := t.TempDir()
	if err := os.Chmod(home, mode); err != nil {
		t.Fatal(err)
	}
	c := newCmd(context.Background(), "doctor")
	c.home = home
	o := harness.Options{HomeDir: t.TempDir(), StateDir: t.TempDir(), Binary: "/opt/a2p2", OS: "linux",
		LookPath: func(f string) (string, error) {
			for _, n := range onPath {
				if n == f {
					return "/bin/" + f, nil
				}
			}
			return "", errors.New("not on PATH")
		}}
	var out bytes.Buffer
	err := runDoctor(c, &out, o)
	return out.String(), err
}

func TestDoctorOnAFreshMachineSaysHowToConnectAHarness(t *testing.T) {
	want := "No harness is connected. Run: a2p2 install claude (or codex, agy, opencode, hermes, pi), then a2p2 doctor again.\n"
	// No harness command on the PATH: no line for any harness, only the hint.
	got, _ := doctorOutput(t, 0o700)
	if !strings.HasSuffix(got, want) || strings.Contains(got, "claude hook") {
		t.Fatalf("no harness on the PATH:\n%s", got)
	}
	// A harness present but not connected: its lines say so, and the hint follows.
	got, _ = doctorOutput(t, 0o700, "claude")
	if !strings.Contains(got, "claude hook") || !strings.HasSuffix(got, want) {
		t.Fatalf("a harness not connected:\n%s", got)
	}
}

func TestDoctorHintListsTheHarnessesInstallAccepts(t *testing.T) {
	h := noHarnessHint()
	for _, n := range harness.Names {
		if !strings.Contains(h, n) {
			t.Errorf("the hint %q lacks %s", h, n)
		}
	}
}

func TestDoctorDoesNotPointAtTheLogWhenTheStateDirectoryFailed(t *testing.T) {
	got, err := doctorOutput(t, 0o755)
	if err == nil {
		t.Fatalf("an unsafe state directory must still fail doctor:\n%s", got)
	}
	if !strings.Contains(got, "not checked: fix the state directory first") || strings.Contains(got, "daemon.log") {
		t.Fatalf("daemon line after a state directory failure:\n%s", got)
	}
}

func TestHelpEndsWithTheDocsLine(t *testing.T) {
	if !strings.HasSuffix(usage, "\nDocs and source: https://github.com/wallter/a2p2\n") {
		t.Fatalf("help ends with %q", usage[len(usage)-80:])
	}
}
