package harness

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
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
	tomlInside        // starts inside a multi-line string, array or inline table
)

// tomlLines says, for each line of a TOML file, what it is. A line inside
// a multi-line string, array or inline table that reads like a header or a
// key is text,
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
			case c == '[' || c == '{': // TOML 1.1 lets an inline table span lines
				depth++
			case (c == ']' || c == '}') && depth > 0:
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
// and with every key decoded (quotes and escapes), so that
// `[ mcp_servers."a2p2" ] # x`, `[mcp_servers."a2p2"]` and
// `[mcp_servers.a2p2]` are the same table. A key that needs quotes is
// quoted again, so `["mcp_servers.a2p2"]` stays another table. An array of
// tables gives "[[", which names no table a2p2 looks for, and a header
// a2p2 cannot read gives tomlUnread.
func tomlHeaderName(l string) string {
	parts, ok := tomlHeaderParts(l)
	switch {
	case !ok:
		return tomlUnread
	case parts == nil:
		return "[["
	}
	return "[" + tomlJoin(parts) + "]"
}

// tomlUnread is tomlHeaderName of a header a2p2 cannot read.
const tomlUnread = "[?]"

// tomlHeaderParts returns the decoded keys of a table header line: nil
// with ok for an array of tables, and ok false for a header a2p2 cannot
// read.
func tomlHeaderParts(l string) (parts []string, ok bool) {
	t := strings.TrimSpace(l)
	if strings.HasPrefix(t, "[[") {
		return nil, true
	}
	if !strings.HasPrefix(t, "[") {
		return nil, false
	}
	parts, rest, ok := tomlKeyParts(t[1:])
	if !ok || !strings.HasPrefix(rest, "]") {
		return nil, false
	}
	if rest = strings.TrimLeft(rest[1:], " \t\r"); rest != "" && rest[0] != '#' {
		return nil, false
	}
	return parts, true
}

func tomlJoin(parts []string) string {
	q := make([]string, len(parts))
	for i, p := range parts {
		q[i] = p
		if !bareKey(p) {
			q[i] = strconv.Quote(p)
		}
	}
	return strings.Join(q, ".")
}

// bareKey reports whether k is a TOML key that needs no quotes.
func bareKey(k string) bool {
	for i := 0; i < len(k); i++ {
		if !bareByte(k[i]) {
			return false
		}
	}
	return k != ""
}

func bareByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// tomlKeyParts reads the dotted key at the start of s: bare keys, basic
// strings with every escape TOML 1.0 and 1.1 define, and literal strings,
// with the spaces TOML allows around the dots. It returns the keys decoded
// and the text after the key. ok is false for anything else, so that a
// caller can refuse a file it does not fully read.
func tomlKeyParts(s string) (parts []string, rest string, ok bool) {
	for {
		s = strings.TrimLeft(s, " \t")
		var p string
		switch {
		case s == "":
			return nil, "", false
		case s[0] == '"':
			if p, s, ok = tomlBasic(s); !ok {
				return nil, "", false
			}
		case s[0] == '\'':
			j := strings.IndexByte(s[1:], '\'')
			if j < 0 {
				return nil, "", false
			}
			p, s = s[1:1+j], s[2+j:]
		default:
			j := 0
			for j < len(s) && bareByte(s[j]) {
				j++
			}
			if j == 0 {
				return nil, "", false
			}
			p, s = s[:j], s[j:]
		}
		parts = append(parts, p)
		s = strings.TrimLeft(s, " \t")
		if !strings.HasPrefix(s, ".") {
			return parts, s, true
		}
		s = s[1:]
	}
}

// tomlBasic decodes the one-line basic string at the start of s, which
// starts with a quote, and returns it and the text after it.
func tomlBasic(s string) (val, rest string, ok bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			return b.String(), s[i+1:], true
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i++; i >= len(s) {
			return "", "", false
		}
		n := 0
		switch s[i] {
		case 'b':
			b.WriteByte('\b')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'f':
			b.WriteByte('\f')
		case 'r':
			b.WriteByte('\r')
		case 'e':
			b.WriteByte(0x1b)
		case '"', '\\':
			b.WriteByte(s[i])
		case 'x':
			n = 2
		case 'u':
			n = 4
		case 'U':
			n = 8
		default:
			return "", "", false
		}
		if n > 0 {
			if i+n >= len(s) {
				return "", "", false
			}
			r, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
			if err != nil || !utf8.ValidRune(rune(r)) {
				return "", "", false
			}
			b.WriteRune(rune(r))
			i += n
		}
	}
	return "", "", false
}

