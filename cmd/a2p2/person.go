package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/store"
)

// Who the CLI acts as (DESIGN 9, SECURITY.md). Without --as it acts as the
// person at this machine, human@<own id>, the identity other members and
// agents take for the person. So the CLI takes it only where a person is
// likely to be typing: never in a shell a harness says belongs to its
// agent, and for a command that speaks or marks mail seen, only at a
// terminal or with --person, which the daemon log records. These are
// guard rails against mistakes and injected instructions; any program
// running as this user can still pass --person or call the socket.

// getenv is os.Getenv; tests replace it so that the environment the tests
// run in (perhaps an agent's shell) changes nothing.
var getenv = os.Getenv

// agentShells are environment variables a harness sets in the shell its
// agent runs commands in, with the harness each names. The first set one
// wins. Claude Code 2.1.332 sets CLAUDECODE=1 and AI_AGENT (seen in its
// shell); Codex sets CODEX_THREAD_ID and, when sandboxed, CODEX_SANDBOX,
// and Gemini CLI sets GEMINI_CLI=1 (both read from their binaries, not
// run). AI_AGENT is a name other harnesses also set.
var agentShells = []struct{ env, harness string }{
	{"CLAUDECODE", "claude"},
	{"CODEX_THREAD_ID", "codex"},
	{"CODEX_SANDBOX", "codex"},
	{"GEMINI_CLI", "gemini"},
	{"AI_AGENT", "agent"},
}

// agentShell is the harness whose agent this shell belongs to and the
// variable that says so, or "".
func agentShell() (harness, env string) {
	for _, a := range agentShells {
		if getenv(a.env) != "" {
			return a.harness, a.env
		}
	}
	return "", ""
}

// agentDir is the directory an agent's default label is made from, as the
// MCP shim finds it.
func agentDir() string {
	if d := getenv("CLAUDE_PROJECT_DIR"); d != "" {
		return d
	}
	d, _ := os.Getwd()
	return d
}

// asSet reports whether --as was given, even as an empty string.
func (c *cmd) asSet() bool {
	set := false
	c.fs.Visit(func(f *flag.Flag) { set = set || f.Name == "as" })
	return set
}

// errNoTerminal is the refusal to act as the person away from a terminal.
var errNoTerminal = errors.New("not at a terminal")

// actor is how the CLI registers for command, and the variable that
// showed an agent's shell when that decided it. writes: the command
// speaks as the actor or marks its mail seen. A refusal says what to run
// instead.
func (c *cmd) actor(command string, writes bool) (localapi.RegisterReq, string, error) {
	person := localapi.RegisterReq{Harness: "human", Label: "human"}
	switch {
	case c.asSet() && *c.as == "":
		return localapi.RegisterReq{}, "", usagef("--as needs a label, such as --as build-bot; leave it out to act as the person at a terminal")
	case *c.as != "" && *c.person:
		return localapi.RegisterReq{}, "", usagef("give --as LABEL or --person, not both")
	case *c.as != "":
		return localapi.RegisterReq{Harness: "cli", Label: *c.as}, "", nil
	case *c.person:
		if h, _ := agentShell(); h != "" || !terminal() {
			person.Unattended = command
		}
		return person, "", nil
	}
	if h, env := agentShell(); h != "" {
		// An agent's shell: the agent's own mailbox, as its MCP tools
		// would get, never the person's.
		return localapi.RegisterReq{Harness: h, Cwd: agentDir()}, env, nil
	}
	if writes && !terminal() {
		peek := ""
		if command == "inbox" {
			peek = "; --peek reads without marking anything seen"
		}
		return localapi.RegisterReq{}, "", fmt.Errorf("%w: a2p2 %s acts as the person at this machine, so it needs a terminal. "+
			"An agent or script names itself with --as LABEL; a script that must act as the person adds --person, "+
			"which the daemon log records%s", errNoTerminal, command, peek)
	}
	return person, "", nil
}

// uiCmd prints or opens the page's address. Its token lets the page act
// as the person (send, seen and ack as human@<own id>), so it is given as
// the person's identity is: never in an agent's shell, and away from a
// terminal only with --person, which the daemon log records.
func uiCmd(c *cmd, args []string) error {
	printOnly := c.fs.Bool("print", false, "print the address and open nothing")
	if err := c.fs.Parse(permute(c.fs, args)); err != nil {
		return errUsage
	}
	switch {
	case *printOnly && *c.json:
		return usagef("a2p2 ui --print prints the address and nothing else; it does not take --json")
	case c.asSet():
		return usagef("a2p2 ui gives the person's page; it takes no --as")
	}
	h, env := agentShell()
	if !*c.person {
		if h != "" {
			return fmt.Errorf("a2p2 ui gives the page that acts as the person, and this shell belongs to an agent (%s is set). "+
				"A person runs it in their own terminal, or adds --person, which the daemon log records", env)
		}
		if !terminal() {
			return fmt.Errorf("%w: a2p2 ui gives the page that acts as the person, so it needs a terminal. "+
				"A script that must have it adds --person, which the daemon log records", errNoTerminal)
		}
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	if *c.person && (h != "" || !terminal()) {
		req := localapi.RegisterReq{Harness: "human", Label: "human", Unattended: "ui"}
		if err := cl.Call(c.ctx, "register", req, &store.Agent{}); err != nil {
			return err
		}
	}
	var st localapi.Status
	if err := c.call("status", nil, &st, nil); err != nil {
		return err
	}
	showUI(os.Stdout, st.UI, *printOnly, openURL) // openURL: tty_unix.go, tty_windows.go
	return nil
}

// who resolves the acting agent for command (see actor) and registers it.
func (c *cmd) who(command string, writes bool) (string, error) {
	req, shell, err := c.actor(command, writes)
	if err != nil {
		return "", err
	}
	cl, err := c.client()
	if err != nil {
		return "", err
	}
	var a store.Agent
	if err := cl.Call(c.ctx, "register", req, &a); err != nil {
		return "", err
	}
	if shell != "" {
		fmt.Fprintf(os.Stderr, "acting as %s, not as the person: this shell belongs to an agent (%s is set). --as LABEL names another agent.\n", a.ID, shell)
	}
	return a.ID, nil
}
