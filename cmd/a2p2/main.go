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
	"os"
	"os/exec"
	"os/signal"
	"runtime"
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
  a2p2 install claude|codex|agy|opencode|hermes|pi [--on-stop] [--dry-run]
                                  connect a harness (undo with: a2p2 uninstall NAME)
                                  --on-stop also checks for messages when a turn ends.
  a2p2 ui                         open the local web page
  a2p2 doctor                     check the daemon and harness connections

Talking (as the human, or --as <label> for a script)
  a2p2 send [--room R] [--to AGENT]... [--reply-to ID] [--ask-handled] [--replaces ID] TEXT...
                                  Use --to label@machine, for example human@1a2b3c4d.
                                  Find machine IDs with a2p2 room list ROOM.
                                  Empty text with --replaces ID withdraws your message.
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
  a2p2 room leave ROOM            stop carrying a room on this machine

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
		fmt.Fprintln(os.Stderr, "a2p2:", err)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "a2p2:", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

func usagef(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, a...))
}

// cmd bundles what most subcommands need.
type cmd struct {
	ctx  context.Context
	fs   *flag.FlagSet
	json *bool
	as   *string
	home string
	cl   *localapi.Client
}

func newCmd(ctx context.Context, name string) *cmd {
	c := &cmd{ctx: ctx, fs: flag.NewFlagSet(name, flag.ContinueOnError), home: daemon.Home()}
	c.json = c.fs.Bool("json", false, "print JSON")
	c.as = c.fs.String("as", "", "act as this agent label instead of the human")
	return c
}

func (c *cmd) client() (*localapi.Client, error) {
	if c.cl != nil {
		return c.cl, nil
	}
	cl, err := daemon.Connect(c.ctx, c.home)
	c.cl = cl
	return cl, err
}

// who resolves the acting agent: the human, or a registered script label.
func (c *cmd) who() (string, error) {
	cl, err := c.client()
	if err != nil {
		return "", err
	}
	var a store.Agent
	req := localapi.RegisterReq{Harness: "human", Label: "human"}
	if *c.as != "" {
		req = localapi.RegisterReq{Harness: "cli", Label: *c.as}
	}
	err = cl.Call(c.ctx, "register", req, &a)
	return a.ID, err
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
func terminal() bool { return isTerminal() }

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
	}

	// Everything below talks to the daemon.
	if err := c.fs.Parse(permute(c.fs, args)); err != nil {
		return errUsage
	}
	rest := c.fs.Args()
	switch name {
	case "status":
		return status(c)
	case "ui":
		var st localapi.Status
		if err := c.call("status", nil, &st, nil); err != nil {
			return err
		}
		fmt.Println(st.UI)
		if runtime.GOOS == "darwin" {
			_ = exec.Command("open", st.UI).Start()
		} else {
			_ = exec.Command("xdg-open", st.UI).Start()
		}
		return nil
	case "trace":
		if len(rest) != 1 {
			return usagef("a2p2 trace ID")
		}
		return trace(c, rest[0])
	case "ack":
		if len(rest) != 1 {
			return usagef("a2p2 ack ID")
		}
		me, err := c.who()
		if err != nil {
			return err
		}
		var r localapi.AckResp
		return c.call("ack", localapi.AckReq{Agent: me, ID: rest[0]}, &r, func() { fmt.Println(ackText(r)) })
	}
	return usagef("unknown command %q; run a2p2 help", name)
}