// tomlKey returns the decoded dotted key of a key/value line and the text
// after its "=". ok is false when the line does not start with a key
// tomlKeyParts reads, followed by "=".
func tomlKey(l string) (key []string, value string, ok bool) {
	key, rest, ok := tomlKeyParts(l)
	if !ok || !strings.HasPrefix(rest, "=") {
		return nil, "", false
	}
	return key, rest[1:], true
}

// tomlValue returns a string value decoded, or any other value as
// written, without its comment. ok is false for a multi-line string a2p2
// does not decode: one that goes on past its line, or whose text holds a
// quote or a backslash. The rest it decodes, so that approve written
// between three double or three single quotes on one line reads as
// approve.
func tomlValue(v string) (val string, ok bool) {
	v = strings.TrimSpace(v)
	switch {
	case strings.HasPrefix(v, `"""`) || strings.HasPrefix(v, `'''`):
		q := v[:3]
		body, after, closed := strings.Cut(v[3:], q)
		if !closed || strings.ContainsAny(body, `"'\`) || strings.HasPrefix(after, q[:1]) {
			return "", false
		}
		return body, true
	case strings.HasPrefix(v, `"`):
		if s, _, ok := tomlBasic(v); ok {
			return s, true
		}
	case strings.HasPrefix(v, `'`):
		if j := strings.IndexByte(v[1:], '\''); j >= 0 {
			return v[1 : 1+j], true
		}
	}
	v, _, _ = strings.Cut(v, "#")
	return strings.TrimSpace(v), true
}

// tomlBlank reports whether a line holds no key: nothing, or a comment.
func tomlBlank(l string) bool {
	t := strings.TrimSpace(l)
	return t == "" || t[0] == '#'
}

// tomlTable is the decoded keys of a header line, for tomlLines' headers.
// An array of tables gets a name no key path a2p2 looks for starts with.
func tomlTable(l string) []string {
	parts, ok := tomlHeaderParts(l)
	if !ok || parts == nil {
		return []string{tomlUnread}
	}
	return parts
}

// mayLeadToApproval reports whether keys written under the table or
// inline table at path p can set an approval for a2p2's server: p is the
// top level, [mcp_servers], a2p2's table, its tools table or one tool's
// table. Other tables, such as a2p2's env, cannot, so a2p2 need not read
// them in full.
func mayLeadToApproval(p []string) bool {
	tools := []string{"mcp_servers", "a2p2", "tools"}
	switch {
	case len(p) <= len(tools):
		return slices.Equal(p, tools[:len(p)])
	case len(p) == len(tools)+1:
		return slices.Equal(p[:len(tools)], tools)
	}
	return false
}

// isApprovalKey reports whether a key under table sets
// default_tools_approval_mode for a2p2's server, however it is spelled.
func isApprovalKey(table, key []string) bool {
	return slices.Equal(slices.Concat(table, key), []string{"mcp_servers", "a2p2", "default_tools_approval_mode"})
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

// codexEdit is what codexApprovalEdit found.
type codexEdit struct {
	removed bool // it removed an approval line a2p2 wrote
	table   bool // [mcp_servers.a2p2] is there
	theirs  bool // that server already has a default_tools_approval_mode a2p2 did not write
	unread  int  // a line (1-based) that could set something in that server and that a2p2 cannot read; 0 if none
}

// codexApprovalEdit returns config.toml text s with the approval lines
// a2p2 wrote removed from a2p2's tables and, when approve is set,
// codexApproval added under [mcp_servers.a2p2]. It adds none when the
// server already has a default_tools_approval_mode in any spelling TOML
// allows (quoted, escaped, or dotted from [mcp_servers] or the top level),
// since a second one is a duplicate key and Codex would not read the file;
// nor when a line that could set one (a header, or a line of a table
// mayLeadToApproval names, an inline table among them) is one a2p2 cannot
// read, since then it cannot tell. Lines inside multi-line strings, arrays and inline
// tables, and lines of other tables, are left as they are.
func codexApprovalEdit(s string, approve bool) (string, codexEdit) {
	var e codexEdit
	lines := strings.Split(s, "\n")
	kind := tomlLines(lines)
	keep := make([]string, 0, len(lines)+1)
	var cur []string
	name, at := "", -1
	unread := func(i int) {
		if e.unread == 0 {
			e.unread = i + 1
		}
	}
	for i, l := range lines {
		if kind[i] == tomlHeader {
			name, cur = tomlHeaderName(l), tomlTable(l)
			switch {
			case name == tomlUnread:
				unread(i)
			case name == codexTable && at < 0:
				e.table, at = true, len(keep)
			}
		}
		ours := name == codexTable || strings.HasPrefix(name, "[mcp_servers.a2p2.")
		if kind[i] == tomlPlain && !tomlBlank(l) {
			key, v, ok := tomlKey(l)
			switch {
			case ours && isCodexApproval(l):
				e.removed = true
				continue
			case !ok && mayLeadToApproval(cur):
				unread(i)
			case !ok:
			case isApprovalKey(cur, key):
				e.theirs = true
			case strings.HasPrefix(strings.TrimSpace(v), "{") && mayLeadToApproval(slices.Concat(cur, key)):
				// An inline table that may hold the key; a2p2 does not read into it.
				unread(i)
			}
		}
		keep = append(keep, l)
	}
	if approve && e.table && !e.theirs && e.unread == 0 {
		keep = append(keep[:at+1], append([]string{codexApproval}, keep[at+1:]...)...)
	}
	return strings.Join(keep, "\n"), e
}

// editCodexApproval applies codexApprovalEdit to Codex's config.toml in one
// guarded write (see editFile), or only reads it on a dry run.
func editCodexApproval(o Options, approve bool) (codexEdit, error) {
	var e codexEdit
	path, err := codexConfig(o)
	if err != nil {
		return e, err
	}
	err = editFile(path, o.DryRun, func(s string) string {
		s, e = codexApprovalEdit(s, approve)
		return s
	})
	return e, err
}

// editFile replaces the file at path with edit of its text, unless the text
// is unchanged, the file is missing or dry is set. The write is guarded
// (see writeGuarded): when another program changed the file since it was
// read, editFile reads it and edits it again, up to three times, so that
// edit is kept.
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
		err = writeGuarded(path, []byte(out), b, true)
		if !errors.Is(err, errChanged) {
			return err
		}
	}
	return fmt.Errorf("%s kept changing while a2p2 edited it; a2p2 left it as it is. Close what is editing it, then run again", path)
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
	e, err := editCodexApproval(o, o.ApproveTools)
	if !o.ApproveTools {
		rep := []string{"tool approval: a2p2 approves none; Codex's own rules decide which a2p2 tools ask first " +
			"(to let " + all + " run without a prompt: a2p2 install codex --approve-tools)"}
		switch {
		case e.removed && o.DryRun:
			rep = append(rep, "would remove the tool approval an earlier a2p2 install added to "+path)
		case e.removed:
			rep = append(rep, "removed the tool approval an earlier a2p2 install added to "+path)
		}
		return rep, err
	}
	what := "let " + all + " run without a prompt, for this server only, in " + path + " (--approve-tools; uninstall removes it)"
	switch {
	case err != nil:
		return nil, err
	case e.theirs:
		return []string{"tool approval: " + path + " already sets default_tools_approval_mode for a2p2's server, and a2p2 did not write it; " +
			"a2p2 added nothing, since a second one would stop Codex reading the file"}, nil
	case e.unread > 0:
		return []string{fmt.Sprintf("tool approval: a2p2 cannot read line %d of %s, so it cannot tell whether a2p2's server already sets "+
			"default_tools_approval_mode; nothing was approved. To let %s run without a prompt, add this line under %s yourself, "+
			"unless the server sets that key already: default_tools_approval_mode = \"approve\"", e.unread, path, all, codexTable)}, nil
	case o.DryRun:
		return []string{"tool approval: would " + what}, nil
	case e.table:
		return []string{"tool approval: " + what}, nil
	default:
		return []string{"could not find " + codexTable + " in " + path + "; nothing was approved. Approve a2p2's tools in Codex yourself if you want them to run without a prompt"}, nil
	}
}

