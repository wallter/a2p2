package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The daemon an auto-start runs works in its home, not in the directory of
// the command that started it, so that it holds no one's directory (a
// temporary worktree, a mount) for as long as it runs; a relative home is
// made absolute first, so the daemon still finds it. The command is run,
// with this test binary standing in for a2p2, and reports the working
// directory and home it got.
func TestAnAutoStartedDaemonWorksInItsHome(t *testing.T) {
	home := t.TempDir()
	homes := []string{home}
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, home); err == nil && !filepath.IsAbs(rel) {
			homes = append(homes, rel)
		}
	}
	want, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range homes {
		cmd, err := daemonCmd(os.Args[0], h)
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Args[1] != "daemon" {
			t.Fatalf("args %q", cmd.Args)
		}
		cmd.Args = []string{os.Args[0], "-test.run=^TestHelperReportsWhereItRuns$"}
		cmd.Env = append(cmd.Env, "A2P2_TEST_HELPER=where")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("home %s: %v", h, err)
		}
		wd, got, ok := strings.Cut(strings.TrimSpace(string(out)), "\n")
		if !ok {
			t.Fatalf("helper said %q", out)
		}
		for name, p := range map[string]string{"working directory": wd, "A2P2_HOME": got} {
			fi, err := os.Stat(p)
			if err != nil || !os.SameFile(fi, want) || !filepath.IsAbs(p) {
				t.Errorf("home %s: %s %q, want the home %s (%v)", h, name, p, home, err)
			}
		}
	}
}

// TestHelperReportsWhereItRuns is the process TestAnAutoStartedDaemonWorksInItsHome
// starts: it prints its working directory and A2P2_HOME, and is a no-op
// otherwise.
func TestHelperReportsWhereItRuns(t *testing.T) {
	if os.Getenv("A2P2_TEST_HELPER") != "where" {
		return
	}
	wd, err := os.Getwd()
	if err != nil {
		os.Exit(2)
	}
	fmt.Printf("%s\n%s\n", wd, os.Getenv("A2P2_HOME"))
	os.Exit(0)
}
