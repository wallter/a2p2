// Command a2p2 is the whole product: daemon, CLI, MCP shim and harness hook
// in one binary. See docs/DESIGN.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/wallter/a2p2/internal/node"
	"github.com/wallter/a2p2/internal/store"
)

var version = "dev" // set by -ldflags at release

const usage = `a2p2 - peer-to-peer messaging for AI agents and the people who run them

Getting started
  a2p2 install claude|codex|agy   connect a harness (undo with: a2p2 uninstall ...)
  a2p2 ui                         open the local web page
  a2p2 doctor                     check that everything is wired up

Talking (as the human, or --as <label> for a script)
  a2p2 send [--room R] [--to AGENT] [--reply ID] TEXT...
  a2p2 inbox [--room R] [--wait SECONDS]
  a2p2 history [--room R] [-n 50]
  a2p2 ack ID                     mark a message handled
  a2p2 trace ID                   where a message got to, per recipient

Rooms across machines
  a2p2 net on [ADDR]              accept peer connections (default 0.0.0.0:7423)
  a2p2 net off                    this machine only
  a2p2 room create LABEL          make a room this node owns
  a2p2 room invite ROOM           print a one-time invite (valid 15 minutes)
  a2p2 join [TOKEN]               join a room (reads the token from stdin if omitted)
  a2p2 room list
  a2p2 room remove ROOM NODE      owner only

Plumbing
  a2p2 status | stop | daemon | version
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
}

func newCmd(ctx context.Context, name string) *cmd {
	c := &cmd{ctx: ctx, fs: flag.NewFlagSet(name, flag.ContinueOnError), home: daemon.Home()}
	c.json = c.fs.Bool("json", false, "print JSON")
	c.as = c.fs.String("as", "", "act as this agent label instead of the human")
	return c
}

func (c *cmd) client() (*localapi.Client, error) { return daemon.Connect(c.ctx, c.home) }

// who resolves the acting agent: the human, or a registered script label.
func (c *cmd) who(cl *localapi.Client) (string, error) {
	var a store.Agent
	req := localapi.RegisterReq{Harness: "human", Label: core.HumanName}
	if *c.as != "" {
		req = localapi.RegisterReq{Harness: "cli", Label: *c.as}
	}
	err := cl.Call(c.ctx, "register", req, &a)
	return a.ID, err
}

func (c *cmd) out(v any, human func()) error {
	if *c.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	human()
	return nil
}

func run(ctx context.Context, name string, args []string) error {
	c := newCmd(ctx, name)
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
		onStop := c.fs.Bool("on-stop", false, "also check for mail when the agent finishes a turn (may start another turn)")
		dry := c.fs.Bool("dry-run", false, "show what would change")
		if err := c.fs.Parse(args); err != nil || c.fs.NArg() != 1 {
			return usagef("a2p2 %s <%s> [--on-stop] [--dry-run]", name, strings.Join(harness.Names, "|"))
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		o := harness.Options{Binary: exe, OnStop: *onStop, DryRun: *dry}
		fn := harness.Install
		if name == "uninstall" {
			fn = harness.Uninstall
		}
		report, err := fn(c.fs.Arg(0), o)
		fmt.Print(report)
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
	}

	// Everything below talks to the daemon.
	if err := c.fs.Parse(permute(args)); err != nil {
		return errUsage
	}
	rest := c.fs.Args()
	switch name {
	case "status":
		return status(c)
	case "ui":
		cl, err := c.client()
		if err != nil {
			return err
		}
		var st localapi.Status
		if err := cl.Call(ctx, "status", nil, &st); err != nil {
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
	case "join":
		return join(c, rest)
	case "room":
		return room(c, rest)
	case "ack":
		if len(rest) != 1 {
			return usagef("a2p2 ack ID")
		}
		cl, err := c.client()
		if err != nil {
			return err
		}
		me, err := c.who(cl)
		if err != nil {
			return err
		}
		return cl.Call(ctx, "ack", localapi.AckReq{Agent: me, ID: rest[0]}, nil)
	}
	return usagef("unknown command %q; run a2p2 help", name)
}

// permute moves flags before positional arguments so "a2p2 trace ID --json"
// works. Flags that take a value must use the --flag=value form after a
// positional argument.
func permute(args []string) []string {
	var flags, pos []string
	for i, a := range args {
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 && len(pos) > 0 {
			flags = append(flags, a)
		} else if len(pos) > 0 || !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		} else {
			flags = append(flags, a)
		}
	}
	return append(flags, pos...)
}

func printMsg(w io.Writer, m store.Message) {
	to := ""
	if len(m.To) > 0 {
		to = " → " + strings.Join(m.To, ", ")
	}
	kind := ""
	if m.Kind != core.KindMessage {
		kind = " [" + string(m.Kind) + "]"
	}
	fmt.Fprintf(w, "%s  %s%s%s\n    %s\n    id %s\n", m.ReceivedAt.Local().Format("Jan 02 15:04:05"), m.From, to, kind,
		strings.ReplaceAll(m.Body.Text, "\n", "\n    "), m.ID)
	if len(m.Body.Choices) > 0 {
		fmt.Fprintf(w, "    choices: %s\n", strings.Join(m.Body.Choices, " | "))
	}
}

func status(c *cmd) error {
	cl, err := c.client()
	if err != nil {
		return err
	}
	var st localapi.Status
	if err := cl.Call(c.ctx, "status", nil, &st); err != nil {
		return err
	}
	return c.out(st, func() {
		fmt.Printf("node     %s (%s)  a2p2 %s  pid %d\n", st.Name, core.ShortNode(st.Node), st.Version, st.PID)
		if len(st.Listen) == 0 {
			fmt.Println("network  off (this machine only; enable with: a2p2 net on)")
		} else {
			fmt.Printf("network  listening, reachable at %s\n", strings.Join(st.Listen, ", "))
		}
		fmt.Printf("ui       %s\n", st.UI)
		fmt.Println("rooms")
		for _, r := range st.Rooms {
			role := "member"
			if r.Owner == st.Node {
				role = "owner"
			}
			fmt.Printf("  %-20s %s  %d node(s)  %s\n", r.Label, r.ID, len(r.Members), role)
		}
		fmt.Println("agents on this node")
		for _, a := range st.Agents {
			fmt.Printf("  %-32s %-8s last seen %s\n", a.ID, a.Harness, a.LastSeen.Local().Format("Jan 02 15:04"))
		}
		if len(st.Peers) > 0 {
			fmt.Println("peers")
			for _, p := range st.Peers {
				state := "not contacted yet"
				if p.LastError != "" {
					state = "PROBLEM: " + p.LastError
				} else if !p.LastOK.IsZero() {
					state = "ok " + p.LastOK.Local().Format("15:04:05")
				}
				fmt.Printf("  %-16s %s  %s  %s\n", p.Name, core.ShortNode(p.Node), strings.Join(p.Addrs, ","), state)
			}
		}
		for _, o := range st.Outbox {
			fmt.Printf("outbox   %d queued for %s, oldest %s  %s\n", o.Queued, core.ShortNode(o.Node),
				time.Since(o.Oldest).Round(time.Second), o.LastError)
		}
	})
}

func trace(c *cmd, id string) error {
	cl, err := c.client()
	if err != nil {
		return err
	}
	var t node.Trace
	if err := cl.Call(c.ctx, "trace", localapi.IDReq{ID: id}, &t); err != nil {
		return err
	}
	return c.out(t, func() {
		printMsg(os.Stdout, t.Message)
		fmt.Printf("room %s  seq %d  created %s\n", t.Message.Room, t.Message.Seq, t.Message.CreatedAt.Local().Format(time.RFC3339))
		if len(t.Deliveries) == 0 {
			fmt.Println("deliveries: none (local room, or received from another node)")
		}
		for _, d := range t.Deliveries {
			fmt.Printf("  node %s  %-8s attempts %d  %s  %s\n", core.ShortNode(d.Node), d.State, d.Attempts,
				d.UpdatedAt.Local().Format("15:04:05"), d.LastError)
		}
		for _, k := range t.Marks {
			fmt.Printf("  %-8s by %s at %s\n", k.State, k.Agent, k.At.Local().Format("15:04:05"))
		}
	})
}

func join(c *cmd, rest []string) error {
	var token string
	switch {
	case len(rest) == 1 && rest[0] != "-":
		token = rest[0]
	case len(rest) <= 1:
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<10))
		if err != nil {
			return err
		}
		token = string(b)
	default:
		return usagef("a2p2 join [TOKEN]")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	var r store.Room
	if err := cl.Call(c.ctx, "room/join", localapi.JoinReq{Token: token}, &r); err != nil {
		return err
	}
	return c.out(r, func() {
		fmt.Printf("joined %q (%s), owned by node %s. Check that fingerprint with the person who invited you.\n",
			r.Label, r.ID, core.ShortNode(r.Owner))
	})
}

func room(c *cmd, rest []string) error {
	if len(rest) == 0 {
		rest = []string{"list"}
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	switch {
	case rest[0] == "list" && len(rest) == 1:
		return status(c)
	case rest[0] == "create" && len(rest) == 2:
		var r store.Room
		if err := cl.Call(c.ctx, "room/create", localapi.RoomReq{Room: rest[1]}, &r); err != nil {
			return err
		}
		return c.out(r, func() {
			fmt.Printf("created %q (%s). Invite someone with: a2p2 room invite %s\n", r.Label, r.ID, r.Label)
		})
	case rest[0] == "invite" && len(rest) == 2:
		var inv localapi.InviteResp
		if err := cl.Call(c.ctx, "room/invite", localapi.RoomReq{Room: rest[1]}, &inv); err != nil {
			return err
		}
		return c.out(inv, func() {
			fmt.Println(inv.Token)
			fmt.Fprintf(os.Stderr, "\nOne use, expires %s. Anyone holding it can join %q, so send it privately.\nThey run: a2p2 join   (and paste it)\n",
				inv.Expires.Local().Format("15:04"), rest[1])
		})
	case rest[0] == "remove" && len(rest) == 3:
		var r store.Room
		if err := cl.Call(c.ctx, "room/remove", localapi.RemoveReq{Room: rest[1], Node: rest[2]}, &r); err != nil {
			return err
		}
		return c.out(r, func() {
			fmt.Printf("removed; %q now has %d node(s). Other members stop sending to it when they next refresh (within a minute if this node is reachable).\n", r.Label, len(r.Members))
		})
	}
	return usagef("a2p2 room create LABEL | invite ROOM | remove ROOM NODE | list")
}

func netCmd(c *cmd, args []string) error {
	if len(args) == 0 || (args[0] != "on" && args[0] != "off") || len(args) > 2 {
		return usagef("a2p2 net on [ADDR] | off")
	}
	if err := daemon.PrepareHome(c.home); err != nil {
		return err
	}
	cfg, err := daemon.LoadConfig(c.home)
	if err != nil {
		return err
	}
	cfg.Listen = ""
	if args[0] == "on" {
		cfg.Listen = daemon.DefaultListen
		if len(args) == 2 {
			cfg.Listen = args[1]
		}
	}
	if err := daemon.SaveConfig(c.home, cfg); err != nil {
		return err
	}
	if cl, ok := daemon.Running(c.ctx, c.home); ok { // restart to apply
		_ = cl.Call(c.ctx, "stop", nil, nil)
		for i := 0; i < 100; i++ {
			if _, up := daemon.Running(c.ctx, c.home); !up {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return status(c)
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
		add("local API", err, fmt.Sprintf("node %s, %d room(s), %d agent(s)", core.ShortNode(st.Node), len(st.Rooms), len(st.Agents)), "")
		if len(st.Listen) == 0 {
			add("network", nil, "off: this machine only (a2p2 net on to enable)", "")
		} else {
			add("network", nil, "reachable at "+strings.Join(st.Listen, ", "), "")
		}
		for _, p := range st.Peers {
			var perr error
			if p.LastError != "" {
				perr = errors.New(p.LastError)
			}
			add("peer "+p.Name+" ("+core.ShortNode(p.Node)+")", perr, "reachable",
				"check that the other machine is on, running a2p2, and that its address "+strings.Join(p.Addrs, ",")+" is reachable from here")
		}
		for _, o := range st.Outbox {
			if age := time.Since(o.Oldest); age > 10*time.Minute {
				add("outbox for "+core.ShortNode(o.Node), fmt.Errorf("%d message(s) waiting, oldest %s", o.Queued, age.Round(time.Minute)), "", "the peer is unreachable; see above")
			}
		}
	}
	exe, _ := os.Executable()
	checks = append(checks, harness.Doctor(harness.Options{Binary: exe})...)
	bad := 0
	err = c.out(checks, func() {
		for _, ch := range checks {
			mark := "ok  "
			if !ch.OK {
				mark = "FAIL"
				bad++
			}
			fmt.Printf("%s  %-28s %s\n", mark, ch.Name, ch.Info)
			if ch.Fix != "" {
				fmt.Printf("      fix: %s\n", ch.Fix)
			}
		}
	})
	if bad > 0 {
		return fmt.Errorf("%d check(s) failed", bad)
	}
	return err
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func send(c *cmd, args []string) error {
	roomName := c.fs.String("room", "", "room label or id (default: local)")
	reply := c.fs.String("reply", "", "message id this replies to")
	var to multi
	c.fs.Var(&to, "to", "recipient agent id (repeatable; default: everyone in the room)")
	if err := c.fs.Parse(args); err != nil {
		return errUsage
	}
	text := strings.Join(c.fs.Args(), " ")
	if text == "-" || text == "" {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, core.MaxBodyBytes+1))
		if err != nil {
			return err
		}
		text = strings.TrimRight(string(b), "\n")
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	me, err := c.who(cl)
	if err != nil {
		return err
	}
	// A caller-chosen id makes the request safe to retry.
	req := node.SendReq{ID: core.NewID(time.Now()), From: me, Room: *roomName, To: to, Text: text, ReplyTo: *reply}
	var m store.Message
	if err := cl.Call(c.ctx, "send", req, &m); err != nil {
		return err
	}
	return c.out(m, func() { fmt.Println(m.ID) })
}

func inbox(c *cmd, args []string) error {
	roomName := c.fs.String("room", "", "only this room")
	wait := c.fs.Int("wait", 0, "wait up to this many seconds for a message")
	peek := c.fs.Bool("peek", false, "do not mark the messages as seen")
	if err := c.fs.Parse(args); err != nil {
		return errUsage
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	me, err := c.who(cl)
	if err != nil {
		return err
	}
	var res localapi.Messages
	if err := cl.Call(c.ctx, "inbox", localapi.InboxReq{Agent: me, Room: *roomName, WaitMS: *wait * 1000}, &res); err != nil {
		return err
	}
	// Build the output first and confirm only after it was written: if stdout
	// is broken the messages must stay unread.
	var buf strings.Builder
	if *c.json {
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res.Messages); err != nil {
			return err
		}
	} else if len(res.Messages) == 0 {
		buf.WriteString("no unread messages\n")
	}
	if !*c.json {
		for _, m := range res.Messages {
			printMsg(&buf, m)
		}
	}
	if _, err := io.WriteString(os.Stdout, buf.String()); err != nil {
		return err
	}
	if *peek || len(res.Messages) == 0 {
		return nil
	}
	ids := make([]string, len(res.Messages))
	for i, m := range res.Messages {
		ids[i] = m.ID
	}
	return cl.Call(c.ctx, "seen", localapi.SeenReq{Agent: me, IDs: ids}, nil)
}

func history(c *cmd, args []string) error {
	roomName := c.fs.String("room", "", "room label or id (default: local)")
	n := c.fs.Int("n", 50, "how many messages")
	if err := c.fs.Parse(args); err != nil {
		return errUsage
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	var res localapi.Messages
	if err := cl.Call(c.ctx, "history", localapi.HistoryReq{Room: *roomName, Limit: *n}, &res); err != nil {
		return err
	}
	return c.out(res.Messages, func() {
		for _, m := range res.Messages {
			printMsg(os.Stdout, m)
		}
	})
}