// codexApprovals reads what Codex is told to approve for a2p2's server
// without a prompt: the server-wide default_tools_approval_mode (mode,
// "" when unset, and by whom: "a2p2 --approve-tools", "a2p2 unasked" or
// "you"), and the tools approved one by one, under [mcp_servers.a2p2.tools.NAME]
// or as tools.NAME.approval_mode in a2p2's table, in any spelling TOML
// allows. found is false when the table is not there. unread is the first
// line (1-based) that could set one of these and that a2p2 cannot read, an
// inline table or a multi-line string it does not decode among them; 0 if
// none.
func codexApprovals(o Options) (found bool, mode, by string, tools []string, unread int, err error) {
	path, err := codexConfig(o)
	if err != nil {
		return false, "", "", nil, 0, err
	}
	b, ok, err := readMaybe(path)
	if err != nil || !ok {
		return false, "", "", nil, 0, err
	}
	var table []string
	lines := strings.Split(string(b), "\n")
	kind := tomlLines(lines)
	for i, l := range lines {
		if kind[i] == tomlInside {
			continue
		}
		if kind[i] == tomlHeader {
			table = tomlTable(l)
			switch tomlHeaderName(l) {
			case tomlUnread:
				unread = cmp.Or(unread, i+1)
			case codexTable:
				found = true
			}
			continue
		}
		if tomlBlank(l) {
			continue
		}
		key, v, ok := tomlKey(l)
		if !ok {
			if mayLeadToApproval(table) {
				unread = cmp.Or(unread, i+1)
			}
			continue
		}
		full := slices.Concat(table, key)
		switch {
		case isApprovalKey(table, key):
			var read bool
			if mode, read = tomlValue(v); !read {
				unread = cmp.Or(unread, i+1)
			}
			by = "you"
			switch strings.TrimSpace(l) {
			case codexApproval:
				by = "a2p2 --approve-tools"
			case codexApprovalUnasked:
				by = "a2p2 unasked"
			}
		case len(full) == 5 && slices.Equal(full[:3], []string{"mcp_servers", "a2p2", "tools"}) && full[4] == "approval_mode":
			switch val, read := tomlValue(v); {
			case !read:
				unread = cmp.Or(unread, i+1)
			case val == "approve":
				tools = append(tools, full[3])
			}
		case strings.HasPrefix(strings.TrimSpace(v), "{") && mayLeadToApproval(full):
			unread = cmp.Or(unread, i+1)
		}
	}
	return found, mode, by, tools, unread, nil
}

