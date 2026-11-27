package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// runWhere runs this test binary, standing in for a2p2, in dir with env,
// and returns the working directory and A2P2_HOME the process got.
func runWhere(t *testing.T, dir string, env []string) (wd, home string) {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^TestHelperReportsWhereItRuns$")
	c.Dir, c.Env = dir, append(slices.Clone(env), "A2P2_TEST_HELPER=where")
	out, err := c.Output()
	if err != nil {
		t.Fatalf("in %q: %v", dir, err)
	}
	wd, home, ok := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if !ok {
		t.Fatalf("helper said %q", out)
	}
	return wd, home
}

// [D72] The daemon a command starts, with an absolute home (the default),
// works in its home, not in the directory of the command that started it,
// so that it holds no one's directory (a temporary worktree, a mount) for
// as long as it runs; A2P2_HOME is passed as given. A relative home names
// a place from the caller's directory, so its command is as before: no
// working directory set and the same environment, no path rewritten.
func TestAnAutoStartedDaemonWorksInItsHome(t *testing.T) {
	home := t.TempDir()
	cmd := daemonCmd(os.Args[0], home)
	if cmd.Dir != home || cmd.Args[1] != "daemon" || cmd.Env[len(cmd.Env)-1] != "A2P2_HOME="+home {
		t.Fatalf("dir %q, args %q, env ends %q", cmd.Dir, cmd.Args, cmd.Env[len(cmd.Env)-1])
	}
	if !slices.Equal(cmd.Env, append(os.Environ(), "A2P2_HOME="+home)) {
		t.Fatal("the environment is not the caller's with A2P2_HOME as given")
	}
	wd, got := runWhere(t, cmd.Dir, cmd.Env)
	want, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(wd); err != nil || !os.SameFile(fi, want) {
		t.Errorf("working directory %q, want the home %s (%v)", wd, home, err)
	}
	if got != home {
		t.Errorf("A2P2_HOME %q, want %q", got, home)
	}

	rel := filepath.Join("..", "state")
	cmd = daemonCmd(os.Args[0], rel)
	if cmd.Dir != "" || !slices.Equal(cmd.Env, append(os.Environ(), "A2P2_HOME="+rel)) {
		t.Fatalf("relative home: dir %q, env ends %q", cmd.Dir, cmd.Env[len(cmd.Env)-1])
	}
}

// TestHelperReportsWhereItRuns is the process runWhere starts: it prints
// its working directory and A2P2_HOME, and is a no-op otherwise.
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
