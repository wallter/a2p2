package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Where each harness's own CLI keeps the MCP registration (user scope), as
// observed with Claude Code 2.1.331, codex-cli 0.171.0 and agy 1.5.16 in a
// throwaway home. Doctor only reads them; registering stays the CLIs' job.
var mcpFiles = map[string][]string{
	"claude": {".claude.json"},
	"codex":  {".codex", "config.toml"},
	"agy":    {".gemini", "config", "mcp_config.json"},
}

// codexTable is the TOML table codex mcp add writes for the server.
const codexTable = "[mcp_servers.a2p2]"

// codexApproval is the one line Install adds to that table, and only when
// the person passes --approve-tools: it lets every tool of this server run
// without a prompt, send, reply, ack and ask_human included, and any tool
// a later a2p2 adds. Without it Codex applies its own approval rules, and
// a headless `codex exec` refuses a2p2's tool calls. The trailing comment
// marks the line as a2p2's, so Uninstall removes exactly it.
//
// codexApprovalUnasked is the same line as a2p2 0.1.0 to 0.2.3 added on
// every install, without asking. Install and Uninstall remove it, and
// doctor reports it as a fault.
const (
	codexApproval        = `default_tools_approval_mode = "approve" # added by a2p2 install --approve-tools`
	codexApprovalUnasked = `default_tools_approval_mode = "approve" # added by a2p2 install`
)

// What a line of a TOML file is, for tomlLines.
const (
	tomlPlain  = iota // a key, a comment or nothing, at the top level
	tomlHeader        // a table header
	tomlInside        // starts inside a multi-line string or array
)

// tomlLines says, for each line of a TOML file, what it is. A line inside
// a multi-line string or array that reads like a header or a key is text,
// and a2p2 must neither write into it nor split the file there.
func tomlLines(lines []string) []int {
	out := make([]int, len(lines))
	str := ""  // `"""` or `'''` while inside a multi-line string
	depth := 0 // open brackets of an array that spans lines
	for i, l := range lines {
		switch {
		case str != "" || depth > 0:
			out[i] = tomlInside
		case strings.HasPrefix(strings.TrimSpace(l), "["):
			out[i] = tomlHeader
			continue
		}
		for j := 0; j < len(l); j++ {
			if str != "" {
				switch {
				case strings.HasPrefix(l[j:], str):
					// The string ends at the last three of a run of three
					// to five quotes: up to two belong to its text.
					n := 3
					for n < 5 && j+n < len(l) && l[j+n] == str[0] {
						n++
					}
					j += n - 1
					str = ""
				case str == `"""` && l[j] == '\\':
					j++
				}
				continue
			}
			switch c := l[j]; {
			case strings.HasPrefix(l[j:], `"""`) || strings.HasPrefix(l[j:], `'''`):
				str = l[j : j+3]
				j += 2
			case c == '"':
				for j++; j < len(l) && l[j] != '"'; j++ {
					if l[j] == '\\' {
						j++
					}
				}
			case c == '\'':
				for j++; j < len(l) && l[j] != '\''; j++ {
				}
			case c == '#':
				j = len(l)
			case c == '[':
				depth++
			case c == ']' && depth > 0:
				depth--
			}
		}
	}
	return out
}

// isCodexApproval reports whether l is an approval line a2p2 wrote.
func isCodexApproval(l string) bool {
	t := strings.TrimSpace(l)
	return t == codexApproval || t == codexApprovalUnasked
}

// tomlHeaderName returns a table header line as "[a.b]": without its
// comment, without the spaces TOML allows around the brackets and dots,
// and with a quoted key that needs no quotes unquoted, so that
// `[ mcp_servers."a2p2" ] # x` and `[mcp_servers.a2p2]` are the same table.
// An array of tables gives "[[", which names no table a2p2 looks for.
func tomlHeaderName(l string) string {
	t := strings.TrimSpace(l)
	if !strings.HasPrefix(t, "[") || strings.HasPrefix(t, "[[") {
		return "[["
	}
	var parts []string
	start, q := 1, byte(0)
	for i := 1; i < len(t); i++ {
		c := t[i]
		if q != 0 {
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			q = c
		case '.', ']':
			p := strings.TrimSpace(t[start:i])
			if len(p) >= 2 && (p[0] == '"' || p[0] == '\'') && p[len(p)-1] == p[0] && bareKey(p[1:len(p)-1]) {
				p = p[1 : len(p)-1]
			}
			parts = append(parts, p)
			start = i + 1
			if c == ']' {
				return "[" + strings.Join(parts, ".") + "]"
			}
		}
	}
	return t
}

