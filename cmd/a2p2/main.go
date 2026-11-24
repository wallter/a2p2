// Command a2p2 is the whole product: daemon, CLI, MCP shim and harness hook
// in one binary. See docs/DESIGN.md. The commands are wiring over the local
// API (PROTOCOL-next section 7); --json prints a response unchanged.
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
	"github.com/wallter/a2p2/internal/store"
)

var version = "dev" // set by -ldflags at release

const usage = `a2p2 - peer-to-peer messaging for AI agents and the people who run them

Getting started
  a2p2 install claude|codex|agy   connect a harness (undo with: a2p2 uninstall ...)
  a2p2 ui                         open the local web page
  a2p2 doctor                     check that everything is wired up

Talking (as the human, or --as <label> for a script)
  a2p2 send [--room R] [--to AGENT]... [--reply-to ID] [--ask-handled] [--replaces ID] TEXT...
                                  (--replaces ID with empty text withdraws the message)
  a2p2 inbox [--room R] [--wait SECONDS] [--limit N] [--peek] [--thread ID]
  a2p2 history [--room R] [-n 50] [--before ID]
  a2p2 ack ID                     mark a message handled
  a2p2 trace ID                   where a message got to: copies and handling reports

Rooms across machines
  a2p2 net on [ADDR]              accept peer connections (default 0.0.0.0:7423)
  a2p2 net off                    this machine only
  a2p2 room create LABEL          make a room with this machine as its admin
  a2p2 room invite ROOM [--history] [--member|--admin]
                                  print a one-time invite (valid 30 minutes)
  a2p2 join [TOKEN] [--no-wait]   preview an invite, approve it here, then wait for the admin
  a2p2 room list [ROOM]           members, admins, changes waiting, limits
  a2p2 room admin ROOM MACHINE    ask to make a member an admin (confirm to sign)
  a2p2 room remove ROOM MACHINE   ask to remove a member (confirm to sign)
  a2p2 room leave ROOM            stop carrying a room on this machine

A person's decisions (at a terminal only)
  a2p2 confirm                    list requests waiting for you
  a2p2 confirm ID [--decline]     approve (or decline) one request
  a2p2 alerts [--wait SECONDS]    what waits for a person; --wait returns when something does

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

// terminal reports whether stdin is a terminal: a person's decisions are
// taken only there (P3), never through a pipe an agent could feed.
func terminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

var stdin = bufio.NewReader(os.Stdin)

func ask(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	line, _ := stdin.ReadString('\n')
	return strings.TrimSpace(line)
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
	if err := c.fs.Parse(permute(args)); err != nil {
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
		return c.call("ack", localapi.AckReq{Agent: me, ID: rest[0]}, &r, func() { fmt.Println("handled, report", r.Handled) })
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
		head += " (reply to " + short(e.Parent)
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

func status(c *cmd) error {
	var st localapi.Status
	return c.call("status", nil, &st, func() {
		if st.Notice != "" {
			fmt.Println("NOTICE   " + st.Notice)
		}
		fmt.Printf("machine  %s (%s)  a2p2 %s  pid %d\n", st.Name, short(st.Node), st.Version, st.PID)
		if len(st.Listen) == 0 {
			fmt.Println("network  off (this machine only; enable with: a2p2 net on)")
		} else {
			fmt.Printf("network  listening, reachable at %s\n", strings.Join(st.Listen, ", "))
		}
		fmt.Printf("ui       %s\n", st.UI)
		fmt.Println("rooms")
		for _, r := range st.Rooms {
			fmt.Printf("  %-20s %s  %d machine(s), %d admin(s)\n", r.Label, r.ID, len(r.Members), len(r.Admins))
		}
		fmt.Println("agents on this machine")
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
	return c.call("trace", localapi.IDReq{ID: id}, &t, func() {
		fmt.Printf("message %s in room %s: saved here, text %s\n", t.ID, t.Room, t.Reason)
		if len(t.Copies) == 0 {
			fmt.Println("copies: none reported yet by a machine this one has been in touch with")
		}
		for _, cp := range t.Copies {
			member := "member"
			if !cp.Member {
				member = "no longer a member"
			}
			fmt.Printf("  copy on %s (%s), reported %s\n", short(cp.Node), member, clock(cp.At))
		}
		if len(t.Handled) == 0 {
			fmt.Println("handled: no report yet")
		}
		for _, h := range t.Handled {
			fmt.Printf("  handled by %s at %s\n", h.Agent, clock(h.At))
		}
	})
}

// join previews an invite, asks the person at this terminal, accepts, and
// waits for the admin machine's person unless --no-wait.
func join(c *cmd, args []string) error {
	noWait := c.fs.Bool("no-wait", false, "do not wait for the admin machine's person")
	if err := c.fs.Parse(permute(args)); err != nil || c.fs.NArg() > 1 {
		return usagef("a2p2 join [TOKEN] [--no-wait]")
	}
	if !terminal() {
		return errors.New("join asks for your approval at a terminal; run it in one (stdin is not a terminal)")
	}
	token := c.fs.Arg(0)
	if token == "" || token == "-" {
		token = ask("Paste the invite: ")
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
	if ask("Join on these terms? Type yes: ") != "yes" {
		return errors.New("not joined")
	}
	req := localapi.AcceptReq{Token: token, Preview: pv.Preview}
	if !*noWait {
		req.WaitMS = 60000
		fmt.Fprintln(os.Stderr, "Waiting for the admin machine's person to approve (a2p2 confirm there)...")
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
				fmt.Println("accepted; waiting for the admin machine's person. The daemon keeps following it.")
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
	if err := c.fs.Parse(permute(args)); err != nil {
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
			for _, r := range res.Rooms {
				fmt.Printf("%s  %s\n", r.Label, r.ID)
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
					fmt.Printf("  fork by %s at %d (open %v)\n", short(f.Author), f.Seq, f.Open)
				}
				for _, l := range r.Limits {
					fmt.Printf("  limit: %s\n", l)
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
				"They run: a2p2 join   (and paste it); then approve here with: a2p2 confirm\n", clock(inv.Expires))
		})
	case (rest[0] == "admin" || rest[0] == "remove") && len(rest) == 3:
		var r localapi.ChangeResp
		return c.call("room/"+rest[0], localapi.MemberReq{Room: rest[1], Node: rest[2]}, &r, func() {
			fmt.Printf("request %s: %s\nApprove it with: a2p2 confirm %s\n", r.Request, r.Text, r.Request)
		})
	case rest[0] == "leave" && len(rest) == 2:
		return c.call("room/leave", localapi.RoomReq{Room: rest[1]}, &localapi.Empty{}, func() {
			fmt.Println("left; its data is deleted here. Other members list this machine until an admin removes it.")
		})
	}
	return usagef("a2p2 room create LABEL | invite ROOM [--history] [--member|--admin] | admin ROOM MACHINE | remove ROOM MACHINE | leave ROOM | list [ROOM]")
}

// confirm lists requests, or decides one; deciding needs a terminal (P3).
func confirm(c *cmd, args []string) error {
	decline := c.fs.Bool("decline", false, "decline instead of approving")
	if err := c.fs.Parse(permute(args)); err != nil || c.fs.NArg() > 1 {
		return usagef("a2p2 confirm [ID] [--decline]")
	}
	if !terminal() {
		return errors.New("confirm takes a person's decision at a terminal only (stdin is not a terminal)")
	}
	var qs localapi.RequestsResp
	if c.fs.NArg() == 0 {
		return c.call("requests", localapi.Empty{}, &qs, func() {
			if len(qs.Requests) == 0 {
				fmt.Println("nothing waits for you")
			}
			for _, q := range qs.Requests {
				fmt.Printf("%s  room %s: %s\n", q.ID, q.Room, q.Text)
			}
		})
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
	fmt.Fprintf(os.Stderr, "Room %s: %s\n", q.Room, q.Text)
	if ask(verb+" this? Type yes: ") != "yes" {
		return errors.New("nothing decided")
	}
	var r localapi.ConfirmResp
	return c.call("confirm", localapi.ConfirmReq{ID: id, Approve: !*decline}, &r, func() {
		fmt.Printf("%s %s", r.ID, r.State)
		if r.Effect != "" {
			fmt.Printf(" (%s)", r.Effect)
		}
		if r.Reason != "" {
			fmt.Printf(": %s", r.Reason)
		}
		fmt.Println()
	})
}

func alerts(c *cmd, args []string) error {
	wait := c.fs.Int("wait", 0, "wait up to this many seconds (at most 120) for something")
	if err := c.fs.Parse(permute(args)); err != nil {
		return errUsage
	}
	var res localapi.AlertsResp
	return c.call("alerts", localapi.WaitReq{WaitMS: int64(*wait) * 1000}, &res, func() {
		if len(res.Alerts) == 0 {
			fmt.Println("nothing waits for a person")
		}
		for _, a := range res.Alerts {
			fmt.Printf("%-8s %s  room %s: %s\n", a.Kind, a.ID, a.Room, a.Text)
		}
	})
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
		if err := writeJSON(checks); err != nil {
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
	c.fs.Var(&to, "to", "addressee agent id, or everyone (repeatable; default: everyone in the room)")
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
	if err := c.fs.Parse(args); err != nil {
		return errUsage
	}
	var res localapi.Entries
	return c.call("history", localapi.HistoryReq{Room: *roomName, Limit: *n, Before: *before}, &res, func() {
		for _, e := range res.Entries {
			printEntry(os.Stdout, e)
		}
	})
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
