package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A person who types --wait 600 is told about --wait and seconds, not about
// the local API's wait_ms field, and nothing is registered or sent first.
func TestWaitOutOfRangeIsReportedInTheFlagsTerms(t *testing.T) {
	d := newDaemonForCLI(t)
	for _, args := range [][]string{{"inbox", "--as", "w", "--wait", "600"}, {"inbox", "--as", "w", "--wait", "-1"}} {
		_, _, err := d.run(t, false, "", args[0], args[1:]...)
		if err == nil || !strings.Contains(err.Error(), "--wait takes 0 to 120 seconds") || strings.Contains(err.Error(), "wait_ms") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for name, fn := range map[string]func(*cmd, []string) error{"alerts": alerts, "fetch": fetch} {
		c := newCmd(context.Background(), name)
		c.fs.SetOutput(io.Discard)
		c.cl = d.cl
		args := []string{"--as", "w", "--wait", "121"}
		if name == "fetch" {
			args = append(args, strings.Repeat("a", 64))
		}
		if err := fn(c, args); err == nil || !strings.Contains(err.Error(), "--wait takes 0 to 120 seconds") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, _, err := d.run(t, false, "", "inbox", "--as", "w", "--peek", "--wait", "1", "--limit", "1"); err != nil {
		t.Fatalf("a wait in range is allowed: %v", err)
	}
}

// A negative --limit or -n names the flag typed, not "limit".
func TestNegativeCountsNameTheFlagTyped(t *testing.T) {
	d := newDaemonForCLI(t)
	if _, _, err := d.run(t, false, "", "inbox", "--as", "w", "--limit", "-3"); err == nil || !strings.Contains(err.Error(), "--limit must not be negative") {
		t.Fatalf("inbox --limit: %v", err)
	}
	if _, _, err := d.run(t, false, "", "history", "--as", "w", "-n", "-3"); err == nil || !strings.Contains(err.Error(), "-n must not be negative") {
		t.Fatalf("history -n: %v", err)
	}
}

// In a harness's shell (CLAUDECODE set, as in make e2e HARNESS=claude), a
// command given --as acts as that label and prints no notice. The notice
// appears only for a command with neither --as nor --person, and then names
// the label that command acts as. The e2e script once ran history without
// --as among commands that had it, which looked like a notice for --as.
func TestAgentShellNoticeNamesTheLabelActuallyUsedAndOnlyWithoutAs(t *testing.T) {
	d := newDaemonForCLI(t)
	dir := filepath.Join(t.TempDir(), "agent-proj")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	inShell(t, map[string]string{"CLAUDECODE": "1", "AI_AGENT": "claude-code/2", "CLAUDE_PROJECT_DIR": dir})
	for _, args := range [][]string{
		{"send", "--as", "tester", "--room", "team", "hello"},
		{"inbox", "--as", "tester", "--peek", "--room", "team"},
		{"history", "--as", "tester", "--room", "team"},
		{"send", "--person", "--room", "team", "by the person"},
	} {
		if _, serr, err := d.run(t, false, "", args[0], args[1:]...); err != nil || strings.Contains(serr, "acting as") {
			t.Fatalf("%v: %q %v", args, serr, err)
		}
	}
	_, serr, err := d.run(t, false, "", "history", "--room", "team")
	if err != nil || !strings.Contains(serr, "acting as claude-agent-proj@") || strings.Contains(serr, "tester") {
		t.Fatalf("history with neither flag: %q %v", serr, err)
	}
}