// bareKey reports whether k is a TOML key that needs no quotes.
func bareKey(k string) bool {
	for _, r := range k {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return k != ""
}

// tomlKey returns the key of a plain line, unquoted, or "" if it has none.
func tomlKey(l string) string {
	k, _, ok := strings.Cut(strings.TrimSpace(l), "=")
	if !ok {
		return ""
	}
	k = strings.TrimSpace(k)
	if len(k) >= 2 && (k[0] == '"' || k[0] == '\'') && k[len(k)-1] == k[0] && bareKey(k[1:len(k)-1]) {
		k = k[1 : len(k)-1]
	}
	return k
}

// configFile is the path of a harness file given relative to the home.
// Codex keeps its files in $CODEX_HOME when that is set (default ~/.codex),
// and its CLI writes config.toml there, so a2p2 reads and edits them there
// too. Tests set HomeDir and get the default.
func configFile(o Options, rel []string) (string, error) {
	if len(rel) > 0 && rel[0] == ".codex" {
		dir, err := envOr(o, "CODEX_HOME", ".codex")
		return filepath.Join(append([]string{dir}, rel[1:]...)...), err
	}
	home, err := o.home()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, rel...)...), nil
}

func codexConfig(o Options) (string, error) {
	return configFile(o, mcpFiles["codex"])
}

// codexApprovalEdit returns config.toml text s with the approval lines
// a2p2 wrote removed from a2p2's tables and, when approve is set,
// codexApproval added under [mcp_servers.a2p2]. table reports whether that
// table is there. theirs reports whether it already has a
// default_tools_approval_mode that a2p2 did not write; then none is added,
// since a second one is a duplicate key and Codex would not read the file.
// Lines inside multi-line strings and arrays, and lines of other tables,
// are left as they are.
func codexApprovalEdit(s string, approve bool) (out string, removed, table, theirs bool) {
	lines := strings.Split(s, "\n")
	kind := tomlLines(lines)
	keep := make([]string, 0, len(lines)+1)
	cur, at := "", -1
	for i, l := range lines {
		if kind[i] == tomlHeader {
			cur = tomlHeaderName(l)
			if cur == codexTable && at < 0 {
				table, at = true, len(keep)
			}
		}
		ours := cur == codexTable || strings.HasPrefix(cur, "[mcp_servers.a2p2.")
		switch {
		case kind[i] != tomlPlain || !ours:
		case isCodexApproval(l):
			removed = true
			continue
		case cur == codexTable && tomlKey(l) == "default_tools_approval_mode":
			theirs = true
		}
		keep = append(keep, l)
	}
	if approve && table && !theirs {
		keep = append(keep[:at+1], append([]string{codexApproval}, keep[at+1:]...)...)
	}
	return strings.Join(keep, "\n"), removed, table, theirs
}

// editCodexApproval applies codexApprovalEdit to Codex's config.toml in one
// guarded write (see editFile), or only reads it on a dry run.
func editCodexApproval(o Options, approve bool) (removed, table, theirs bool, err error) {
	path, err := codexConfig(o)
	if err != nil {
		return false, false, false, err
	}
	err = editFile(path, o.DryRun, func(s string) string {
		s, removed, table, theirs = codexApprovalEdit(s, approve)
		return s
	})
	return removed, table, theirs, err
}

// editFile replaces the file at path with edit of its text, unless the text
// is unchanged, the file is missing or dry is set. It writes only while the
// file still holds what edit read, and otherwise reads it again, so an edit
// another program made in the meantime is kept. The moment between that
// last check and the rename remains.
func editFile(path string, dry bool, edit func(string) string) error {
	for range 3 {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			edit("")
			return nil
		}
		if err != nil {
			return err
		}
		out := edit(string(b))
		if dry || out == string(b) {
			return nil
		}
		err = writeChecked(path, []byte(out), func(cur []byte) bool { return bytes.Equal(cur, b) })
		if !errors.Is(err, errChanged) {
			return err
		}
	}
	return fmt.Errorf("%s kept changing while a2p2 edited it; close what is editing it, then run again", path)
}

