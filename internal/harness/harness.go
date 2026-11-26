// Package harness knows how to plug a2p2 into each agent harness: registering
// the MCP server, installing the unread-notice hook, and undoing both.
//
// MCP registration goes through each harness's own CLI (their config formats
// are theirs to own). The hook JSON files are edited here, preserving every
// key we do not own.
package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
)

// Names lists supported harnesses: first those registered through their own
// CLI with an unread-notice hook (hookNames), then those whose one
// configuration file a2p2 edits itself (adapters, in config.go).
var Names = append(append([]string{}, hookNames...), "opencode", "hermes", "pi")

var hookNames = []string{"claude", "codex", "agy"}

// Options control install/uninstall. Paths default to the real config
// locations; tests override HomeDir, Run and LookPath.
type Options struct {
	HomeDir      string                                  // user's home directory ("" = os.UserHomeDir)
	Binary       string                                  // absolute path of the a2p2 binary to register
	OnStop       bool                                    // also install the end-of-turn hook
	DryRun       bool                                    // describe changes without writing or running anything
	ApproveTools bool                                    // Codex: let every a2p2 tool run without a prompt; only when the person asks
	Run          func(name string, args ...string) error // runs a harness CLI (nil = exec)
	LookPath     func(file string) (string, error)       // nil = exec.LookPath
	StateDir     string                                  // where install records live ("" = $A2P2_HOME/installs or ~/.a2p2/installs)
	OS           string                                  // the system to install for ("" = runtime.GOOS; tests set "windows")
}

type spec struct {
	cli     string
	file    []string // path under home
	wrapped bool     // events live under a top-level "hooks" key
	nested  bool     // entry is {"hooks":[{command}]} rather than the command itself
	first   string   // turn-start event
}

var specs = map[string]spec{
	"claude": {"claude", []string{".claude", "settings.json"}, true, true, "UserPromptSubmit"},
	"codex":  {"codex", []string{".codex", "hooks.json"}, true, true, "UserPromptSubmit"},
	// agy: top-level event names. The PreInvocation event name is UNVERIFIED.
	"agy": {"agy", []string{".gemini", "config", "hooks.json"}, false, false, "PreInvocation"},
}

func (o Options) lookPath(f string) (string, error) {
	if o.LookPath != nil {
		return o.LookPath(f)
	}
	return exec.LookPath(f)
}

