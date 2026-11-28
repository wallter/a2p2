// Command a2p2 is the whole product: daemon, CLI, MCP shim and harness hook
// in one binary. See docs/DESIGN.md. The commands are wiring over the local
// API (PROTOCOL section 7); --json prints a response unchanged.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wallter/a2p2/internal/core"
	"github.com/wallter/a2p2/internal/daemon"
	"github.com/wallter/a2p2/internal/harness"
	"github.com/wallter/a2p2/internal/localapi"
	"github.com/wallter/a2p2/internal/mcpshim"
	"github.com/wallter/a2p2/internal/members"
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

var version = "dev" // set by -ldflags at release

const usage = `a2p2 - peer-to-peer messaging for AI agents and the people who run them

Getting started
  a2p2 install claude|codex|agy|opencode|hermes|pi [--on-stop] [--dry-run] [--approve-tools]
                                  connect a harness (undo with: a2p2 uninstall NAME)
                                  --on-stop also checks for messages when a turn ends.
                                  --approve-tools (Codex) lets every a2p2 tool, send included, run without a prompt.
  a2p2 ui [--print]               open the local web page; --print only prints its address
                                  The address works once, within 30 seconds: do not log or share it.
                                  At a terminal only, never in an agent's shell, unless --person is given.
  a2p2 doctor                     check the daemon and harness connections

Talking (as the person at a terminal, or --as <label> for a script)
  In an agent's shell these act as that agent. Away from a terminal, send, ack and
  inbox act as the person only with --person, which the daemon log records.
  a2p2 send [--room R] [--to AGENT]... [--reply-to ID] [--ask-handled] [--replaces ID] TEXT...
                                  Use --to label@machine, for example human@1a2b3c4d.
                                  Find machine IDs with a2p2 room list ROOM.
                                  Empty text with --replaces ID withdraws your message.
  a2p2 send --file PATH [--file PATH]... [flags] TEXT
                                  attach up to 8 files; a person confirms on this machine before
                                  anything is sent. With --replaces, list kept files as sha256:HEX.
  a2p2 fetch SHA256 [--room R] [--message ID] [--wait SECONDS]
                                  copy a verified file of a message you were shown; never opened
  a2p2 inbox [--room R] [--wait SECONDS] [--limit N] [--peek] [--thread ID]
                                  Read unread messages. Use --peek or --thread to leave seen status unchanged.
  a2p2 inbox --level all|addressed|none [--room R] --as LABEL
                                  Set how closely an agent follows a room (--levels shows them).
  a2p2 history [--room R] [-n 50] [--before ID] [--skipped --as LABEL]
                                  --skipped lists what that agent's level kept out of its inbox.
  a2p2 ack ID                     report that you handled a message you read in inbox
  a2p2 trace ID                   show which machines reported a copy and who handled it

Rooms across machines
  a2p2 net on [ADDR]              listen for other machines and connect out
                                  Without ADDR, use the configured address or 0.0.0.0:7423.
  a2p2 net on [ADDR] --map-port   also ask the router to forward the port (opens it to the internet)
  a2p2 net on --outbound-only     connect out only: pull rooms and follow joins, listen on nothing
  a2p2 net off                    this machine only: no listener, no connections
  a2p2 net check [HOST:PORT | --invite [TOKEN]]
                                  can this machine reach its members, can they reach it, and what to do
  a2p2 room create LABEL          make a room with this machine as its admin
  a2p2 room invite ROOM [--history] [--member|--admin]
                                  print a one-time invite (valid 30 minutes)
  a2p2 join [TOKEN] [--no-wait]   preview an invite, approve it here, then wait for the admin
  a2p2 room list [ROOM]           members, admins, changes waiting, limits
  a2p2 room admin ROOM MACHINE    ask to make a member an admin (confirm to sign)
  a2p2 room remove ROOM MACHINE   ask to remove a member (confirm to sign)
  a2p2 room leave ROOM            stop carrying a room and delete its data here (asks at a terminal)
  a2p2 room files ROOM [off|carry|keep]
                                  show or set what this machine does with the room's files:
                                  off: hold only what this machine's agents fetch; carry (default):
                                  also fetch a file a member asks this machine for; keep: every file

Requests and decisions
  The CLI asks for decisions at a terminal. The daemon records them.
  Agent tools cannot approve requests. Programs running as you can access the socket.
  a2p2 confirm                    list pending requests and their IDs
  a2p2 confirm ID [--decline]     approve a listed request, or use --decline to decline it
  a2p2 alerts [--wait SECONDS]    list alerts or wait for one to arrive

This machine
  a2p2 status                     list rooms, agent IDs and network settings
  a2p2 stop | daemon | version
  a2p2 mcp --harness NAME [--name LABEL]     stdio MCP server (launched by harnesses)
  a2p2 hook --harness NAME --event EVENT     unread notice (launched by harness hooks)

Most commands accept --json. State lives in $A2P2_HOME (default ~/.a2p2).
Docs and source: https://github.com/wallter/a2p2
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err := run(ctx, os.Args[1], os.Args[2:])
	var le *localapi.Error
	switch {
	case err == nil:
	case errors.As(err, &le) || errors.Is(err, errUsage):
		fmt.Fprintln(os.Stderr, "a2p2:", errorText(err))
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "a2p2:", errorText(err))
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

// errorText is an error as the last line of a command: escaped as one
// line, since an error can quote what another machine sent (a refusal, an
// address, a name) and no error of a2p2 spans lines.
func errorText(err error) string { return node.EscapeDisplay(err.Error()) }

// showUI prints the page's address and opens it with open unless
// printOnly. The address goes only to w, never to a log. It carries a
// one-time code, which the page trades for a session (internal/web,
// session.go); note, on stderr so that w holds the address alone, says
// so. remote: the shell is over SSH, so nothing was opened and the note
// says how to reach the page. A daemon of 0.2.3 or earlier still running after an
// upgrade gives an address with its long-lived token instead: that is
// neither opened nor printed, since it works for the daemon's whole run
// and opening puts it in the arguments of the browser, where other users
// can read it; the refusal says to restart the daemon. daemonVersion is the
// running daemon's version. When open fails, the address is printed
// already and the error says so.
func showUI(w, note io.Writer, url, daemonVersion string, printOnly, remote bool, open func(string) error) error {
	if !strings.Contains(url, "/#c=") {
		return errors.New("the running a2p2 daemon (" + node.EscapeDisplay(daemonVersion) + ") is older than this a2p2 (" + version +
			") and its address carries its long-lived token, so it was neither opened nor printed. " +
			"Run a2p2 stop, then a2p2 ui, for an address that works once")
	}
	fmt.Fprintln(w, url)
	fmt.Fprintln(note, "This address opens the page once, within 30 seconds. Run a2p2 ui again for a new one.")
	if daemonVersion != version {
		fmt.Fprintln(note, "The running a2p2 daemon is "+node.EscapeDisplay(daemonVersion)+" and this a2p2 is "+version+"; run a2p2 stop to start this one.")
	}
	if remote {
		port := "PORT"
		if u, err := neturl.Parse(url); err == nil && u.Port() != "" {
			port = u.Port()
		}
		fmt.Fprintln(note, "This shell is over SSH, so no browser was opened on this computer. On your own computer, run "+
			"ssh -L "+port+":127.0.0.1:"+port+" with this host, then open the address there.")
	}
	if printOnly {
		return nil
	}
	if err := open(url); err != nil {
		return fmt.Errorf("could not open a browser (%v); open the address printed above within 30 seconds", err)
	}
	return nil
}

func usagef(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, a...))
}

// waitSeconds checks a --wait given in seconds against what the daemon
// allows, and says so in the flag's terms, not in the local API's field.
func waitSeconds(sec int) (int64, error) {
	if sec < 0 || sec > int(node.MaxWait/time.Second) {
		return 0, &localapi.Error{User: true, Msg: fmt.Sprintf("--wait takes 0 to %d seconds, not %d", int(node.MaxWait/time.Second), sec)}
	}
	return int64(sec) * 1000, nil
}

// notNegative checks a count flag, so that the error names the flag typed.
func notNegative(flagName string, n int) error {
	if n < 0 {
		return &localapi.Error{User: true, Msg: fmt.Sprintf("%s must not be negative, not %d", flagName, n)}
	}
	return nil
}

// cmd bundles what most subcommands need.
type cmd struct {
	ctx    context.Context
	fs     *flag.FlagSet
	json   *bool
	as     *string
	person *bool
	home   string
	cl     *localapi.Client
}

func newCmd(ctx context.Context, name string) *cmd {
	c := &cmd{ctx: ctx, fs: flag.NewFlagSet(name, flag.ContinueOnError), home: daemon.Home()}
	c.json = c.fs.Bool("json", false, "print JSON")
	c.as = c.fs.String("as", "", "act as this agent label instead of the person")
	c.person = c.fs.Bool("person", false, "act as the person without a terminal or in an agent's shell; status counts it")
	return c
}

// connectDaemon is daemon.Connect; tests replace it so that no test starts a
// daemon.
var connectDaemon = daemon.Connect

// homeWarnings is daemon.HomeWarnings, printed by status and doctor; tests
// replace it.
var homeWarnings = daemon.HomeWarnings

func (c *cmd) client() (*localapi.Client, error) {
	if c.cl != nil {
		return c.cl, nil
	}
	cl, err := connectDaemon(c.ctx, c.home)
	c.cl = cl
	return cl, err
}

// call makes one local API call; with --json it prints the response as
// received, otherwise human runs.
func (c *cmd) call(name string, req, resp any, human func()) error {
	cl, err := c.client()
	if err != nil {
		return err
	}
	raw, err := cl.CallRaw(c.ctx, name, req, resp)
	if err != nil {
		return err
	}
	if *c.json {
		_, err := os.Stdout.Write(raw)
		return err
	}
	if human != nil {
		human()
	}
	return nil
}

// terminal reports whether stdin is a terminal. A person's decisions are
// asked for there and not through a pipe, which keeps them out of what an
// agent's tools do; it does not prove a person answered, since a program
// running as this user can drive a terminal or call the socket directly.
// It asks the terminal driver (a character device such as /dev/null is
// not a terminal); see tty_unix.go.
var terminal = isTerminal

var stdin = bufio.NewReader(os.Stdin)

func ask(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	line, _ := stdin.ReadString('\n')
	return strings.TrimSpace(line)
}

func run(ctx context.Context, name string, args []string) (err error) {
	c := newCmd(ctx, name)
	defer func() {
		var apiErr *localapi.Error
		if !*c.json && errors.As(err, &apiErr) && apiErr.User {
			err = cliError(apiErr)
		}
	}()
	switch name {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "version", "--version":
		fmt.Println("a2p2", version)
		return nil

	case "daemon":
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
		return daemon.Run(ctx, c.home, version)

	case "stop":
		cl, ok := daemon.Running(ctx, c.home)
		if !ok {
			fmt.Println("not running")
			return nil
		}
		return cl.Call(ctx, "stop", nil, nil)

	case "mcp":
		h := c.fs.String("harness", "mcp", "harness launching this server")
		label := c.fs.String("name", os.Getenv("A2P2_NAME"), "agent label (default: <harness>-<directory>)")
		if err := c.fs.Parse(args); err != nil {
			return errUsage
		}
		// stdout belongs to the MCP protocol: all logging goes to stderr.
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
		cl, err := c.client()
		if err != nil {
			return err
		}
		cwd := os.Getenv("CLAUDE_PROJECT_DIR")
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		return mcpshim.Run(ctx, cl, *h, *label, cwd, version)

	case "hook":
		h := c.fs.String("harness", "", "harness running this hook")
		ev := c.fs.String("event", "", "hook event name")
		if err := c.fs.Parse(args); err != nil {
			return nil // a hook must never break a turn
		}
		cl, ok := daemon.Running(ctx, c.home) // never start a daemon from a hook
		if !ok {
			return nil
		}
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_ = harness.Hook(hctx, cl, *h, *ev, os.Stdin, os.Stdout)
		return nil

	case "install", "uninstall":
		target, o, err := installArgs(c.fs, name, args)
		if err != nil {
			return err
		}
		if o.Binary, err = os.Executable(); err != nil {
			return err
		}
		fn := harness.Install
		if name == "uninstall" {
			fn = harness.Uninstall
		}
		report, err := fn(target, o)
		printReport(os.Stdout, report)
		return err

	case "doctor":
		return doctor(c, args)
	case "send":
		return send(c, args)
	case "fetch":
		return fetch(c, args)
	case "inbox":
		return inbox(c, args)
	case "history":
		return history(c, args)
	case "net":
		return netCmd(c, args)
	case "join":
		return join(c, args)
	case "room":
		return room(c, args)
	case "confirm":
		return confirm(c, args)
	case "alerts":
		return alerts(c, args)
	case "ack":
		return ack(c, args)
	case "ui":
		return uiCmd(c, args)
	}

	// Everything below talks to the daemon.
	if err := c.fs.Parse(permute(c.fs, args)); err != nil {
		return errUsage
	}
	rest := c.fs.Args()
	switch name {
	case "status":
		return status(c)
	case "trace":
		if len(rest) != 1 {
			return usagef("a2p2 trace ID")
		}
		return trace(c, rest[0])
	}
	if name == "reply" {
		return usagef("unknown command %q; to reply to a message, run a2p2 send --reply-to ID TEXT", name)
	}
	return usagef("unknown command %q; run a2p2 help", name)
}

// ack reports a message handled, as the acting agent.
func ack(c *cmd, args []string) error {
	if err := c.fs.Parse(permute(c.fs, args)); err != nil {
		return errUsage
	}
	if c.fs.NArg() != 1 {
		return usagef("a2p2 ack ID")
	}
	me, err := c.who("ack", true)
	if err != nil {
		return err
	}
	var r localapi.AckResp
	return c.call("ack", localapi.AckReq{Agent: me, ID: c.fs.Arg(0)}, &r, func() { fmt.Println(ackText(r)) })
}

// installArgs reads install and uninstall arguments: the harness name and
// its flags, in either order.
func installArgs(fs *flag.FlagSet, name string, args []string) (string, harness.Options, error) {
	onStop := fs.Bool("on-stop", false, "also check for mail when the agent finishes a turn (may start another turn)")
	dry := fs.Bool("dry-run", false, "show what would change")
	approve := fs.Bool("approve-tools", false, "Codex: let every a2p2 tool, send included, run without a prompt")
	if err := fs.Parse(permute(fs, args)); err != nil || fs.NArg() != 1 {
		return "", harness.Options{}, usagef("a2p2 %s <%s> [--on-stop] [--dry-run] [--approve-tools]", name, strings.Join(harness.Names, "|"))
	}
	return fs.Arg(0), harness.Options{OnStop: *onStop, DryRun: *dry, ApproveTools: *approve}, nil
}

// ackText is what a2p2 ack prints: a repeated ack names the first report
// rather than looking like a new one.
func ackText(r localapi.AckResp) string {
	if r.Already {
		return "already reported handled, report " + r.Handled
	}
	return "handled, report " + r.Handled
}

// printReport prints an install or uninstall report as whole lines.
func printReport(w io.Writer, report string) {
	if report == "" {
		return
	}
	if !strings.HasSuffix(report, "\n") {
		report += "\n"
	}
	fmt.Fprint(w, report)
}

// The daemon's user errors are shared with the MCP tools. Point CLI callers
// to commands they can run here, preserving the original error category.
func cliError(err *localapi.Error) error {
	switch {
	case strings.HasPrefix(err.Msg, "no room ") && strings.HasSuffix(err.Msg, " here"):
		return fmt.Errorf("%w. Run a2p2 room list to see rooms on this machine", err)
	case strings.HasSuffix(err.Msg, " is not an agent id (label@node, as in peers)"):
		id := strings.TrimSuffix(err.Msg, " is not an agent id (label@node, as in peers)")
		return &localapi.Error{User: true, Msg: id + " needs a full agent ID, such as human@1a2b3c4d. Run a2p2 room list ROOM for machine IDs"}
	case strings.HasPrefix(err.Msg, "not an invite:"), strings.HasPrefix(err.Msg, "bad invite token:"):
		return &localapi.Error{User: true, Msg: "that invite is not valid: it may be cut short or mistyped. " +
			"Paste the whole invite, starting with a2p2:, or ask for a new one"}
	case strings.HasSuffix(err.Msg, " is not an agent of a machine in this room"):
		return &localapi.Error{User: true, Msg: strings.TrimSuffix(err.Msg, " is not an agent of a machine in this room") +
			" is not on a machine in this room. Run a2p2 room list ROOM to see its machines"}
	case strings.HasPrefix(err.Msg, "no member ") && strings.HasSuffix(err.Msg, " in the room"):
		return fmt.Errorf("%w. Run a2p2 room list ROOM to see its machines", err)
	case err.Msg == "no such request":
		return &localapi.Error{User: true, Msg: "no request with that ID is waiting. Run a2p2 confirm to list them"}
	case err.Msg == "ask_handled is only for a post to everyone":
		return &localapi.Error{User: true, Msg: "--ask-handled is for posts to the whole room, so leave out --to. " +
			"An addressee can always report a message handled with a2p2 ack ID"}
	case err.Msg == "everyone is the only addressee when given":
		return &localapi.Error{User: true, Msg: "--to everyone cannot be combined with other --to names"}
	case err.Msg == members.ReasonAlreadyAdmin:
		return &localapi.Error{User: true, Msg: "this machine already lists that member as an admin. Run a2p2 room list ROOM to see the members"}
	case err.Msg == node.ErrNotHeld.Error():
		return &localapi.Error{User: true, Msg: "this machine has no message with that ID. Run a2p2 history --room ROOM to find a message ID"}
	}
	return err
}

// permute moves flags before positional arguments, so a flag works on
// either side of them: "a2p2 install claude --dry-run" and "a2p2 trace ID
// --json". It asks fs which flags take a value, so "--wait 5" stays
// together wherever it appears. Everything after "--" is positional.
func permute(fs *flag.FlagSet, args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue // Parse reports it
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(append(flags, "--"), pos...)
}

// clock is a time stamp as a person reads it; one that does not parse is
// shown escaped, since a stamp can come from another machine.
func clock(t string) string {
	if tm, err := time.Parse(time.RFC3339, t); err == nil {
		return tm.Local().Format("Jan 02 15:04:05")
	}
	return node.EscapeDisplay(t)
}

// authorMark says where a message's author is, from from_this_machine,
// which the daemon sets by the full machine key (D65): any member machine
// can post as human, so a human on another machine is marked as that
// machine's person and never as this machine's.
func authorMark(e localapi.Entry) string { return marks(e.From, e.FromThisMachine, false) }

// marks are the CLI's words after an author written as from: where it is
// (node.AuthorMark, from the full key) and, with shared, that its short
// id is another known machine's too, so the full key is written.
func marks(from string, here, shared bool) string {
	out := ""
	if m := node.AuthorMark(from, here); m != "" {
		out += " [" + m + "]"
	}
	if shared {
		out += " [" + idSharedWords + "]"
	}
	return out
}

// idSharedWords mark an id written with the full key because another
// machine known here has the same short id (D65).
const idSharedWords = "id written in full: short id shared"

// machineShown is a machine key as the CLI writes it: the short id, or
// the full key when another known machine shares the short id.
func machineShown(n string, shared bool) string {
	if shared {
		return node.EscapeDisplay(n) + " [" + idSharedWords + "]"
	}
	return short(n)
}

// printEntry shows one entry. Its text, choices, author and addressees
// come from other machines, so each is escaped (node.EscapeDisplayText for
// the text, node.EscapeDisplay for the rest): a terminal escape or a
// character that hides or reorders text is shown as \u{...}. The marks
// after the author and [edited] are the CLI's own words.
func printEntry(w io.Writer, e localapi.Entry) {
	head := node.EscapeDisplay(e.From) + authorMark(e)
	if len(e.To) > 0 {
		head += " → " + escapeAll(e.To, ", ")
	}
	if e.IDShared { // the author's or an addressee's id
		head += " [" + idSharedWords + "]"
	}
	if e.Kind != "message" {
		head += " [" + e.Kind + "]"
	}
	if e.Edited {
		head += " [edited]"
	}
	if c := node.ChangedWords(e.ChangedBy, e.FromThisMachine); c != "" {
		head += " [" + c + "]"
	}
	if e.Parent != "" {
		head += " (reply to " + node.EscapeDisplay(e.Parent)
		if e.ParentState != "" && e.ParentState != "here" {
			head += ", " + e.ParentState
		}
		head += ")"
	}
	if e.AuthorRemoved {
		head += " [author no longer a member]"
	}
	if e.Late {
		head += " [late]"
	}
	if e.AskHandled {
		head += " [asks for handled reports]"
	}
	if e.Skipped {
		head += " [skipped by your level]"
	}
	fmt.Fprintf(w, "%s  %s\n", clock(e.Created), head)
	if e.Text != "" {
		fmt.Fprintf(w, "    %s\n", strings.ReplaceAll(node.EscapeDisplayText(e.Text), "\n", "\n    "))
	}
	if len(e.Choices) > 0 {
		fmt.Fprintf(w, "    choices: %s\n", escapeAll(e.Choices, " | "))
	}
	if e.Kind == node.EntryRequest {
		switch e.State {
		case node.RequestSent:
			fmt.Fprintf(w, "    your send with files (request %s) was approved and sent\n", e.Request)
		default:
			fmt.Fprintf(w, "    your send with files (request %s) was declined: %s; nothing was sent\n", e.Request, node.EscapeDisplay(e.Reason))
		}
	}
	printFiles(w, e)
	fmt.Fprintf(w, "    id %s\n", e.ID)
}

// fileWords say what this machine holds of a file of a message.
var fileWords = map[string]string{
	node.FileStateHere:        "here: a2p2 fetch copies it out",
	node.FileStateNotFetched:  "not fetched: a2p2 fetch gets it",
	node.FileStatePartial:     "partly downloaded",
	node.FileStateComing:      "coming: asked of its author",
	node.FileStateUnavailable: "unavailable: no member served it",
}

// printFiles lists the files of an entry; names come from other machines
// and are escaped.
func printFiles(w io.Writer, e localapi.Entry) {
	for _, f := range e.Files {
		state := fileWords[f.State]
		if state == "" {
			state = f.State
		}
		fmt.Fprintf(w, "    file %s  %s  %s\n         sha256 %s\n", node.EscapeDisplay(f.Name), sizeText(f.Size), state, f.SHA256)
	}
	if e.FilesPending {
		fmt.Fprintln(w, "    files: has files; their list is not here yet")
	}
	if e.FilesNote != "" {
		fmt.Fprintf(w, "    files: %s\n", e.FilesNote)
	}
}

// escapeAll is ss, each escaped for one line (node.EscapeDisplay), joined
// by sep.
func escapeAll(ss []string, sep string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = node.EscapeDisplay(s)
	}
	return strings.Join(out, sep)
}

// approvalText is a sentence the daemon wrote with node.Isolate (the text
// of a request, the join preview) as a terminal shows it. A terminal that
// orders text by direction could move right-to-left text in a remote
// string across the words around it, so when an isolated part holds any,
// each such part is shown as [1], [2] in the sentence and on its own line
// after it, under indent, where it cannot reorder anything else. Other
// parts stay in place.
func approvalText(text, indent string) string {
	var b, after strings.Builder
	n := 0
	for _, p := range node.Isolated(text) {
		if !p.Isolated || !node.RightToLeft(p.Text) {
			b.WriteString(p.Text)
			continue
		}
		n++
		fmt.Fprintf(&b, "[%d]", n)
		fmt.Fprintf(&after, "\n%s[%d] %s", indent, n, p.Text)
	}
	return b.String() + after.String()
}

// short is the first 8 bytes of a machine's key or an entry's id, as
// shown: escaped, since a key or id named in an entry from another
// machine is whatever that machine wrote.
func short(s string) string {
	if len(s) > 8 {
		s = s[:8]
	}
	return node.EscapeDisplay(s)
}

// Label loopback addresses without implying that a public or LAN address
// was tested from another machine. The address is escaped: an advertised
// one can be the router's answer.
func addressText(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil && (net.ParseIP(host).IsLoopback() || strings.EqualFold(host, "localhost")) {
		return node.EscapeDisplay(addr) + " (this machine only)"
	}
	return node.EscapeDisplay(addr)
}

func status(c *cmd) error {
	var st localapi.Status
	defer func() {
		// Printed (as text or JSON): a person has seen the notice.
		if st.Notice != "" && c.cl != nil {
			_ = c.cl.Call(c.ctx, "status", localapi.StatusReq{NoticeShown: true}, nil)
		}
	}()
	if *c.json {
		// Without the page's address: it carries a code that opens the
		// page as the person, and agents run status --json into their
		// transcripts. a2p2 ui
		// --print gives it to a person who asks.
		cl, err := c.client()
		if err != nil {
			return err
		}
		raw, err := cl.CallRaw(c.ctx, "status", localapi.StatusReq{ShowNotice: true}, &st)
		if err != nil {
			return err
		}
		if raw, err = withoutKey(raw, "ui"); err != nil {
			return err
		}
		_, err = os.Stdout.Write(raw)
		return err
	}
	return c.call("status", localapi.StatusReq{ShowNotice: true}, &st, func() {
		if st.Notice != "" {
			fmt.Println("NOTICE   " + st.Notice)
		}
		fmt.Printf("machine  %s (%s)  a2p2 %s  pid %d\n", node.EscapeDisplay(st.Name), short(st.Node), st.Version, st.PID)
		if _, line := daemonVersionNote(st.Version, version); line != "" {
			fmt.Println("         " + line)
		}
		switch {
		case st.Network.Reason == "being applied":
			fmt.Printf("network  %s decided, being applied (still %s)\n", st.Network.State, st.Network.Applied)
		case st.Network.Applied != "":
			fmt.Printf("network  %s, %s\n", st.Network.State, st.Network.Reason)
		case st.Network.State == "on":
			fmt.Printf("network  on, listening on %s\n", addressText(st.Network.Listen))
			if st.Network.IPv4Only {
				fmt.Println("         IPv4 only: decided before a2p2 listened on IPv6; run a2p2 net on again to add it")
			}
			if len(st.Listen) > 0 {
				addresses := make([]string, 0, len(st.Listen))
				for _, addr := range st.Listen {
					addresses = append(addresses, addressText(addr))
				}
				fmt.Println("         advertised:", strings.Join(addresses, ", "))
			}
		case st.Network.State == "outbound":
			fmt.Println("network  outbound only, accepts no incoming connections")
		default:
			fmt.Println("network  off, this machine only. To connect to other machines, run a2p2 net on.")
		}
		if st.Network.State != "off" {
			reach := "reachable by peers: " + st.Reach.State
			if st.Reach.Checked != "" {
				reach += ", checked " + clock(st.Reach.Checked)
			}
			if st.Reach.Why != "" {
				// it can quote members' dial-back errors
				reach += " (" + node.EscapeDisplay(st.Reach.Why) + ")"
			}
			fmt.Println("         " + reach + "; a2p2 net check explains")
		}
		if st.Mapping != nil {
			fmt.Println("         " + mappingText(st.Mapping))
		}
		if st.Waiting > 0 {
			fmt.Printf("requests %d pending. Run a2p2 confirm to review.\n", st.Waiting)
		}
		fmt.Printf("files    %s held of %s on this machine\n", sizeText(st.Files.Held), sizeText(st.Files.Budget))
		if st.Files.Unknown > 0 {
			fmt.Printf("         %d unknown files under blobs/ in the state directory, left alone: a2p2 did not write them\n", st.Files.Unknown)
		}
		fmt.Println("ui       run a2p2 ui to open the local page")
		if u := st.Unattended; u != nil {
			fmt.Println("person   " + personUseText(*u))
		}
		for _, w := range homeWarnings(c.home) {
			fmt.Println("WARNING  " + node.EscapeDisplay(w))
		}
		fmt.Println("rooms")
		// Labels come from the rooms' creators, perhaps on other
		// machines: escaped, and compared as shown.
		labels := make(map[string]int, len(st.Rooms))
		for _, r := range st.Rooms {
			labels[node.LabelKey(r.Label)]++
		}
		for _, r := range st.Rooms {
			name := node.EscapeDisplay(r.Label)
			if labels[node.LabelKey(r.Label)] > 1 {
				name += " (" + r.ID + ")"
			}
			files := ""
			if r.Files != nil {
				files = fmt.Sprintf("  files: %s, %s held", r.Files.Mode, sizeText(r.Files.Held))
			}
			fmt.Printf("  %-20s machines: %d%s\n", name, len(r.Members), files)
			if r.MayBeRemoved {
				fmt.Println("    " + node.MaybeRemovedText)
			}
		}
		for _, q := range st.Quarantined {
			fmt.Printf("  %-20s %s\n", node.EscapeDisplay(q.Label)+" ("+q.ID+")", node.QuarantineText)
			fmt.Println("    why: " + node.EscapeDisplay(q.Reason))
			fmt.Println("    fix: " + node.QuarantineFix(q.ID, q.ID == st.Local))
		}
		fmt.Println("agents on this machine")
		if len(st.Agents) == 0 {
			fmt.Println("  none registered")
		}
		for _, a := range st.Agents {
			fmt.Printf("  %-32s %-8s last seen %s\n", a.ID, a.Harness, a.LastSeen.Local().Format("Jan 02 15:04"))
		}
		if len(st.Peers) > 0 {
			fmt.Println("other machines")
			for _, p := range st.Peers {
				heard := "not heard from in this run"
				if p.LastHeard != "" {
					heard = "last heard " + clock(p.LastHeard)
				}
				fmt.Printf("  %-16s %s  %s  %s\n", node.EscapeDisplay(p.Name), short(p.Node), escapeAll(p.Addrs, ","), heard)
				if h := heldText(p.Held); h != "" {
					fmt.Println("    " + h)
				}
			}
		}
	})
}

func trace(c *cmd, id string) error {
	var t localapi.TraceResp
	return c.call("trace", localapi.IDReq{ID: id}, &t, func() { printTrace(os.Stdout, t) })
}

// The words for a copy report and for the state of a message's text; the
// page (app.js, openTrace) uses the same ones.
const copyReportWords = "machines that reported holding the author's messages up to this one, as this machine observed"

var textWords = map[string]string{
	store.ReasonLive:        "available on this machine",
	store.ReasonMissing:     "waiting for a copy",
	store.ReasonNotAdmitted: "outside this machine's permitted room history",
	store.ReasonWithdrawn:   "withdrawn by its author",
	store.ReasonExpired:     "expired (text is kept 30 days)",
}

func printTrace(w io.Writer, t localapi.TraceResp) {
	reason := t.Reason
	if s, ok := textWords[reason]; ok {
		reason = s
	}
	header := "saved on this machine"
	if !t.Saved {
		header = "not held on this machine"
	}
	fmt.Fprintf(w, "message  %s\nroom     %s\n", t.ID, t.Room)
	if t.From != "" {
		fmt.Fprintf(w, "from     %s%s\n", node.EscapeDisplay(t.From), marks(t.From, t.FromThisMachine, t.IDShared))
	}
	fmt.Fprintf(w, "header   %s\ntext     %s\n", header, reason)
	if len(t.Copies) == 0 {
		fmt.Fprintln(w, "copy reports: none yet")
	} else {
		fmt.Fprintln(w, "copy reports ("+copyReportWords+"):")
	}
	for _, cp := range t.Copies {
		member := "member now"
		if !cp.Member {
			member = "not a member now"
		}
		fmt.Fprintf(w, "  %s (%s), observed %s\n", machineShown(cp.Node, cp.IDShared), member, clock(cp.At))
	}
	if len(t.Handled) == 0 {
		fmt.Fprintln(w, "handled: no report yet")
	} else {
		fmt.Fprintln(w, "handled reports (time signed by the agent's machine):")
	}
	for _, h := range t.Handled {
		fmt.Fprintf(w, "  by %s%s at %s\n", node.EscapeDisplay(h.Agent), marks(h.Agent, h.FromThisMachine, h.IDShared), clock(h.At))
	}
	if len(t.Files) > 0 {
		fmt.Fprintln(w, "files (what this machine holds; each member's last answer, as this machine observed it):")
	}
	for _, f := range t.Files {
		fmt.Fprintf(w, "  %s  %s  %s  sha256 %s\n", node.EscapeDisplay(f.Name), sizeText(f.Size), node.EscapeDisplay(f.State), node.EscapeDisplay(f.SHA256))
		for _, h := range f.Holders {
			fmt.Fprintf(w, "    %s answered %s at %s\n", short(h.Node), node.EscapeDisplay(h.Answer), clock(h.At))
		}
	}
}

// join previews an invite, asks the person at this terminal, accepts, and
// waits for the admin machine's person unless --no-wait. Without a
// terminal it shows the preview only.
func join(c *cmd, args []string) error {
	noWait := c.fs.Bool("no-wait", false, "do not wait for the admin machine's person")
	if err := c.fs.Parse(permute(c.fs, args)); err != nil || c.fs.NArg() > 1 {
		return usagef("a2p2 join [TOKEN] [--no-wait]")
	}
	token := c.fs.Arg(0)
	if token == "" || token == "-" {
		prompt := ""
		if terminal() {
			prompt = "Paste the full invite starting with a2p2: "
		}
		token = ask(prompt)
		if token == "" && !*c.json {
			return usagef("run a2p2 join in a terminal and paste the full invite starting with a2p2:")
		}
	}
	var pv localapi.PreviewResp
	cl, err := c.client()
	if err != nil {
		return err
	}
	if err := cl.Call(c.ctx, "room/join", localapi.JoinReq{Token: token}, &pv); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, previewText(pv))
	if pv.Member {
		fmt.Fprintln(os.Stderr, "This machine is a member already: approving fetches the room's log and changes nothing.")
	}
	if !terminal() {
		return errors.New("run a2p2 join in a terminal and paste the invite to accept it")
	}
	if ask("Join on these terms? Type yes: ") != "yes" {
		return errors.New("not joined")
	}
	req := localapi.AcceptReq{Token: token, Preview: pv.Preview}
	if !*noWait {
		req.WaitMS = 60000
		fmt.Fprintln(os.Stderr, "Waiting for approval. Run a2p2 confirm on the admin machine to review the request.")
	}
	deadline := time.Now().Add(30 * time.Minute)
	for {
		var o localapi.AcceptResp
		raw, err := cl.CallRaw(c.ctx, "room/join/accept", req, &o)
		if err != nil {
			return err
		}
		if o.State != "accepted" || *noWait || time.Now().After(deadline) {
			if *c.json {
				_, err := os.Stdout.Write(raw)
				return err
			}
			switch o.State {
			case "signed":
				// the admin machine's answer: escaped
				fmt.Printf("joined room %s as %s.%s\n", node.EscapeDisplay(o.Room), node.EscapeDisplay(o.Role),
					map[bool]string{true: " " + node.EscapeDisplay(o.Note), false: ""}[o.Note != ""])
			case "accepted":
				fmt.Println("Invite accepted on this machine. Waiting for admin approval. The daemon will keep checking.")
			default:
				fmt.Printf("%s: %s\n", node.EscapeDisplay(o.State), node.EscapeDisplay(o.Reason))
			}
			return nil
		}
	}
}

func room(c *cmd, args []string) error {
	history := c.fs.Bool("history", false, "invite: the joiner may read earlier history")
	member := c.fs.Bool("member", false, "invite: as a member")
	admin := c.fs.Bool("admin", false, "invite: as an admin")
	if err := c.fs.Parse(permute(c.fs, args)); err != nil {
		return errUsage
	}
	rest := c.fs.Args()
	if len(rest) == 0 {
		rest = []string{"list"}
	}
	switch {
	case rest[0] == "list" && len(rest) <= 2:
		req := localapi.RoomListReq{}
		if len(rest) == 2 {
			req.Room = rest[1]
		}
		var res localapi.RoomsResp
		return c.call("room/list", req, &res, func() {
			for i, r := range res.Rooms {
				if i > 0 {
					fmt.Println()
				}
				fmt.Printf("Room %s  %s\n", node.EscapeDisplay(r.Label), r.ID)
				if r.MayBeRemoved {
					fmt.Println("  " + node.MaybeRemovedText)
				}
				for _, m := range r.Members {
					role := "member"
					if m.Admin {
						role = "admin"
					}
					fmt.Printf("  %s %-16s %s\n", short(m.Node), node.EscapeDisplay(m.Name), role)
				}
				for _, p := range r.Pending {
					fmt.Printf("  waiting for a second admin: %s %s by %s\n", node.EscapeDisplay(p.Kind), short(p.Target), short(p.Author))
				}
				for _, n := range r.NoEffect {
					fmt.Printf("  no effect: entry %s: %s\n", short(n.Entry), node.EscapeDisplay(n.Reason))
				}
				for _, f := range r.Forks {
					state := "settled"
					if f.Open {
						state = "unresolved"
					}
					fmt.Printf("  conflicting membership entries from %s at position %d (%s)\n", short(f.Author), f.Seq, state)
				}
				// The limits of a shared room are for whoever asks about it by
				// name; a room of this machine alone has none worth showing.
				if req.Room != "" && len(r.Members) > 1 {
					for _, l := range r.Limits {
						fmt.Printf("  limit: %s\n", l)
					}
				}
			}
		})
	case rest[0] == "create" && len(rest) == 2:
		var r localapi.Room
		return c.call("room/create", localapi.RoomReq{Room: rest[1]}, &r, func() {
			fmt.Printf("created \"%s\" (%s). Invite a machine with: a2p2 room invite %s\n", node.EscapeDisplay(r.Label), r.ID, node.EscapeDisplay(r.Label))
		})
	case rest[0] == "invite" && len(rest) == 2:
		if *member && *admin {
			return usagef("--member or --admin, not both")
		}
		req := localapi.InviteReq{Room: rest[1], History: *history}
		if *member {
			req.Role = "member"
		} else if *admin {
			req.Role = "admin"
		}
		var inv localapi.InviteResp
		return c.call("room/invite", req, &inv, func() {
			fmt.Println(inv.Token)
			fmt.Fprintf(os.Stderr, "\nOne use, expires %s. Anyone holding it can ask to join, so send it privately.\n"+
				"On the other machine, run a2p2 join and paste the invite. Then run a2p2 confirm here to review the request.\n", clock(inv.Expires))
		})
	case (rest[0] == "admin" || rest[0] == "remove") && len(rest) == 3:
		var r localapi.ChangeResp
		return c.call("room/"+rest[0], localapi.MemberReq{Room: rest[1], Node: rest[2]}, &r, func() {
			fmt.Println("Request awaiting a decision on this machine")
			printTerms(os.Stdout, r.Terms)
			fmt.Printf("Decide it with: a2p2 confirm %s\n", r.Request)
		})
	case rest[0] == "files" && (len(rest) == 2 || len(rest) == 3):
		req := localapi.RoomFilesReq{Room: rest[1]}
		if len(rest) == 3 {
			// Changing it is a person's decision: asked at a terminal, as
			// confirm and net are; reading it needs none.
			if !terminal() {
				return errors.New("run this room files command in a terminal to change the setting")
			}
			req.Mode = rest[2]
			fmt.Fprintf(os.Stderr, "Room %s: files %s (%s)\n", node.EscapeDisplay(rest[1]), node.EscapeDisplay(req.Mode), filesModeWords[req.Mode])
			if ask("Change it? Type yes: ") != "yes" {
				return errors.New("nothing changed")
			}
		}
		var r localapi.RoomFiles
		return c.call("room/files", req, &r, func() {
			fmt.Printf("files %s: %s of %s held for this room\n", r.Mode, sizeText(r.Held), sizeText(r.Budget))
		})
	case rest[0] == "leave" && len(rest) == 2:
		return leave(c, rest[1])
	}
	return usagef("a2p2 room create LABEL | invite ROOM [--history] [--member|--admin] | admin ROOM MACHINE | remove ROOM MACHINE | " +
		"files ROOM [off|carry|keep] | leave ROOM | list [ROOM]")
}

// The daemon describes the action, room ID, full fingerprint, role, history
// and second signature in Text. Keep those terms together for review.
func printTerms(w io.Writer, q localapi.Request) {
	fmt.Fprintf(w, "Request %s\n  If approved: %s\n", q.ID, approvalText(q.Text, "    "))
	if q.Kind == store.RequestSendFiles {
		printSendTerms(w, q)
		return
	}
	if q.Name != "" && q.Kind != store.RequestNetwork {
		fmt.Fprintln(w, "  "+nameNote(q))
	}
	if q.Deadline != "" {
		fmt.Fprintf(w, "  Invite expires %s\n", clock(q.Deadline))
	}
	if q.Warning != "" { // escaped and isolated by the daemon, as Text is
		fmt.Fprintf(w, "  WARNING: %s\n", approvalText(q.Warning, "    "))
	}
}

// previewText is the invite a person accepts or not. The room's label was
// chosen by the room's creator, on another machine: it is escaped and
// isolated as every remote string a person approves (node.EscapeDisplay,
// approvalText).
func previewText(pv localapi.PreviewResp) string {
	hist := "without earlier history"
	if pv.History {
		hist = "with earlier history"
	}
	return approvalText(fmt.Sprintf("Invite to room \"%s\" (%s), as %s, %s, from admin machine %s; expires %s.",
		node.Isolate(node.EscapeDisplay(pv.Room.Label)), node.EscapeDisplay(pv.Room.ID), pv.Role, hist, short(pv.Admin), clock(pv.Expires)), "  ")
}

// nameNote says who chose the name a request carries (node.NameFrom*),
// so that a name another admin wrote is not read as the machine's own.
func nameNote(q localapi.Request) string {
	switch q.NameFrom {
	case node.NameFromMachine:
		return "The machine chose its own name. Check its fingerprint."
	case node.NameFromAdmin:
		return "The admin who signed the first entry chose this name. Check the fingerprint against the room's members."
	case node.NameFromHere:
		return "The name was given on this machine. Check the fingerprint."
	}
	return "Check the machine's fingerprint, not its name."
}

// printSendTerms shows every term of a send with files (DESIGN 12 item
// 8): the daemon has escaped controls and bidirectional characters in
// paths, names and lines, so each path is shown in full as resolved from
// the file opened.
func printSendTerms(w io.Writer, q localapi.Request) {
	// The daemon escaped the text, paths, names and the replaced line;
	// agent ids are escaped here: the author of a parent is another
	// machine's.
	fmt.Fprintf(w, "  agent     %s\n", node.EscapeDisplay(q.Agent))
	fmt.Fprintf(w, "  room      %s (%s)\n", node.EscapeDisplay(q.RoomLabel), q.Room)
	switch {
	case len(q.To) == 1 && q.To[0] == node.Everyone:
		fmt.Fprintf(w, "  to        everyone in the room (%d members)\n", q.Members)
	case len(q.To) > 0:
		fmt.Fprintf(w, "  to        %s\n", escapeAll(q.To, ", "))
	case q.ReplyTo != "" && q.ReplyAuthor != "":
		fmt.Fprintf(w, "  to        %s, the author of message %s\n", node.EscapeDisplay(q.ReplyAuthor), node.EscapeDisplay(q.ReplyTo))
	case q.ReplyTo != "":
		fmt.Fprintf(w, "  to        the author of message %s\n", node.EscapeDisplay(q.ReplyTo))
	}
	if q.ReplyTo != "" {
		fmt.Fprintf(w, "  reply to  %s\n", node.EscapeDisplay(q.ReplyTo))
	}
	if q.Replaces != nil {
		fmt.Fprintf(w, "  replaces  %s: \"%s\"\n", node.EscapeDisplay(q.Replaces.ID), q.Replaces.Line)
		if q.Replaces.Version != "" {
			fmt.Fprintf(w, "            its version %s\n", node.EscapeDisplay(q.Replaces.Version))
		}
		fmt.Fprintln(w, "            the replacement keeps its recipients and has only the files listed here")
	}
	// the whole text, each line escaped by the daemon
	body := q.Body
	if body == "" {
		body = q.TextLine
	}
	for i, line := range strings.Split(body, "\n") {
		label := "text"
		if i > 0 {
			label = ""
		}
		fmt.Fprintf(w, "  %-9s | %s\n", label, line)
	}
	for i, f := range q.Files {
		fmt.Fprintf(w, "  file %d    %s\n", i+1, f.Path)
		kept := ""
		if f.Kept {
			kept = ", kept from the message replaced"
		}
		fmt.Fprintf(w, "            %s, %s, sha256 %s%s\n", f.Name, sizeText(f.Size), f.SHA256, kept)
	}
	fmt.Fprintf(w, "  terms     %s\n", q.Terms)
	if q.Deadline != "" {
		fmt.Fprintf(w, "  Expires %s, declined unless approved\n", clock(q.Deadline))
	}
}

// confirm lists requests, or decides one. Deciding is asked at a terminal
// and recorded; no agent tool reaches it.
func confirm(c *cmd, args []string) error {
	decline := c.fs.Bool("decline", false, "decline instead of approving")
	if err := c.fs.Parse(permute(c.fs, args)); err != nil || c.fs.NArg() > 1 {
		return usagef("a2p2 confirm [ID] [--decline]")
	}
	var qs localapi.RequestsResp
	if c.fs.NArg() == 0 {
		return c.call("requests", localapi.Empty{}, &qs, func() {
			if len(qs.Requests) == 0 {
				fmt.Println("No pending requests.")
			}
			for _, q := range qs.Requests {
				printTerms(os.Stdout, q)
				fmt.Println()
			}
			if len(qs.Requests) > 0 {
				fmt.Println("Run a2p2 confirm ID to approve, or a2p2 confirm ID --decline to decline.")
			}
		})
	}
	if !terminal() {
		return errors.New("run this confirm command in a terminal to decide the request")
	}
	id := c.fs.Arg(0)
	cl, err := c.client()
	if err != nil {
		return err
	}
	if err := cl.Call(c.ctx, "requests", localapi.Empty{}, &qs); err != nil {
		return err
	}
	var q *localapi.Request
	for i := range qs.Requests {
		if qs.Requests[i].ID == id {
			q = &qs.Requests[i]
		}
	}
	if q == nil {
		return errors.New("no request " + id + " is waiting; run a2p2 confirm to list them")
	}
	verb := "Approve"
	if *decline {
		verb = "Decline"
	}
	printTerms(os.Stderr, *q)
	if ask(verb+" request "+id+"? Type yes: ") != "yes" {
		return errors.New("nothing decided")
	}
	send := func(q localapi.ConfirmReq) (localapi.ConfirmResp, []byte, error) {
		var r localapi.ConfirmResp
		raw, err := cl.CallRaw(c.ctx, "confirm", q, &r)
		return r, raw, err
	}
	// the text and terms digest shown go with the decision; the daemon
	// refuses a decision on a request whose text changed since, and an
	// approval whose terms are not the request's current ones
	r, raw, err := decide(send, ask, os.Stderr, localapi.ConfirmReq{ID: id, Approve: !*decline, Terms: q.Terms, Text: q.Text})
	var ue *localapi.Error
	if errors.As(err, &ue) && ue.Msg == node.RequestChangedText {
		return errors.New("request " + id + " changed since it was shown; nothing was decided. Run a2p2 confirm " + id + " again to see it as it is now")
	}
	if err != nil {
		return err
	}
	if *c.json {
		_, err := os.Stdout.Write(raw)
		return err
	}
	fmt.Printf("%s %s", r.ID, r.State)
	if r.Effect != "" {
		fmt.Printf(" (%s)", r.Effect)
	}
	if r.Reason != "" {
		fmt.Printf(": %s", node.EscapeDisplay(r.Reason))
	}
	fmt.Println()
	return nil
}

// decide sends a decision. If an approval answers not_caught_up (this
// machine has not checked the room's log with another member since it
// started), it shows the risk on warn and asks again; only a second
// "yes" repeats the approval with not_caught_up_ok, which is recorded.
func decide(send func(localapi.ConfirmReq) (localapi.ConfirmResp, []byte, error), ask func(string) string, warn io.Writer,
	q localapi.ConfirmReq) (localapi.ConfirmResp, []byte, error) {
	r, raw, err := send(q)
	if err != nil || !q.Approve || r.Reason != node.ReasonNotCaughtUp {
		return r, raw, err
	}
	fmt.Fprintf(warn, "\nWARNING: %s\n\n", node.NotCaughtUpWarning)
	if ask("Approve anyway, accepting this risk? Type yes: ") != "yes" {
		return r, raw, errors.New("nothing decided; the request still waits")
	}
	q.NotCaughtUpOK = true
	return send(q)
}

func alerts(c *cmd, args []string) error {
	wait := c.fs.Int("wait", 0, "wait up to this many seconds (at most 120) for something")
	if err := c.fs.Parse(permute(c.fs, args)); err != nil {
		return errUsage
	}
	waitMS, err := waitSeconds(*wait)
	if err != nil {
		return err
	}
	var res localapi.AlertsResp
	return c.call("alerts", localapi.WaitReq{WaitMS: waitMS}, &res, func() {
		if len(res.Alerts) == 0 {
			fmt.Println("No alerts.")
		}
		for _, a := range res.Alerts {
			printAlert(os.Stdout, a)
		}
	})
}

// printAlert shows one alert with the command that deals with it. The
// daemon has escaped what other machines wrote in its text; a request's
// text is shown as confirm shows it (approvalText), and the lines of a
// question's body after its first are indented deeper than the asker's
// line, so a body cannot draw a line that reads as another asker with a
// mark (D65).
func printAlert(w io.Writer, a localapi.Alert) {
	switch a.Kind {
	case node.AlertRequest:
		fmt.Fprintf(w, "Request %s\n  %s\n  Review with a2p2 confirm %s\n", a.ID, approvalText(a.Text, "    "), a.ID)
	case node.AlertQuestion:
		fmt.Fprintf(w, "Question %s in room %s\n  %s\n  Answer with a2p2 send --room %s --reply-to %s TEXT\n",
			a.ID, a.Room, strings.ReplaceAll(a.Text, "\n", "\n    "), a.Room, a.ID)
	case node.AlertNetwork:
		fmt.Fprintf(w, "Networking\n  %s\n  Choose another address with a2p2 net on ADDR, or run a2p2 net off\n", a.Text)
	case node.AlertRemoved:
		fmt.Fprintf(w, "Room %s\n  %s\n", a.Room, approvalText(a.Text, "    "))
	default:
		fmt.Fprintf(w, "%s %s in room %s\n  %s\n", a.Kind, a.ID, a.Room, a.Text)
	}
}

// advertisedWarning is said before a2p2 net on: who learns the addresses
// it advertises. a2p2 status lists them once it listens.
const advertisedWarning = "Every member of your rooms and every holder of an invite gets the addresses it advertises, " +
	"which can include VPN and office network addresses. a2p2 status lists them once it listens."

// netCmd is a person's networking decision: shown exactly, asked at a
// terminal, recorded by the daemon (a decided request of kind network),
// and applied by the daemon at once.
func netCmd(c *cmd, args []string) error {
	if len(args) > 0 && args[0] == "check" {
		return netCheck(c, args[1:]) // observes only: no decision, no terminal needed
	}
	outbound := c.fs.Bool("outbound-only", false, "connect out only; listen on nothing")
	mapPort := c.fs.Bool("map-port", false, "also ask the router to forward the listening port")
	if err := c.fs.Parse(permute(c.fs, args)); err != nil {
		return errUsage
	}
	rest := c.fs.Args()
	if len(rest) == 0 || (rest[0] != "on" && rest[0] != "off") || len(rest) > 2 ||
		rest[0] == "off" && (len(rest) > 1 || *outbound || *mapPort) || *outbound && (len(rest) > 1 || *mapPort) {
		return usagef("a2p2 net on [ADDR] [--map-port] | net on --outbound-only | net off")
	}
	if !terminal() {
		return errors.New("run this networking command in a terminal to review and apply it")
	}
	if err := daemon.CheckHome(c.home); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	req := localapi.NetReq{State: "off"}
	var advertise []string // config.json's advertise, which replaces the detected addresses
	switch {
	case rest[0] == "on" && *outbound:
		req.State = "outbound"
	case rest[0] == "on":
		req.State = "on"
		cfg, err := daemon.LoadConfig(c.home)
		if err != nil {
			return err
		}
		advertise = cfg.Advertise
		req.Listen = daemon.DefaultListen
		if cfg.Listen != "" {
			req.Listen = cfg.Listen
		}
		if len(rest) == 2 {
			req.Listen = rest[1]
		}
	}
	what := map[string]string{
		"off":      "This machine will stop listening for connections and connecting to other machines.",
		"outbound": "This machine will connect out to room members and to admins when joining a room. It will accept no incoming connections.",
		"on":       "This machine will listen on " + addressText(req.Listen) + ". Anyone who can reach this address can connect. Only room members can fetch room messages. This machine will also connect out.",
	}[req.State]
	fmt.Fprintln(os.Stderr, what)
	if req.State == "on" {
		if len(advertise) > 0 {
			// The daemon gives these instead of the addresses it detects
			// (less any the listener cannot serve, which status names), so
			// the detected ones FamiliesText lists would be wrong here.
			fmt.Fprintln(os.Stderr, "It advertises the addresses set in config.json instead of the ones it detects: "+escapeAll(advertise, ", ")+".")
		} else {
			fmt.Fprintln(os.Stderr, daemon.FamiliesText(req.Listen))
		}
		fmt.Fprintln(os.Stderr, advertisedWarning)
	}
	if *mapPort {
		req.MapPort = true
		fmt.Fprintln(os.Stderr, node.MapPortText)
	}
	// Port mapping an earlier version recorded is not applied (D68): say
	// what this decision does with it. Only a running daemon is asked;
	// none is started before the person says yes.
	cl, ok := c.cl, c.cl != nil
	if !ok {
		cl, ok = daemon.Running(c.ctx, c.home)
	}
	if ok {
		var st localapi.Status
		if cl.Call(c.ctx, "status", localapi.StatusReq{}, &st) == nil && st.Network.MapPortEarlier {
			s := "Port mapping was turned on by an earlier version of a2p2 and is off since the upgrade; this decision "
			switch {
			case *mapPort:
				s += "turns it on again."
			case req.State == "on":
				s += "keeps it off. Add --map-port to turn it on again."
			default:
				s += "keeps it off."
			}
			fmt.Fprintln(os.Stderr, s)
		}
	}
	fmt.Fprintln(os.Stderr, "The daemon applies it as soon as it is recorded.")
	if ask("Apply this? Type yes: ") != "yes" {
		return errors.New("nothing changed")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	var res localapi.NetResp
	if err := cl.Call(c.ctx, "net", req, &res); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "recorded as decision %s\n", res.Request)
	// The daemon applies it by itself; wait to show the result.
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		var st localapi.Status
		if cl.Call(c.ctx, "status", localapi.StatusReq{}, &st) == nil && st.Network.Request == res.Request &&
			st.Network.Reason != "being applied" {
			return status(c)
		}
	}
	return errors.New("the decision is recorded; the daemon has not applied it yet. Run a2p2 status to check")
}

// daemonVersionNote compares the running daemon's version with this
// binary's. line is empty when they are the same or the daemon did not
// report one. cmp is -1 when the daemon is older, 1 when it is newer and 0
// when they differ in a way that cannot be ordered (a development build).
func daemonVersionNote(daemon, binary string) (cmp int, line string) {
	if daemon == "" || daemon == binary {
		return 0, ""
	}
	d, dok := versionCore(daemon)
	b, bok := versionCore(binary)
	if dok && bok {
		for i := range d {
			if d[i] != b[i] {
				cmp = -1
				if d[i] > b[i] {
					cmp = 1
				}
				break
			}
		}
	}
	head := "The running a2p2 daemon is " + node.EscapeDisplay(daemon) + " and this a2p2 is " + binary
	switch cmp {
	case -1:
		return cmp, head + ": the daemon is older. Run a2p2 stop to start this one."
	case 1:
		return cmp, head + ": the daemon is newer. Install the newer a2p2, or run a2p2 stop to start this one."
	}
	return cmp, head + ": they differ. Run a2p2 stop to start this one."
}

// versionCore reads the three numbers of vMAJOR.MINOR.PATCH, ignoring any
// pre-release or build suffix.
func versionCore(v string) (n [3]int, ok bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

func doctor(c *cmd, args []string) error {
	if err := c.fs.Parse(args); err != nil {
		return errUsage
	}
	exe, _ := os.Executable()
	return runDoctor(c, os.Stdout, harness.Options{Binary: exe})
}

// noHarnessHint is the closing line of doctor when no harness is connected.
// The harness list is the one install accepts.
func noHarnessHint() string {
	return "No harness is connected. Run: a2p2 install " + harness.Names[0] +
		" (or " + strings.Join(harness.Names[1:], ", ") + "), then a2p2 doctor again."
}

// personUseText says how often a2p2 acted as the person with --person
// where no person was seen typing (away from a terminal or in an agent's
// shell), since the daemon started.
func personUseText(u localapi.Unattended) string {
	return fmt.Sprintf("--person used %d time(s) away from a terminal or in an agent's shell since the daemon started, last %s (a2p2 %s)",
		u.Count, clock(u.Last), node.EscapeDisplay(u.Command))
}

// quarantineChecks is one failing doctor line per room set aside, with
// what the person can do about it.
func quarantineChecks(st localapi.Status) []harness.Check {
	var out []harness.Check
	for _, q := range st.Quarantined {
		out = append(out, harness.Check{Name: "room " + node.EscapeDisplay(q.Label) + " (" + short(q.ID) + ")",
			Info: node.QuarantineText + " (" + node.EscapeDisplay(q.Reason) + ")",
			Fix:  node.QuarantineFix(q.ID, q.ID == st.Local)})
	}
	return out
}

// runDoctor writes the checks to w. o says how to look for harnesses.
func runDoctor(c *cmd, w io.Writer, o harness.Options) error {
	checks := []harness.Check{}
	add := func(name string, err error, info, fix string) {
		ch := harness.Check{Name: name, OK: err == nil, Info: info}
		if err != nil {
			ch.Info, ch.Fix = node.EscapeDisplay(err.Error()), fix
		}
		checks = append(checks, ch)
	}
	homeErr := daemon.PrepareHome(c.home)
	add("state directory", homeErr, c.home, "fix the permissions shown above")
	// Warnings pass: the home works, and the person decides.
	var warns []harness.Check
	for _, w := range homeWarnings(c.home) {
		warns = append(warns, harness.Check{Name: "state directory", OK: true, Info: "warning: " + node.EscapeDisplay(w)})
	}
	if homeErr != nil {
		// The daemon cannot be reached through an unsafe directory, so its
		// line says it was not looked at rather than pointing at a log.
		checks = append(checks, harness.Check{Name: "daemon", Info: "not checked: fix the state directory first"})
	} else {
		cl, err := c.client()
		add("daemon", err, "running", "see "+filepath.Join(c.home, "daemon.log"))
		if err == nil {
			var st localapi.Status
			err = cl.Call(c.ctx, "status", nil, &st)
			add("local API", err, fmt.Sprintf("machine %s, %d room(s), %d agent(s)", short(st.Node), len(st.Rooms), len(st.Agents)), "")
			if err == nil {
				if cmp, line := daemonVersionNote(st.Version, version); cmp < 0 {
					checks = append(checks, harness.Check{Name: "daemon version", Info: line, Fix: "a2p2 stop"})
				} else if line != "" {
					warns = append(warns, harness.Check{Name: "daemon version", OK: true, Info: "warning: " + line})
				}
			}
			if u := st.Unattended; u != nil {
				warns = append(warns, harness.Check{Name: "person", OK: true, Info: "warning: " + personUseText(*u)})
			}
			state := st.Network.State
			if st.Network.Applied != "" {
				state = st.Network.Applied
			}
			if len(st.Listen) > 0 {
				add("network", nil, "reachable at "+escapeAll(st.Listen, ", "), "")
			} else if state == node.NetworkOutbound {
				add("network", nil, "outbound only: connects to members, accepts no incoming connections", "")
			} else {
				add("network", nil, "off: this machine only (a2p2 net on to enable)", "")
			}
			checks = append(checks, quarantineChecks(st)...)
			for _, p := range st.Peers {
				info := "heard from at " + clock(p.LastHeard)
				if p.LastHeard == "" {
					info = "not heard from in this run"
				}
				if h := heldText(p.Held); h != "" {
					info += "; " + h
				}
				add("machine "+node.EscapeDisplay(p.Name)+" ("+short(p.Node)+")", nil, info, "")
			}
		}
	}
	hs := harness.Doctor(o)
	checks = append(checks, warns...)
	checks = append(checks, hs...)
	bad := 0
	for _, ch := range checks {
		if !ch.OK {
			bad++
		}
	}
	if *c.json {
		if err := writeJSON(w, checks); err != nil {
			return err
		}
	} else {
		for _, ch := range checks {
			mark := "ok  "
			switch {
			case !ch.OK:
				mark = "FAIL"
			case strings.HasPrefix(ch.Info, "warning: "):
				mark = "warn"
			}
			fmt.Fprintf(w, "%s  %-28s %s\n", mark, ch.Name, ch.Info)
			if ch.Fix != "" {
				fmt.Fprintf(w, "      fix: %s\n", ch.Fix)
			}
		}
		if !harness.AnyConnected(hs) {
			fmt.Fprintln(w, noHarnessHint())
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d check(s) failed", bad)
	}
	return nil
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func send(c *cmd, args []string) error {
	roomName := c.fs.String("room", "", "room label or id (default: local)")
	replyTo := c.fs.String("reply-to", "", "message id this replies to")
	askHandled := c.fs.Bool("ask-handled", false, "room-wide post: ask readers to report handling")
	replaces := c.fs.String("replaces", "", "id of your own message: new text, or withdraw it with empty text")
	var to, files multi
	c.fs.Var(&to, "to", "agent ID such as human@1a2b3c4d, or everyone (repeatable)")
	c.fs.Var(&files, "file", "attach this file (repeatable, at most 8); with --replaces, sha256:HEX keeps a file")
	if err := c.fs.Parse(args); err != nil {
		return errUsage
	}
	for i, p := range files {
		if strings.HasPrefix(p, node.KeptPrefix) {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		files[i] = abs
	}
	text := strings.Join(c.fs.Args(), " ")
	if text == "-" || (text == "" && !terminal()) {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, core.MaxBodyBytes+1))
		if err != nil {
			return err
		}
		text = strings.TrimRight(string(b), "\n")
	}
	if text == "" && *replaces == "" {
		return usagef("a2p2 send [flags] TEXT (or text on stdin)")
	}
	me, err := c.who("send", true)
	if err != nil {
		return err
	}
	req := localapi.SendReq{Agent: me, Room: *roomName, Text: text, To: to, ReplyTo: *replyTo, AskHandled: *askHandled,
		Replaces: *replaces, Files: files}
	if len(files) > 0 {
		var w localapi.SendFilesResp
		return c.call("send", req, &w, func() { printSendFiles(os.Stdout, w) })
	}
	var m localapi.SendResp
	return c.call("send", req, &m, func() {
		fmt.Println(m.ID)
		if line := notRegisteredLine(m.NotRegistered); line != "" {
			fmt.Fprintln(os.Stderr, line)
		}
	})
}

// notRegisteredLine is the one sentence for the addressees on this machine
// that have not registered (D73). It names up to three, then "and N more".
func notRegisteredLine(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	who := strings.Join(ids[:min(len(ids), 3)], ", ")
	verb := "has"
	if len(ids) > 1 {
		verb = "have"
	}
	if len(ids) > 3 {
		who += fmt.Sprintf(" and %d more", len(ids)-3)
	}
	it := "it"
	if len(ids) > 1 {
		it = "each"
	}
	return who + " " + verb + " not registered on this machine yet; the message is unread for " + it +
		" when it registers, if this machine still holds it among its newest messages"
}

// filesModeWords says what each files setting of a room does.
var filesModeWords = map[string]string{
	store.FilesOff:   "this machine does not carry other members' files for the room",
	store.FilesCarry: "this machine carries a file a member asks it for, within its budget",
	store.FilesKeep:  "this machine fetches and keeps every file of the room, within its budget",
}

// printSendFiles shows a send with files waiting for a person.
func printSendFiles(w io.Writer, r localapi.SendFilesResp) {
	fmt.Fprintf(w, "request %s %s: a person must confirm on this machine; nothing is sent until then\n", r.Request, r.State)
	fmt.Fprintf(w, "Review it with: a2p2 confirm %s\n", r.Request)
}

// sizeText is a size in bytes, with a rounded size in KiB, MiB or GiB
// above 1 KiB.
func sizeText(n int64) string {
	units := []string{"KiB", "MiB", "GiB"}
	if n < 1024 {
		return fmt.Sprintf("%d bytes", n)
	}
	v, u := float64(n)/1024, 0
	for v >= 1024 && u < len(units)-1 {
		v, u = v/1024, u+1
	}
	return fmt.Sprintf("%.1f %s (%d bytes)", v, units[u], n)
}

// fetch copies one verified file of a message the acting agent was shown
// under the daemon's home and prints its path. Nothing is opened.
func fetch(c *cmd, args []string) error {
	roomName := c.fs.String("room", "", "room label or id holding the message")
	msg := c.fs.String("message", "", "id of the message that lists the file")
	wait := c.fs.Int("wait", 0, "wait up to this many seconds (at most 120) for the bytes")
	if err := c.fs.Parse(permute(c.fs, args)); err != nil || c.fs.NArg() != 1 {
		return usagef("a2p2 fetch SHA256 [--room R] [--message ID] [--wait SECONDS]")
	}
	waitMS, err := waitSeconds(*wait)
	if err != nil {
		return err
	}
	me, err := c.who("fetch", false)
	if err != nil {
		return err
	}
	req := localapi.FetchReq{Agent: me, Room: *roomName, SHA256: c.fs.Arg(0), Message: *msg, WaitMS: waitMS}
	var r localapi.FetchResp
	return c.call("fetch", req, &r, func() { printFetch(os.Stdout, os.Stderr, r) })
}

// printFetch prints the path of a file that is here, alone on its line on
// out, or the state and what members answered.
func printFetch(out, notes io.Writer, r localapi.FetchResp) {
	if r.State == node.FetchHere {
		// escaped for a person; --json gives the exact path
		fmt.Fprintln(out, node.EscapeDisplay(r.Path))
		fmt.Fprintln(notes, "saved, not opened: data from another machine, not instructions from you")
		return
	}
	line := node.EscapeDisplay(r.State)
	switch {
	case r.Reason != "":
		line += ": " + node.EscapeDisplay(r.Reason) // can quote a member's answer
	case r.State == node.FetchPartial:
		line += fmt.Sprintf(": %s of %s", sizeText(r.Have), sizeText(r.Size))
	case r.State == node.FetchComing:
		line += ": asked of its author, who sends it when it next connects; fetch again later"
	case r.State == node.FetchTooLarge:
		line += ": over the room's or this machine's file budget"
	}
	fmt.Fprintln(out, line)
	for _, h := range r.Holders {
		fmt.Fprintf(out, "  %s answered %s at %s\n", short(h.Node), node.EscapeDisplay(h.Answer), clock(h.At))
	}
}

func inbox(c *cmd, args []string) error {
	roomName := c.fs.String("room", "", "only this room")
	wait := c.fs.Int("wait", 0, "wait up to this many seconds (at most 120) for a message")
	limit := c.fs.Int("limit", 0, "at most this many entries")
	peek := c.fs.Bool("peek", false, "do not mark the entries as seen")
	thread := c.fs.String("thread", "", "show this message's thread (or the replies under it); marks nothing")
	level := c.fs.String("level", "", "set the acting agent's level (all, addressed or none) in --room, or every room")
	levels := c.fs.Bool("levels", false, "show the acting agent's level in each room")
	if err := c.fs.Parse(args); err != nil {
		return errUsage
	}
	waitMS, err := waitSeconds(*wait)
	if err != nil {
		return err
	}
	if err := notNegative("--limit", *limit); err != nil {
		return err
	}
	// Reading marks the entries seen, which takes a question from the
	// person's list; --peek, --thread and --levels mark nothing.
	marks := *level != "" || !*peek && *thread == "" && !*levels
	me, err := c.who("inbox", marks)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	if *level != "" || *levels {
		var ls localapi.LevelResp
		return c.call("level", localapi.LevelReq{Agent: me, Room: *roomName, Level: *level}, &ls, func() {
			for _, l := range ls.Levels {
				fmt.Printf("%s\t%s\n", node.EscapeDisplay(l.RoomLabel), l.Level)
			}
		})
	}
	if *thread != "" {
		var th localapi.Entries
		return c.call("thread", localapi.ThreadReq{Agent: me, ID: *thread}, &th, func() {
			for _, e := range th.Entries {
				printEntry(os.Stdout, e)
			}
		})
	}
	var res localapi.InboxResp
	raw, err := cl.CallRaw(c.ctx, "inbox", localapi.InboxReq{Agent: me, Room: *roomName, Limit: *limit, WaitMS: waitMS}, &res)
	if err != nil {
		return err
	}
	// Build the output first and confirm only after it was written: if
	// stdout is broken the entries must stay unread.
	var buf strings.Builder
	if *c.json {
		buf.Write(raw)
	} else {
		if len(res.Entries) == 0 {
			buf.WriteString("no unread messages\n")
		}
		for _, e := range res.Entries {
			printEntry(&buf, e)
		}
		if len(res.Waiting) > 0 {
			fmt.Fprintf(&buf, "(%d stream(s) still being fetched from other machines)\n", len(res.Waiting))
		}
		if res.More > 0 {
			fmt.Fprintf(&buf, "(%d more unread: run a2p2 inbox again)\n", res.More)
		}
	}
	if _, err := io.WriteString(os.Stdout, buf.String()); err != nil {
		return err
	}
	if *peek || len(res.Entries) == 0 {
		return nil
	}
	seen := localapi.SeenReq{Agent: me}
	for _, e := range res.Entries {
		seen.Entries = append(seen.Entries, localapi.SeenEntry{Key: e.Key, Version: e.Version})
	}
	return cl.Call(c.ctx, "seen", seen, nil)
}

func history(c *cmd, args []string) error {
	roomName := c.fs.String("room", "", "room label or id (default: local)")
	n := c.fs.Int("n", 50, "how many messages")
	before := c.fs.String("before", "", "only messages before this message id")
	skipped := c.fs.Bool("skipped", false, "only what the acting agent's level kept out of its inbox (every room unless --room)")
	if err := c.fs.Parse(args); err != nil {
		return errUsage
	}
	if err := notNegative("-n", *n); err != nil {
		return err
	}
	// As the acting agent: entries its level kept out are marked, and it
	// may then report one it was shown handled.
	me, err := c.who("history", false)
	if err != nil {
		return err
	}
	req := localapi.HistoryReq{Agent: me, Room: *roomName, Limit: *n, Before: *before, Skipped: *skipped}
	var res localapi.Entries
	return c.call("history", req, &res, func() {
		if len(res.Entries) == 0 {
			fmt.Println("No messages in this part of the room history.")
		}
		for _, e := range res.Entries {
			printEntry(os.Stdout, e)
		}
	})
}

// writeJSON prints v as indented JSON with text as written: '<', '>' and
// '&' are not escaped, as in the local API's own responses.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// heldText says, for status and doctor, how many of a machine's addresses
// are kept and not dialled, and why (D71); "" when none. Every string is
// another machine's or the store's, so each is escaped for the terminal.
func heldText(held []node.HeldAddr) string {
	if len(held) == 0 {
		return ""
	}
	parts := make([]string, 0, len(held))
	for _, h := range held {
		parts = append(parts, node.EscapeDisplay(h.Addr)+" ("+node.EscapeDisplay(h.Why)+")")
	}
	n := "1 address is"
	if len(held) > 1 {
		n = fmt.Sprintf("%d addresses are", len(held))
	}
	return n + " held and not dialled, on another machine's word: " + strings.Join(parts, ", ") +
		"; " + heldFix
}

// heldFix is what a person does about a held address they want dialled:
// an invite this machine's person accepts is the only way an address the
// rule refuses is dialled (D71), and a member may join again.
const heldFix = "to have this machine dial one, ask that machine's person for an invite and run a2p2 join with it"