// setCodexApproval makes the approval lines a2p2 wrote match the person's
// choice, and returns lines for the report. Without --approve-tools it
// removes every line a2p2 wrote, the one earlier versions added without
// asking included, and adds none. Either way it is one write.
func setCodexApproval(o Options) ([]string, error) {
	path, err := codexConfig(o)
	if err != nil {
		return nil, err
	}
	const all = "every a2p2 tool, send, reply, ack and ask_human included, and any tool a later a2p2 adds,"
	removed, table, theirs, err := editCodexApproval(o, o.ApproveTools)
	if !o.ApproveTools {
		rep := []string{"tool approval: a2p2 approves none; Codex's own rules decide which a2p2 tools ask first " +
			"(to let " + all + " run without a prompt: a2p2 install codex --approve-tools)"}
		switch {
		case removed && o.DryRun:
			rep = append(rep, "would remove the tool approval an earlier a2p2 install added to "+path)
		case removed:
			rep = append(rep, "removed the tool approval an earlier a2p2 install added to "+path)
		}
		return rep, err
	}
	what := "let " + all + " run without a prompt, for this server only, in " + path + " (--approve-tools; uninstall removes it)"
	switch {
	case err != nil:
		return nil, err
	case theirs:
		return []string{"tool approval: " + path + " already sets default_tools_approval_mode for a2p2's server, and a2p2 did not write it; " +
			"a2p2 added nothing, since a second one would stop Codex reading the file"}, nil
	case o.DryRun:
		return []string{"tool approval: would " + what}, nil
	case table:
		return []string{"tool approval: " + what}, nil
	default:
		return []string{"could not find " + codexTable + " in " + path + "; nothing was approved. Approve a2p2's tools in Codex yourself if you want them to run without a prompt"}, nil
	}
}

// codexApprovals reads what Codex is told to approve for a2p2's server
// without a prompt: the server-wide default_tools_approval_mode (mode,
// "" when unset, and by whom: "a2p2 --approve-tools", "a2p2 unasked" or
// "you"), and the tools approved one by one, under [mcp_servers.a2p2.tools.NAME]
// or as tools.NAME.approval_mode in a2p2's table. found is false when the
// table is not there.
func codexApprovals(o Options) (found bool, mode, by string, tools []string, err error) {
	path, err := codexConfig(o)
	if err != nil {
		return false, "", "", nil, err
	}
	b, ok, err := readMaybe(path)
	if err != nil || !ok {
		return false, "", "", nil, err
	}
	value := func(v string) string {
		v = strings.TrimSpace(v)
		if q := v[:min(1, len(v))]; q == `"` || q == `'` {
			if j := strings.Index(v[1:], q); j >= 0 {
				return v[1 : 1+j]
			}
		}
		v, _, _ = strings.Cut(v, "#")
		return strings.TrimSpace(v)
	}
	table := ""
	lines := strings.Split(string(b), "\n")
	kind := tomlLines(lines)
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if kind[i] == tomlInside {
			continue
		}
		if kind[i] == tomlHeader {
			table = tomlHeaderName(l)
			if table == codexTable {
				found = true
			}
			continue
		}
		_, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		k := tomlKey(l)
		switch {
		case table == codexTable && k == "default_tools_approval_mode":
			mode, by = value(v), "you"
			switch t {
			case codexApproval:
				by = "a2p2 --approve-tools"
			case codexApprovalUnasked:
				by = "a2p2 unasked"
			}
		case table == codexTable && strings.HasPrefix(k, "tools.") && strings.HasSuffix(k, ".approval_mode"):
			if value(v) == "approve" {
				tools = append(tools, strings.Trim(strings.TrimSuffix(strings.TrimPrefix(k, "tools."), ".approval_mode"), `"'`))
			}
		case strings.HasPrefix(table, "[mcp_servers.a2p2.tools.") && k == "approval_mode":
			if value(v) == "approve" {
				tools = append(tools, strings.Trim(strings.TrimSuffix(strings.TrimPrefix(table, "[mcp_servers.a2p2.tools."), "]"), `"'`))
			}
		}
	}
	return found, mode, by, tools, nil
}

