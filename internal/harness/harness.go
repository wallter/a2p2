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
	"strings"
)

// Names lists supported harnesses.
var Names = []string{"claude", "codex", "agy"}

// Options control install/uninstall. Paths default to the real config
// locations; tests override HomeDir, Run and LookPath.
type Options struct {
	HomeDir  string                                  // user's home directory ("" = os.UserHomeDir)
	Binary   string                                  // absolute path of the a2p2 binary to register
	OnStop   bool                                    // also install the end-of-turn hook
	DryRun   bool                                    // describe changes without writing or running anything
	Run      func(name string, args ...string) error // runs a harness CLI (nil = exec)
	LookPath func(file string) (string, error)       // nil = exec.LookPath
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
	home, err := o.home()
	if err != nil {
		return nil, err
	}
	h := &hookFile{path: filepath.Join(append([]string{home}, sp.file...)...), spec: sp, doc: map[string]json.RawMessage{}}
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

// write saves atomically, backing up the original once. A symlinked hook
// file is resolved so the link survives; existing permission bits are kept.
func (h *hookFile) write() error {
	b, err := h.encode()
	if err != nil {
		return err
	}
	path := h.path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		path = r
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if bak := path + ".a2p2.bak"; h.existed {
		if _, err := os.Stat(bak); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(bak, h.orig, 0o600); err != nil {
				return err
			}
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".a2p2-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
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
	h, err := loadHooks(name, o) // validate early; re-read below
	if err != nil {
		return "", err
	}
	cli := h.spec.cli
	if _, err := o.lookPath(cli); err != nil {
		return "", fmt.Errorf("%s is not on PATH; install it first, then run: a2p2 install %s", cli, name)
	}
	p := prefix(o)
	rep := []string{fmt.Sprintf("%sregister MCP server: %s mcp add a2p2 (%s)", p, cli, o.Binary)}
	if !o.DryRun {
		_ = o.run(cli, "mcp", "remove", "a2p2") // idempotent: ignore failure
		if err := o.run(cli, mcpAdd(name, o)...); err != nil {
			return strings.Join(rep, "\n"), fmt.Errorf("%s mcp add failed: %w", cli, err)
		}
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
			if err := h.write(); err != nil {
				return strings.Join(rep, "\n"), err
			}
		}
		rep = append(rep, fmt.Sprintf("%supdate hook(s) %s in %s", p, strings.Join(changed, ", "), h.path))
		if h.existed {
			rep = append(rep, "original saved at "+h.path+".a2p2.bak (first change only)")
		}
	}
	if name == "codex" {
		rep = append(rep, "Codex requires trusting new hooks: open the Codex TUI and approve them with /hooks")
	}
	return strings.Join(rep, "\n"), nil
}

// Uninstall removes exactly what Install added.
func Uninstall(name string, o Options) (string, error) {
	h, err := loadHooks(name, o) // validate before touching anything
	if err != nil {
		return "", err
	}
	cli := h.spec.cli
	p := prefix(o)
	var mcp []string
	if _, err := o.lookPath(cli); err != nil {
		mcp = append(mcp, cli+" is not on PATH; skipped mcp remove")
	} else {
		mcp = append(mcp, p+"unregister MCP server: "+cli+" mcp remove a2p2")
		if !o.DryRun {
			if err := o.run(cli, "mcp", "remove", "a2p2"); err != nil {
				mcp = append(mcp, "mcp remove reported: "+err.Error())
			}
		}
	}
	if h, err = loadHooks(name, o); err != nil {
		return "", err
	}
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
			if err := h.write(); err != nil {
				return "", err
			}
		}
		rep = append(rep, fmt.Sprintf("%sremove hook(s) %s from %s", p, strings.Join(changed, ", "), h.path))
	}
	return strings.Join(append(rep, mcp...), "\n"), nil
}

// Check is one doctor finding.
type Check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Info string `json:"info"`
	Fix  string `json:"fix,omitempty"`
}

// Doctor reports whether each installed harness is wired correctly. It never
// runs a model.
func Doctor(o Options) []Check {
	var out []Check
	for _, name := range Names {
		if _, err := o.lookPath(specs[name].cli); err != nil {
			continue
		}
		c := Check{Name: name + " hook", Fix: "a2p2 install " + name}
		h, err := loadHooks(name, o)
		if err != nil {
			c.Info = err.Error()
			out = append(out, c)
			continue
		}
		arr, err := h.array(h.spec.first)
		if err != nil {
			c.Info = err.Error()
			out = append(out, c)
			continue
		}
		var found string
		for _, e := range arr {
			for _, cmd := range commands(e) {
				if isOurs(name, cmd) {
					found = cmd
				}
			}
		}
		want := hookCmd(name, h.spec.first, o)
		switch {
		case found == "":
			// Not connecting a harness is a choice, not a fault.
			c.OK, c.Info, c.Fix = true, "not connected (to connect: a2p2 install "+name+")", ""
		case found != want:
			c.Info = "hook in " + h.path + " points at a different a2p2 binary: " + found
		default:
			c.OK, c.Info, c.Fix = true, "unread-notice hook installed in "+h.path, ""
		}
		out = append(out, c)
	}
	return out
}