// installArgs reads install and uninstall arguments: the harness name and
// its flags, in either order.
func installArgs(fs *flag.FlagSet, name string, args []string) (string, harness.Options, error) {
	onStop := fs.Bool("on-stop", false, "also check for mail when the agent finishes a turn (may start another turn)")
	dry := fs.Bool("dry-run", false, "show what would change")
	if err := fs.Parse(permute(fs, args)); err != nil || fs.NArg() != 1 {
		return "", harness.Options{}, usagef("a2p2 %s <%s> [--on-stop] [--dry-run]", name, strings.Join(harness.Names, "|"))
	}
	return fs.Arg(0), harness.Options{OnStop: *onStop, DryRun: *dry}, nil
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

func clock(t string) string {
	if tm, err := time.Parse(time.RFC3339, t); err == nil {
		return tm.Local().Format("Jan 02 15:04:05")
	}
	return t
}

func printEntry(w io.Writer, e localapi.Entry) {
	head := e.From
	if len(e.To) > 0 {
		head += " → " + strings.Join(e.To, ", ")
	}
	if e.Kind != "message" {
		head += " [" + e.Kind + "]"
	}
	if e.Parent != "" {
		head += " (reply to " + e.Parent
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
		fmt.Fprintf(w, "    %s\n", strings.ReplaceAll(e.Text, "\n", "\n    "))
	}
	if len(e.Choices) > 0 {
		fmt.Fprintf(w, "    choices: %s\n", strings.Join(e.Choices, " | "))
	}
	fmt.Fprintf(w, "    id %s\n", e.ID)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// Label loopback addresses without implying that a public or LAN address
// was tested from another machine.
func addressText(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil && (net.ParseIP(host).IsLoopback() || strings.EqualFold(host, "localhost")) {
		return addr + " (this machine only)"
	}
	return addr
}

func status(c *cmd) error {
	var st localapi.Status
	defer func() {
		// Printed (as text or JSON): a person has seen the notice.
		if st.Notice != "" && c.cl != nil {
			_ = c.cl.Call(c.ctx, "status", localapi.StatusReq{NoticeShown: true}, nil)
		}
	}()
	return c.call("status", localapi.StatusReq{ShowNotice: true}, &st, func() {
		if st.Notice != "" {
			fmt.Println("NOTICE   " + st.Notice)
		}
		fmt.Printf("machine  %s (%s)  a2p2 %s  pid %d\n", st.Name, short(st.Node), st.Version, st.PID)
		switch {
		case st.Network.Reason == "being applied":
			fmt.Printf("network  %s decided, being applied (still %s)\n", st.Network.State, st.Network.Applied)
		case st.Network.Applied != "":
			fmt.Printf("network  %s, %s\n", st.Network.State, st.Network.Reason)
		case st.Network.State == "on":
			fmt.Printf("network  on, listening on %s\n", addressText(st.Network.Listen))
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
				reach += " (" + st.Reach.Why + ")"
			}
			fmt.Println("         " + reach + "; a2p2 net check explains")
		}
		if st.Mapping != nil {
			fmt.Println("         " + mappingText(st.Mapping))
		}
		if st.Waiting > 0 {
			fmt.Printf("requests %d pending. Run a2p2 confirm to review.\n", st.Waiting)
		}
		fmt.Println("ui       run a2p2 ui to open the local page")
		fmt.Println("rooms")
		labels := make(map[string]int, len(st.Rooms))
		for _, r := range st.Rooms {
			labels[r.Label]++
		}
		for _, r := range st.Rooms {
			name := r.Label
			if labels[name] > 1 {
				name += " (" + r.ID + ")"
			}
			fmt.Printf("  %-20s machines: %d\n", name, len(r.Members))
			if r.MayBeRemoved {
				fmt.Println("    " + node.MaybeRemovedText)
			}
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
				fmt.Printf("  %-16s %s  %s  %s\n", p.Name, short(p.Node), strings.Join(p.Addrs, ","), heard)
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
	fmt.Fprintf(w, "message  %s\nroom     %s\nheader   %s\ntext     %s\n", t.ID, t.Room, header, reason)
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
		fmt.Fprintf(w, "  %s (%s), observed %s\n", short(cp.Node), member, clock(cp.At))
	}
	if len(t.Handled) == 0 {
		fmt.Fprintln(w, "handled: no report yet")
	} else {
		fmt.Fprintln(w, "handled reports (time signed by the agent's machine):")
	}
	for _, h := range t.Handled {
		fmt.Fprintf(w, "  by %s at %s\n", h.Agent, clock(h.At))
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
	hist := "without earlier history"
	if pv.History {
		hist = "with earlier history"
	}
	fmt.Fprintf(os.Stderr, "Invite to room %q (%s), as %s, %s, from admin machine %s; expires %s.\n",
		pv.Room.Label, pv.Room.ID, pv.Role, hist, short(pv.Admin), clock(pv.Expires))
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
				fmt.Printf("joined room %s as %s.%s\n", o.Room, o.Role, map[bool]string{true: " " + o.Note, false: ""}[o.Note != ""])
			case "accepted":
				fmt.Println("Invite accepted on this machine. Waiting for admin approval. The daemon will keep checking.")
			default:
				fmt.Printf("%s: %s\n", o.State, o.Reason)
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
				fmt.Printf("Room %s  %s\n", r.Label, r.ID)
				if r.MayBeRemoved {
					fmt.Println("  " + node.MaybeRemovedText)
				}
				for _, m := range r.Members {
					role := "member"
					if m.Admin {
						role = "admin"
					}
					fmt.Printf("  %s %-16s %s\n", short(m.Node), m.Name, role)
				}
				for _, p := range r.Pending {
					fmt.Printf("  waiting for a second admin: %s %s by %s\n", p.Kind, short(p.Target), short(p.Author))
				}
				for _, n := range r.NoEffect {
					fmt.Printf("  no effect: entry %s: %s\n", short(n.Entry), n.Reason)
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
			fmt.Printf("created %q (%s). Invite a machine with: a2p2 room invite %s\n", r.Label, r.ID, r.Label)
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
	case rest[0] == "leave" && len(rest) == 2:
		return c.call("room/leave", localapi.RoomReq{Room: rest[1]}, &localapi.Empty{}, func() {
			fmt.Println("left; its data is deleted here. Other members list this machine until an admin removes it.")
		})
	}
	return usagef("a2p2 room create LABEL | invite ROOM [--history] [--member|--admin] | admin ROOM MACHINE | remove ROOM MACHINE | leave ROOM | list [ROOM]")
}

// The daemon describes the action, room ID, full fingerprint, role, history
// and second signature in Text. Keep those terms together for review.
func printTerms(w io.Writer, q localapi.Request) {
	fmt.Fprintf(w, "Request %s\n  If approved: %s\n", q.ID, q.Text)
	if q.Name != "" && q.Kind != store.RequestNetwork {
		fmt.Fprintln(w, "  The machine chose its own name. Check its fingerprint.")
	}
	if q.Deadline != "" {
		fmt.Fprintf(w, "  Invite expires %s\n", clock(q.Deadline))
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
	r, raw, err := decide(send, ask, os.Stderr, localapi.ConfirmReq{ID: id, Approve: !*decline})
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
		fmt.Printf(": %s", r.Reason)
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
	var res localapi.AlertsResp
	return c.call("alerts", localapi.WaitReq{WaitMS: int64(*wait) * 1000}, &res, func() {
		if len(res.Alerts) == 0 {
			fmt.Println("No alerts.")
		}
		for _, a := range res.Alerts {
			printAlert(os.Stdout, a)
		}
	})
}

// printAlert shows one alert with the command that deals with it.
func printAlert(w io.Writer, a localapi.Alert) {
	switch a.Kind {
	case node.AlertRequest:
		fmt.Fprintf(w, "Request %s\n  %s\n  Review with a2p2 confirm %s\n", a.ID, a.Text, a.ID)
	case node.AlertQuestion:
		fmt.Fprintf(w, "Question %s in room %s\n  %s\n  Answer with a2p2 send --room %s --reply-to %s TEXT\n",
			a.ID, a.Room, a.Text, a.Room, a.ID)
	case node.AlertNetwork:
		fmt.Fprintf(w, "Networking\n  %s\n  Choose another address with a2p2 net on ADDR, or run a2p2 net off\n", a.Text)
	case node.AlertRemoved:
		fmt.Fprintf(w, "Room %s\n  %s\n", a.Room, a.Text)
	default:
		fmt.Fprintf(w, "%s %s in room %s\n  %s\n", a.Kind, a.ID, a.Room, a.Text)
	}
}

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
	switch {
	case rest[0] == "on" && *outbound:
		req.State = "outbound"
	case rest[0] == "on":
		req.State = "on"
		cfg, err := daemon.LoadConfig(c.home)
		if err != nil {
			return err
		}
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
	if *mapPort {
		req.MapPort = true
		fmt.Fprintln(os.Stderr, node.MapPortText)
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

func doctor(c *cmd, args []string) error {
	if err := c.fs.Parse(args); err != nil {
		return errUsage
	}
	checks := []harness.Check{}
	add := func(name string, err error, info, fix string) {
		ch := harness.Check{Name: name, OK: err == nil, Info: info}
		if err != nil {
			ch.Info, ch.Fix = err.Error(), fix
		}
		checks = append(checks, ch)
	}
	add("state directory", daemon.PrepareHome(c.home), c.home, "fix the permissions shown above")
	cl, err := c.client()
	add("daemon", err, "running", "see "+c.home+"/daemon.log")
	if err == nil {
		var st localapi.Status
		err = cl.Call(c.ctx, "status", nil, &st)
		add("local API", err, fmt.Sprintf("machine %s, %d room(s), %d agent(s)", short(st.Node), len(st.Rooms), len(st.Agents)), "")
		if len(st.Listen) == 0 {
			add("network", nil, "off: this machine only (a2p2 net on to enable)", "")
		} else {
			add("network", nil, "reachable at "+strings.Join(st.Listen, ", "), "")
		}
		for _, p := range st.Peers {
			info := "heard from at " + clock(p.LastHeard)
			if p.LastHeard == "" {
				info = "not heard from in this run"
			}
			add("machine "+p.Name+" ("+short(p.Node)+")", nil, info, "")
		}
	}
	exe, _ := os.Executable()
	checks = append(checks, harness.Doctor(harness.Options{Binary: exe})...)
	bad := 0
	for _, ch := range checks {
		if !ch.OK {
			bad++
		}
	}
	if *c.json {
		if err := writeJSON(os.Stdout, checks); err != nil {
			return err
		}
	} else {
		for _, ch := range checks {
			mark := "ok  "
			if !ch.OK {
				mark = "FAIL"
			}
			fmt.Printf("%s  %-28s %s\n", mark, ch.Name, ch.Info)
			if ch.Fix != "" {
				fmt.Printf("      fix: %s\n", ch.Fix)
			}
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
	var to multi
	c.fs.Var(&to, "to", "agent ID such as human@1a2b3c4d, or everyone (repeatable)")
	if err := c.fs.Parse(args); err != nil {
		return errUsage
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
	me, err := c.who()
	if err != nil {
		return err
	}
	req := localapi.SendReq{Agent: me, Room: *roomName, Text: text, To: to, ReplyTo: *replyTo, AskHandled: *askHandled, Replaces: *replaces}
	var m localapi.SendResp
	return c.call("send", req, &m, func() { fmt.Println(m.ID) })
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
	me, err := c.who()
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
				fmt.Printf("%s\t%s\n", l.RoomLabel, l.Level)
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
	raw, err := cl.CallRaw(c.ctx, "inbox", localapi.InboxReq{Agent: me, Room: *roomName, Limit: *limit, WaitMS: int64(*wait) * 1000}, &res)
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
	// As the acting agent: entries its level kept out are marked, and it
	// may then report one it was shown handled.
	me, err := c.who()
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