// codexApprovalCheck is doctor's line on what Codex runs without a prompt.
func codexApprovalCheck(o Options) Check {
	c := Check{Name: "codex tool approval"}
	path, _ := codexConfig(o)
	found, mode, by, tools, err := codexApprovals(o)
	switch {
	case err != nil:
		c.Info = "cannot read " + path + ": " + err.Error()
		c.Fix = "a2p2 install codex"
	case !found:
		c.OK, c.Info = true, "no [mcp_servers.a2p2] table in "+path+"; nothing is approved there"
	case mode == "approve" && by == "a2p2 unasked":
		c.Info = "every a2p2 tool, send included, runs without a prompt: an earlier a2p2 install approved them in " + path + " without asking"
		c.Fix = "a2p2 install codex (or a2p2 install codex --approve-tools to keep the approval)"
	case mode == "approve" && by == "a2p2 --approve-tools":
		c.OK, c.Info = true, "every a2p2 tool, send included, runs without a prompt: you chose a2p2 install codex --approve-tools ("+path+")"
	case mode == "approve":
		c.OK, c.Info = true, "every a2p2 tool, send included, runs without a prompt: default_tools_approval_mode = \"approve\" in "+path+", not written by a2p2"
	default:
		c.OK = true
		if mode != "" {
			c.Info = "default_tools_approval_mode = \"" + mode + "\" in " + path + ": Codex's own rules decide which a2p2 tools ask first"
		} else {
			c.Info = "a2p2 approved no tools: Codex's own rules decide which a2p2 tools ask first (to approve them all, send included: a2p2 install codex --approve-tools)"
		}
	}
	if len(tools) > 0 {
		c.Info += "; approved one by one in " + path + ": " + strings.Join(tools, ", ")
	}
	return c
}

// revokeCodexApproval removes every approval line Install added to a2p2's
// tables, outside multi-line strings and arrays, and nothing else. It
// reports whether there was one (on a dry run, whether there would be).
func revokeCodexApproval(o Options) (bool, error) {
	removed, _, _, err := editCodexApproval(o, false)
	return removed, err
}

// errChanged is writeChecked's refusal when the file changed under it.
var errChanged = errors.New("the file changed while a2p2 edited it")

// beforeCheck runs just before writeChecked compares the file; tests use
// it to edit the file at that moment.
var beforeCheck = func(path string) {}

// writeKeepingMode replaces path atomically, keeping its permission bits.
// A symlink is followed (see resolve); a dangling one is refused.
func writeKeepingMode(path string, b []byte) error {
	return writeChecked(path, b, nil)
}

// writeChecked is writeKeepingMode that, when unchanged is set, renames
// the new file into place only if unchanged accepts what the file holds
// just before, and returns errChanged otherwise.
func writeChecked(path string, b []byte, unchanged func(cur []byte) bool) error {
	path, err := resolve(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
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
	// Windows has no permission bits to keep (File.Chmod fails there); the
	// new file takes its directory's access control list.
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(mode); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if unchanged != nil {
		beforeCheck(path)
		if cur, err := os.ReadFile(path); err != nil || !unchanged(cur) {
			return errChanged
		}
	}
	return os.Rename(tmp.Name(), path)
}

// registered reports whether the harness's own config registers the a2p2
// MCP server, and the command it runs ("" when unknown).
func registered(name string, o Options) (found bool, command string, err error) {
	path, err := configFile(o, mcpFiles[name])
	if err != nil {
		return false, "", err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if name == "codex" {
		lines := strings.Split(string(b), "\n")
		kind := tomlLines(lines)
		for i, l := range lines {
			if kind[i] != tomlHeader || tomlHeaderName(l) != codexTable {
				continue
			}
			for j, x := range lines[i+1:] {
				x = strings.TrimSpace(x)
				if kind[i+1+j] == tomlHeader {
					break
				}
				if kind[i+1+j] == tomlInside {
					continue
				}
				if _, v, ok := strings.Cut(x, "="); ok && tomlKey(x) == "command" {
					var s string
					if json.Unmarshal([]byte(strings.TrimSpace(v)), &s) == nil {
						command = s
					}
				}
			}
			return true, command, nil
		}
		return false, "", nil
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return false, "", err
	}
	s, ok := doc.MCPServers["a2p2"]
	return ok, s.Command, nil
}