func (o Options) run(name string, args ...string) error {
	if o.Run != nil {
		return o.Run(name, args...)
	}
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (o Options) home() (string, error) {
	if o.HomeDir != "" {
		return o.HomeDir, nil
	}
	return os.UserHomeDir()
}

// quote makes s safe as one shell word: bare if it only has safe characters,
// otherwise single-quoted with embedded quotes written as '\”.
func quote(s string) string {
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@%=,", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}

func marker(name string) string { return " hook --harness " + name + " " }

func hookCmd(name, event string, o Options) string {
	return quote(o.Binary) + marker(name) + "--event " + event
}

// firstWord splits a leading shell word (bare, single-quoted, backslash
// escaped, or the double-quoted form older installs wrote) from cmd.
func firstWord(cmd string) (word, rest string, ok bool) {
	var b strings.Builder
	i := 0
	for i < len(cmd) && cmd[i] != ' ' && cmd[i] != '\t' {
		switch c := cmd[i]; c {
		case '\'', '"':
			j := strings.IndexByte(cmd[i+1:], c)
			if j < 0 {
				return "", "", false
			}
			b.WriteString(cmd[i+1 : i+1+j])
			i += j + 2
		case '\\':
			if i+1 >= len(cmd) {
				return "", "", false
			}
			b.WriteByte(cmd[i+1])
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), cmd[i:], i > 0
}

// commands lists the command strings inside one event entry.
func commands(entry json.RawMessage) []string {
	var e struct {
		Command string `json:"command"`
		Hooks   []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if json.Unmarshal(entry, &e) != nil {
		return nil
	}
	cs := []string{}
	if e.Command != "" {
		cs = append(cs, e.Command)
	}
	for _, h := range e.Hooks {
		cs = append(cs, h.Command)
	}
	return cs
}

// isOurs reports whether cmd is exactly "<path to a2p2> hook --harness <name>
// --event <Event>" for some binary path; anything else is the user's.
func isOurs(name, cmd string) bool {
	path, rest, ok := firstWord(cmd)
	if !ok || strings.TrimSuffix(filepath.Base(path), ".exe") != "a2p2" {
		return false
	}
	for _, ev := range hookEvents(specs[name]) {
		if rest == marker(name)+"--event "+ev {
			return true
		}
	}
	return false
}

type events = map[string]json.RawMessage

// hookFile is a parsed hook JSON file.
type hookFile struct {
	path    string
	spec    spec
	existed bool
	orig    []byte
	doc     map[string]json.RawMessage
	evs     events
}

func loadHooks(name string, o Options) (*hookFile, error) {
	sp, ok := specs[name]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q (want one of %s)", name, strings.Join(Names, ", "))
	}
	path, err := configFile(o, sp.file)
	if err != nil {
		return nil, err
	}
	h := &hookFile{path: path, spec: sp, doc: map[string]json.RawMessage{}}
	h.orig, err = os.ReadFile(h.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		h.existed = true
		if len(bytes.TrimSpace(h.orig)) > 0 {
			if err := json.Unmarshal(h.orig, &h.doc); err != nil || h.doc == nil {
				return nil, fmt.Errorf("%s is not a valid JSON object (%v); fix or move it, a2p2 will not touch it", h.path, err)
			}
		}
	}
	h.evs = h.doc
	if sp.wrapped {
		h.evs = events{}
		if raw, ok := h.doc["hooks"]; ok {
			if err := json.Unmarshal(raw, &h.evs); err != nil || h.evs == nil {
				return nil, fmt.Errorf("%s: \"hooks\" is not an object; a2p2 will not touch it", h.path)
			}
		}
	}
	return h, nil
}

func (h *hookFile) array(event string) ([]json.RawMessage, error) {
	raw, ok := h.evs[event]
	if !ok {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("%s: hooks.%s is not an array; a2p2 will not touch it", h.path, event)
	}
	return arr, nil
}

// set makes the event contain exactly one current a2p2 entry (want) or none.
// It reports whether anything changed.
func (h *hookFile) set(name, event string, want bool, o Options) (bool, error) {
	arr, err := h.array(event)
	if err != nil {
		return false, err
	}
	cmd := hookCmd(name, event, o)
	var keep []json.RawMessage
	changed, current := false, false
	for _, e := range arr {
		cs := commands(e)
		ours, other := 0, 0
		for _, c := range cs {
			if isOurs(name, c) {
				ours++
			} else {
				other++
			}
		}
		switch {
		case ours == 0:
			keep = append(keep, e)
		case want && !current && ours == 1 && other == 0 && cs[0] == cmd:
			current = true
			keep = append(keep, e)
		case other == 0:
			changed = true // stale or duplicate entry of ours: drop
		default: // ours mixed with foreign commands in one entry: strip only ours
			var m map[string]json.RawMessage
			var inner []json.RawMessage
			if json.Unmarshal(e, &m) != nil || json.Unmarshal(m["hooks"], &inner) != nil {
				keep = append(keep, e)
				continue
			}
			var rest []json.RawMessage
			for _, ih := range inner {
				if cs := commands(ih); len(cs) == 0 || !isOurs(name, cs[0]) {
					rest = append(rest, ih)
				}
			}
			m["hooks"], _ = json.Marshal(rest)
			ne, _ := json.Marshal(m)
			keep = append(keep, ne)
			changed = true
		}
	}
	if want && !current {
		cmdObj := map[string]any{"type": "command", "command": cmd, "timeout": 5}
		var entry any = cmdObj
		if h.spec.nested {
			entry = map[string]any{"hooks": []any{cmdObj}}
		}
		b, _ := json.Marshal(entry)
		keep = append(keep, b)
		changed = true
	}
	if !changed {
		return false, nil
	}
	if len(keep) == 0 {
		delete(h.evs, event)
	} else {
		h.evs[event], _ = json.Marshal(keep)
	}
	return true, nil
}

func (h *hookFile) encode() ([]byte, error) {
	if h.spec.wrapped {
		if len(h.evs) == 0 {
			delete(h.doc, "hooks")
		} else {
			h.doc["hooks"], _ = json.Marshal(h.evs)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(h.doc)
	return buf.Bytes(), err
}

// Before a2p2 first changes a hook file it records what was there, beside
// the file, so that Uninstall can put the home back as it was (I2):
//
//   - <file>.a2p2.bak is the file as it was before a2p2 first touched it.
//     Uninstall removes a2p2's entries, writes these exact bytes back when
//     nothing else changed since, and deletes the backup.
//   - <file>.a2p2.new says a2p2 created the file, and lists the directories
//     it created for it. Uninstall deletes the file if nothing but a2p2's
//     entries were in it, then those directories if they are empty, then
//     the marker.
const (
	bakSuffix = ".a2p2.bak"
	newSuffix = ".a2p2.new"
)

// realPath resolves a symlinked hook file so the link survives.
func (h *hookFile) realPath() string {
	if r, err := filepath.EvalSymlinks(h.path); err == nil {
		return r
	}
	return h.path
}

// write saves atomically, recording the original first (see bakSuffix).
// Existing permission bits are kept.
func (h *hookFile) write(record bool) error {
	b, err := h.encode()
	if err != nil {
		return err
	}
	return saveRecorded(h.realPath(), h.existed, h.orig, b, record)
}

func hookEvents(sp spec) []string { return []string{sp.first, "Stop"} }

func mcpAdd(name string, o Options) []string {
	switch name {
	case "claude":
		return []string{"mcp", "add", "--transport", "stdio", "--scope", "user", "a2p2", "--", o.Binary, "mcp", "--harness", name}
	default:
		return []string{"mcp", "add", "a2p2", "--", o.Binary, "mcp", "--harness", name}
	}
}

func prefix(o Options) string {
	if o.DryRun {
		return "would "
	}
	return ""
}

// Install registers a2p2 with a harness and returns a human-readable report.
// The CLI runs first; the hook file is read and rewritten as late as possible.
func Install(name string, o Options) (string, error) {
	if err := forOS(name, &o); err != nil { // windows.go
		return "", err
	}
	warn, err := checkBinary(o) // binary.go
	if err != nil {
		return "", err
	}
	var rep string
	if a, ok := adapters[name]; ok {
		rep, err = a.install(name, o)
	} else {
		rep, err = installHooked(name, o)
	}
	if warn != "" {
		rep = strings.TrimPrefix(rep+"\n"+warn, "\n")
	}
	return rep, err
}

// installHooked installs into a harness registered through its own CLI.
func installHooked(name string, o Options) (string, error) {
	h, err := loadHooks(name, o) // validate early; re-read below
	if err != nil {
		return "", err
	}
	cli := h.spec.cli
	if _, err := o.lookPath(cli); err != nil {
		return "", fmt.Errorf("%s is not on PATH; install it first, then run: a2p2 install %s", cli, name)
	}
	if _, err := resolve(h.path); err != nil {
		return "", err
	}
	if path, _ := codexConfig(o); name == "codex" {
		if _, err := resolve(path); err != nil {
			return "", err
		}
	}
	p := prefix(o)
	rep := []string{fmt.Sprintf("%sregister MCP server: %s mcp add a2p2 (%s)", p, cli, o.Binary)}
	// The first install records what it replaces, before changing anything.
	rec, err := loadRecord(name, o)
	if err != nil {
		return "", err
	}
	if rec == nil {
		file, had, prior, err := cliEntry(name, o)
		if err != nil {
			return "", err
		}
		rec = &record{Harness: name, File: file, Had: had, Prior: prior}
		if had {
			rep = append(rep, p+"replace the entry named a2p2 already in "+file+"; uninstall puts it back")
		}
		if !o.DryRun {
			if err := saveRecord(rec, o); err != nil {
				return "", err
			}
		}
	}
	if !o.DryRun {
		_ = o.run(cli, "mcp", "remove", "a2p2") // idempotent: ignore failure
		if err := o.run(cli, mcpAdd(name, o)...); err != nil {
			err = fmt.Errorf("%s mcp add failed: %w", cli, err)
			// The approval lines a2p2 wrote go even when the CLI fails and
			// keeps the table: nothing is approved for a server that is not
			// registered as asked.
			if name == "codex" {
				if removed, rerr := revokeCodexApproval(o); rerr != nil {
					err = errors.Join(err, rerr)
				} else if removed {
					path, _ := codexConfig(o)
					rep = append(rep, "removed the tool approval an earlier a2p2 install added to "+path)
				}
			}
			return strings.Join(rep, "\n"), err
		}
	}
	if name == "codex" {
		more, err := setCodexApproval(o)
		rep = append(rep, more...)
		if err != nil {
			return strings.Join(rep, "\n"), err
		}
	} else if o.ApproveTools {
		rep = append(rep, "--approve-tools has no effect for "+name+": a2p2 approves no tools there; "+name+" asks by its own rules")
	}
	if h, err = loadHooks(name, o); err != nil {
		return strings.Join(rep, "\n"), err
	}
	var changed []string
	for _, ev := range hookEvents(h.spec) {
		c, err := h.set(name, ev, ev != "Stop" || o.OnStop, o)
		if err != nil {
			return strings.Join(rep, "\n"), err
		}
		if c {
			changed = append(changed, ev)
		}
	}
	if len(changed) == 0 {
		rep = append(rep, fmt.Sprintf("hooks already up to date in %s", h.path))
	} else {
		if !o.DryRun {
			if err := h.write(true); err != nil {
				return strings.Join(rep, "\n"), err
			}
		}
		rep = append(rep, fmt.Sprintf("%supdate hook(s) %s in %s", p, strings.Join(changed, ", "), h.path))
		if h.existed {
			rep = append(rep, "the file as it was before a2p2 first changed it is kept at "+h.path+bakSuffix+
				" until uninstall restores it")
		}
	}
	if name == "codex" {
		rep = append(rep, "Codex requires trusting new hooks: open the Codex TUI and approve them with /hooks")
	}
	return strings.Join(rep, "\n"), nil
}

// Uninstall removes exactly what Install added, and the records Install
// kept to undo it, so the home is as it was (I2).
func Uninstall(name string, o Options) (string, error) {
	if a, ok := adapters[name]; ok {
		return a.uninstall(name, o)
	}
	h, err := loadHooks(name, o) // validate before touching anything
	if err != nil {
		return "", err
	}
	rec, err := loadRecord(name, o)
	if err != nil {
		return "", err
	}
	if rec == nil {
		return noRecord(name, o), nil
	}
	if _, err := resolve(h.path); err != nil {
		return "", err
	}
	// The reverse of Install: the hook file first, then the registration.
	p := prefix(o)
	var changed []string
	for _, ev := range hookEvents(h.spec) {
		c, err := h.set(name, ev, false, o)
		if err != nil {
			return "", err
		}
		if c {
			changed = append(changed, ev)
		}
	}
	var rep []string
	if len(changed) == 0 {
		rep = append(rep, "no a2p2 hooks found in "+h.path)
	} else {
		if !o.DryRun {
			if err := h.write(false); err != nil {
				return "", err
			}
		}
		rep = append(rep, fmt.Sprintf("%sremove hook(s) %s from %s", p, strings.Join(changed, ", "), h.path))
	}
	more, err := h.restore(o.DryRun)
	if err != nil {
		return "", err
	}
	rep = append(rep, more...)
	cli := h.spec.cli
	var mcp []string
	if name == "codex" {
		path, _ := codexConfig(o)
		switch ok, err := revokeCodexApproval(o); {
		case err != nil:
			return "", err
		case ok && o.DryRun:
			mcp = append(mcp, "would remove a2p2's tool approval from "+path)
		case ok:
			mcp = append(mcp, "removed a2p2's tool approval from "+path)
		}
	}
	if _, err := o.lookPath(cli); err != nil {
		mcp = append(mcp, cli+" is not on PATH; skipped mcp remove. The install record is kept: run uninstall again once "+cli+" is back")
		return strings.Join(append(rep, mcp...), "\n"), nil
	}
	mcp = append(mcp, p+"unregister MCP server: "+cli+" mcp remove a2p2")
	if !o.DryRun {
		if err := o.run(cli, "mcp", "remove", "a2p2"); err != nil {
			mcp = append(mcp, "mcp remove reported: "+err.Error())
		}
	}
	if rec.Had {
		mcp = append(mcp, p+"put back the entry named a2p2 that install replaced, in "+rec.File)
		if !o.DryRun {
			if err := putBackCLIEntry(name, rec); err != nil {
				return strings.Join(append(rep, mcp...), "\n"), err
			}
		}
	}
	if !o.DryRun {
		if err := dropRecord(name, o); err != nil {
			return strings.Join(append(rep, mcp...), "\n"), err
		}
	}
	return strings.Join(append(rep, mcp...), "\n"), nil
}

// restore finishes an uninstall of the hook file once a2p2's entries are
// gone (see bakSuffix and restoreRecorded). It returns lines for the report.
func (h *hookFile) restore(dry bool) ([]string, error) {
	cur, err := h.encode()
	if err != nil {
		return nil, err
	}
	return restoreRecorded(h.realPath(), cur, len(h.doc) == 0, same, dry)
}

func prefixed(dry bool, s string) string {
	if dry {
		return "would " + s
	}
	return s
}

// same reports whether two hook files hold the same JSON value; an empty
// file counts as an empty object.
func same(a, b []byte) bool {
	var x, y any
	parse := func(b []byte, v *any) bool {
		if len(bytes.TrimSpace(b)) == 0 {
			*v = map[string]any{}
			return true
		}
		return json.Unmarshal(b, v) == nil
	}
	return parse(a, &x) && parse(b, &y) && reflect.DeepEqual(x, y)
}

// Check is one doctor finding.
type Check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Info string `json:"info"`
	Fix  string `json:"fix,omitempty"`

	// NotConnected marks a harness that is present but not set up by a2p2,
	// which is a choice and not a fault. It is not part of the JSON output.
	NotConnected bool `json:"-"`
}

// AnyConnected reports whether any of the checks belongs to a harness that
// is connected, fully or partly. With no checks at all (no harness command
// on the PATH) it is false.
func AnyConnected(checks []Check) bool {
	for _, c := range checks {
		if !c.NotConnected {
			return true
		}
	}
	return false
}

// Doctor reports whether each installed harness is wired correctly: its
// unread-notice hook and its MCP registration, each by name, so a half
// install says which half is missing, and for Codex which a2p2 tools run
// without a prompt. It never runs a model or a harness.
func Doctor(o Options) []Check {
	var out []Check
	for _, name := range hookNames {
		if _, err := o.lookPath(specs[name].cli); err != nil {
			continue
		}
		o := o
		_ = forOS(name, &o) // the binary as install writes it (windows.go)
		hook := Check{Name: name + " hook", Fix: "a2p2 install " + name}
		reg := Check{Name: name + " MCP server", Fix: "a2p2 install " + name}
		var found string
		h, err := loadHooks(name, o)
		if err == nil {
			var arr []json.RawMessage
			if arr, err = h.array(h.spec.first); err == nil {
				for _, e := range arr {
					for _, cmd := range commands(e) {
						if isOurs(name, cmd) {
							found = cmd
						}
					}
				}
			}
		}
		isReg, command, regErr := registered(name, o)
		if err == nil && regErr == nil && found == "" && !isReg {
			// Not connecting a harness is a choice, not a fault.
			hook.OK, hook.Info, hook.Fix, hook.NotConnected = true, "not connected (to connect: a2p2 install "+name+")", "", true
			reg.OK, reg.Info, reg.Fix, reg.NotConnected = true, "not connected", "", true
			out = append(out, hook, reg)
			continue
		}
		switch want := hookCmd(name, specs[name].first, o); {
		case err != nil:
			hook.Info = err.Error()
		case found == "":
			hook.Info = "the unread-notice hook is missing from " + h.path + " although the MCP server is registered"
		case found != want:
			hook.Info = "hook in " + h.path + " points at a different a2p2 binary: " + found
		default:
			hook.OK, hook.Info, hook.Fix = true, "unread-notice hook installed in "+h.path, ""
		}
		switch {
		case regErr != nil:
			reg.Info = "cannot read the MCP registration: " + regErr.Error()
		case !isReg:
			reg.Info = "the a2p2 MCP server is not registered with " + name + " although its hook is installed"
		case command != "" && command != o.Binary:
			reg.Info = "the MCP server points at a different a2p2 binary: " + command
		default:
			reg.OK, reg.Info, reg.Fix = true, "MCP server registered with "+name, ""
		}
		out = append(out, hook, reg)
		if name == "codex" {
			out = append(out, codexApprovalCheck(o))
		}
	}
	for _, name := range Names[len(hookNames):] {
		if a := adapters[name]; a.cli != "" {
			if _, err := o.lookPath(a.cli); err == nil {
				o := o
				_ = forOS(name, &o)
				out = append(out, a.doctor(name, o))
			}
		}
	}
	if AnyConnected(out) {
		if c, ok := binaryCheck(o); ok {
			out = append(out, c)
		}
	}
	return out
}