// codexApprovalCheck is doctor's line on what Codex runs without a prompt.
func codexApprovalCheck(o Options) Check {
	c := Check{Name: "codex tool approval"}
	path, _ := codexConfig(o)
	found, mode, by, tools, unread, err := codexApprovals(o)
	switch {
	case err != nil:
		c.Info = "cannot read " + path + ": " + err.Error()
		c.Fix = "a2p2 install codex"
	case unread > 0:
		c.Info = fmt.Sprintf("cannot tell which a2p2 tools Codex runs without a prompt: a2p2 cannot read line %d of %s", unread, path)
		c.Fix = "look for default_tools_approval_mode and approval_mode under a2p2's server in " + path
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
	e, err := editCodexApproval(o, false)
	return e.removed, err
}

// errChanged is the refusal of a guarded write or removal when the file
// changed since a2p2 read it.
var errChanged = errors.New("changed since a2p2 read it")

// beforeCheck runs just before a guarded write or removal compares the
// file; tests use it to edit the file at that moment.
var beforeCheck = func(path string) {}

// Every write a2p2 makes to a file a harness or the person also edits (the
// hook files, the harness CLIs' files, the adapters' files and their
// restores) is guarded: writeGuarded and removeGuarded read the file again
// just before the rename or removal and refuse, with errChanged, when it no
// longer holds what a2p2 read, so an edit made in the meantime is kept. The
// instant between that last read and the rename remains: closing it needs
// a lock that the harness also takes, and none of them has one.
//
// writeGuarded replaces path with b atomically, keeping its permission
// bits, if the file still holds was (or, when existed is false, is still
// missing). A symlink is followed (see resolve); a dangling one is refused.
func writeGuarded(path string, b, was []byte, existed bool) error {
	return replaceFile(path, b, func(path string) error { return unchanged(path, was, existed) })
}

// removeGuarded removes path if it still holds was; a file already gone is
// not an error.
func removeGuarded(path string, was []byte) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := unchanged(path, was, true); err != nil {
		return err
	}
	return os.Remove(path)
}

// unchanged returns errChanged, with a message for the person, unless the
// file at path holds was (or, when existed is false, is missing).
func unchanged(path string, was []byte, existed bool) error {
	beforeCheck(path)
	cur, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && !existed:
		return nil
	case err == nil && existed && bytes.Equal(cur, was):
		return nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	return fmt.Errorf("%s %w, so a2p2 left it as it is; run the command again", path, errChanged)
}

// writeOwn replaces a file only a2p2 writes (its install records)
// atomically, keeping its permission bits.
func writeOwn(path string, b []byte) error {
	return replaceFile(path, b, nil)
}

// replaceFile writes b to a temporary file beside path and renames it into
// place, after check, when set, accepts the file at path.
func replaceFile(path string, b []byte, check func(path string) error) error {
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
	if check != nil {
		if err := check(path); err != nil {
			return err
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
				if key, v, ok := tomlKey(x); ok && slices.Equal(key, []string{"command"}) {
					command, _ = tomlValue(v)
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
