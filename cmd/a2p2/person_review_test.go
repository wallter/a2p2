package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wallter/a2p2/internal/harness"
)

// Agy, second review: a person at a terminal who pipes text in (echo hi |
// a2p2 send, or a here-document) has no terminal on stdin, and was
// refused. stderr is still the terminal there, which a harness's tool
// never leaves it, so the CLI acts as the person; with neither, it still
// refuses.
func TestAPersonPipingTextAtATerminalSendsAsThePerson(t *testing.T) {
	d := newDaemonForCLI(t)
	pipe := func(text string) {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		w.WriteString(text)
		w.Close()
		old := os.Stdin
		os.Stdin = r
		t.Cleanup(func() { os.Stdin = old; r.Close() })
	}
	old := stderrTerminal
	t.Cleanup(func() { stderrTerminal = old })
	stderrTerminal = func() bool { return true }
	pipe("piped by the person\n")
	if _, _, err := d.run(t, false, "", "send", "--room", "team"); err != nil {
		t.Fatalf("piped at a terminal: %v", err)
	}
	stderrTerminal = func() bool { return false }
	pipe("piped by a script\n")
	if _, _, err := d.run(t, false, "", "send", "--room", "team"); !errors.Is(err, errNoTerminal) {
		t.Fatalf("piped with no terminal at all: %v", err)
	}
	out, _, _ := d.run(t, true, "", "history", "--room", "team")
	if !strings.Contains(out, "piped by the person") || strings.Contains(out, "piped by a script") || !strings.Contains(out, d.s.HumanID()) {
		t.Fatalf("history: %q", out)
	}
}

// GPT, second review: in a recognised agent's shell the CLI took the
// default label, another mailbox than the agent's MCP tools if their label
// was set. It now takes A2P2_NAME when the shell has it, as a2p2 mcp does,
// and always says on stderr which identity it used and how to pick
// another. Without A2P2_NAME it is the default label, as before.
func TestAgentShellUsesItsA2P2NameAndSaysWhichIdentity(t *testing.T) {
	d := newDaemonForCLI(t)
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	inShell(t, map[string]string{"CLAUDECODE": "1", "A2P2_NAME": "reviewer"})
	_, serr, err := d.run(t, false, "", "send", "--room", "team", "named")
	if err != nil || !strings.Contains(serr, "acting as reviewer@") || !strings.Contains(serr, "--as") {
		t.Fatalf("with A2P2_NAME: %q %v", serr, err)
	}
	inShell(t, map[string]string{"CLAUDECODE": "1"})
	if _, serr, err := d.run(t, false, "", "send", "--room", "team", "default"); err != nil || !strings.Contains(serr, "acting as claude-proj@") {
		t.Fatalf("default label: %q %v", serr, err)
	}
	inShell(t, nil)
	out, _, _ := d.run(t, true, "", "history", "--room", "team")
	if !strings.Contains(out, "reviewer@") || !strings.Contains(out, "claude-proj@") {
		t.Fatalf("history: %q", out)
	}
}

// GPT and Agy, second review: --person away from a terminal was recorded
// only in the rotating daemon log. Status and doctor now say how often
// since the daemon started, the last time and the command; with no such
// use, neither says anything.
func TestUnattendedPersonUseIsShownInStatusAndDoctor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("doctor's state directory check is a Unix rule here")
	}
	d := newDaemonForCLI(t)
	doctor := func() string {
		t.Helper()
		c := newCmd(context.Background(), "doctor")
		c.home, c.cl = t.TempDir(), d.cl
		if err := os.Chmod(c.home, 0o700); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		_ = runDoctor(c, &out, harness.Options{HomeDir: t.TempDir(), StateDir: t.TempDir(), Binary: "/opt/a2p2", OS: "linux",
			LookPath: func(string) (string, error) { return "", errors.New("not on PATH") }})
		return out.String()
	}
	if out, _, err := d.run(t, true, "", "status"); err != nil || strings.Contains(out, "--person used") {
		t.Fatalf("status before any use: %q %v", out, err)
	}
	if got := doctor(); strings.Contains(got, "--person used") || !strings.Contains(got, "ok    local API") {
		t.Fatalf("doctor before any use:\n%s", got)
	}
	if _, _, err := d.run(t, true, "", "send", "--person", "--room", "team", "at a terminal"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := d.run(t, false, "", "send", "--person", "--room", "team", "from a script"); err != nil {
			t.Fatal(err)
		}
	}
	out, _, err := d.run(t, true, "", "status")
	if err != nil || !strings.Contains(out, "--person used 2 time(s)") || !strings.Contains(out, "(a2p2 send)") {
		t.Fatalf("status: %q %v", out, err)
	}
	if got := doctor(); !strings.Contains(got, "warn  person") || !strings.Contains(got, "--person used 2 time(s)") {
		t.Fatalf("doctor:\n%s", got)
	}
}

// GPT, second review: a home under a directory a group the user is in can
// change (staff on macOS) is no longer refused; status and doctor say so
// instead, and doctor does not fail for it.
func TestHomeWarningsAreShownInStatusAndDoctor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("state directory modes are a Unix rule")
	}
	old := homeWarnings
	t.Cleanup(func() { homeWarnings = old })
	homeWarnings = func(string) []string {
		return []string{"/x, a directory above the a2p2 home, can be changed by every member of its group staff (20)"}
	}
	d := newDaemonForCLI(t)
	if out, _, err := d.run(t, true, "", "status"); err != nil || !strings.Contains(out, "WARNING  /x, a directory above") {
		t.Fatalf("status: %q %v", out, err)
	}
	got, err := doctorOutput(t, 0o700)
	if !strings.Contains(got, "warn  state directory") || !strings.Contains(got, "group staff") || strings.Contains(got, "FAIL  state directory") {
		t.Fatalf("doctor:\n%s", got)
	}
	homeWarnings = func(string) []string { return nil }
	if got, _ := doctorOutput(t, 0o700); strings.Contains(got, "warn ") {
		t.Fatalf("doctor without a warning:\n%s %v", got, err)
	}
}
