package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/node"
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

// stderrTerminal reports whether stderr is a terminal; tests replace it.
var stderrTerminal = func() bool { return isTerminalFile(os.Stderr) } // tty_unix.go, tty_windows.go

// attended reports whether a person is likely to be typing: stdin is a
// terminal, or stderr is, as when a person pipes text in at a terminal
// (echo hi | a2p2 send). A harness's tool captures stderr as well as
// stdout, so its agent has neither.
func attended() bool { return terminal() || stderrTerminal() }

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
		if h, _ := agentShell(); h != "" || !attended() {
			person.Unattended = command
		}
		return person, "", nil
	}
	if h, env := agentShell(); h != "" {
		// An agent's shell: the agent's own mailbox, as its MCP tools
		// would get under the default label, or under A2P2_NAME when the
		// shell has it (as a2p2 mcp reads it), never the person's.
		return localapi.RegisterReq{Harness: h, Label: getenv("A2P2_NAME"), Cwd: agentDir()}, env, nil
	}
	if writes && !attended() {
		peek := ""
		if command == "inbox" {
			peek = "; --peek reads without marking anything seen"
		}
		return localapi.RegisterReq{}, "", fmt.Errorf("%w: a2p2 %s acts as the person at this machine, so it needs a terminal. "+
			"An agent or script names itself with --as LABEL; a script that must act as the person adds --person, "+
			"which status counts%s", errNoTerminal, command, peek)
	}
	return person, "", nil
}

// uiCmd prints or opens the page's address. Its one-time code lets the page act
// as the person (send, seen and ack as human@<own id>), and whoever reads
// a printed address can trade the code as a browser would, so printing it
// gives the person's identity as opening it does: never in an agent's
// shell, and away from a terminal only with --person, which status counts
// and the daemon log records. Over SSH nothing is opened on the remote
// computer, whose browser would use the code up; the address is printed to
// be opened through a forwarded port.
func uiCmd(c *cmd, args []string) error {
	printOnly := c.fs.Bool("print", false, "print the page's address, which works once within 30 seconds, and open nothing")
	if err := c.fs.Parse(permute(c.fs, args)); err != nil {
		return errUsage
	}
	switch {
	case *c.json:
		return usagef("a2p2 ui prints the address and nothing else; it does not take --json")
	case c.asSet():
		return usagef("a2p2 ui gives the person's page; it takes no --as")
	}
	h, env := agentShell()
	if !*c.person {
		if h != "" {
			return fmt.Errorf("a2p2 ui gives the page that acts as the person, and this shell belongs to an agent (%s is set). "+
				"A person runs it in their own terminal, or adds --person, which status counts", env)
		}
		if !attended() {
			return fmt.Errorf("%w: a2p2 ui gives the page that acts as the person, and so does the address it prints, "+
				"so it needs a terminal (over SSH, ssh -t gives the command one). "+
				"A script that must have it adds --person, which status counts", errNoTerminal)
		}
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	if *c.person && (h != "" || !attended()) {
		req := localapi.RegisterReq{Harness: "human", Label: "human", Unattended: "ui"}
		if err := cl.Call(c.ctx, "register", req, &store.Agent{}); err != nil {
			return err
		}
	}
	var st localapi.Status
	if err := c.call("status", nil, &st, nil); err != nil {
		return err
	}
	remote := !*printOnly && getenv("SSH_CONNECTION") != ""
	return showUI(os.Stdout, os.Stderr, st.UI, st.Version, *printOnly || remote, remote, openURL)
}

// openURL is openBrowser (tty_unix.go, tty_windows.go); tests replace it.
var openURL = openBrowser

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
		// Always said: the agent's MCP tools may use another label (set
		// with --name or A2P2_NAME in the harness's settings, which this
		// shell does not show), and then this is another mailbox.
		fmt.Fprintf(os.Stderr, "acting as %s, not as the person: this shell belongs to an agent (%s is set). "+
			"If this agent's a2p2 tools use another label, pass --as with that label.\n", node.EscapeDisplay(a.ID), shell)
	}
	return a.ID, nil
}
