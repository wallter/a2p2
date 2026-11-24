package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Hermes Agent (seen with 0.15.2) reads mcp_servers from
// $HERMES_HOME/config.yaml (default ~/.hermes). a2p2 has no YAML library,
// so it edits the text: it adds, replaces or removes only the a2p2 entry
// under a block-style top-level mcp_servers key, keeping the file's line
// endings, comments and final newline. Any layout it cannot follow (a
// document that is not a block mapping, flow style, tabs, several
// documents, mixed line endings) is refused before anything is written,
// and every computed text is checked before it is written: it parses
// back, its a2p2 entry is the one intended, and it still holds every
// original line outside a2p2's entry, in order.
//
// Hermes passes a stdio server only PATH, HOME, USER, LANG, LC_ALL, TERM,
// SHELL, TMPDIR and XDG_* from its own environment, so an A2P2_HOME set
// at install time is written into the entry.
var hermesAdapter = fileAdapter{
	cli:   "hermes",
	path:  hermesPath,
	what:  "MCP server",
	add:   hermesAdd,
	del:   hermesDel,
	prior: hermesPrior,
	put:   hermesPut,
	empty: func(b []byte) bool { return len(bytes.TrimSpace(b)) == 0 },
	same:  hermesSame,
	find:  hermesFind,
	notes: []string{"no hook installed for Hermes; its tools appear as mcp_a2p2_<tool>, and it needs a model context of at least 64K tokens"},
}

func hermesPath(o Options) (string, error) {
	dir, err := envOr(o, "HERMES_HOME", ".hermes")
	return filepath.Join(dir, "config.yaml"), err
}

// yamlDoc is config.yaml as lines without their endings.
type yamlDoc struct {
	lines   []string
	eol     string // "\n" or "\r\n"
	finalNL bool   // the last line ended with eol
	head    int    // the mcp_servers line, -1 if none
	headVal bool   // that line has a value ({}, null or ~) instead of starting a block
	end     int    // first line after the mcp_servers block
	indent  string // indentation of the block's entries ("" if none yet)
	entry   int    // first line of the a2p2 entry, -1 if none
	eod     int    // first line after it
}

func indentOf(l string) string { return l[:len(l)-len(strings.TrimLeft(l, " \t"))] }

func significant(l string) bool {
	t := strings.TrimSpace(l)
	return t != "" && !strings.HasPrefix(t, "#")
}

// keyIs reports whether a trimmed YAML line is the mapping key k, and what
// follows the colon, without a trailing comment.
func keyIs(t, k string) (rest string, ok bool) {
	for _, q := range []string{k, `"` + k + `"`, `'` + k + `'`} {
		if r, ok := strings.CutPrefix(t, q+":"); ok && (r == "" || r[0] == ' ' || r[0] == '\t') {
			r = strings.TrimSpace(r)
			if strings.HasPrefix(r, "#") {
				r = ""
			} else if i := strings.Index(r, " #"); i >= 0 {
				r = strings.TrimSpace(r[:i])
			}
			return r, true
		}
	}
	return "", false
}

// isKeyLine reports whether a trimmed line starts a mapping entry "key:".
func isKeyLine(t string) bool {
	if t == "" || strings.ContainsRune("-[{>|!&*?%@`", rune(t[0])) {
		return false
	}
	if t[0] == '"' || t[0] == '\'' {
		j := strings.IndexByte(t[1:], t[0])
		return j >= 0 && strings.HasPrefix(t[j+2:], ":")
	}
	i := strings.Index(t, ":")
	return i > 0 && (i == len(t)-1 || t[i+1] == ' ' || t[i+1] == '\t')
}

var errYAML = errors.New("yaml")

func refuseYAML(why string) error {
	return fmt.Errorf("%w: config.yaml %s; a2p2 will not edit it. Add an a2p2 entry under mcp_servers yourself", errYAML, why)
}

func parseHermes(b []byte) (*yamlDoc, error) {
	s := string(b)
	y := &yamlDoc{eol: "\n", head: -1, entry: -1, eod: -1}
	crlf, lf := strings.Count(s, "\r\n"), strings.Count(s, "\n")
	switch {
	case crlf > 0 && crlf == lf:
		y.eol = "\r\n"
	case crlf > 0:
		return nil, refuseYAML("mixes line endings")
	}
	if s != "" {
		y.finalNL = strings.HasSuffix(s, y.eol)
		y.lines = strings.Split(strings.TrimSuffix(s, y.eol), y.eol)
	}
	if strings.ContainsRune(strings.Join(y.lines, ""), '\r') {
		return nil, refuseYAML("has a carriage return inside a line")
	}
	first := true
	for i, l := range y.lines {
		if !significant(l) {
			continue
		}
		ind := indentOf(l)
		if strings.Contains(ind, "\t") {
			return nil, refuseYAML("is indented with tabs")
		}
		if ind != "" {
			if first {
				return nil, refuseYAML("does not start at the left margin")
			}
			continue
		}
		t := strings.TrimSpace(l)
		if t == "---" && first {
			continue
		}
		first = false
		if t == "---" || strings.HasPrefix(t, "--- ") || t == "..." {
			return nil, refuseYAML("holds more than one document")
		}
		if !isKeyLine(t) {
			return nil, refuseYAML("is not a block mapping")
		}
		rest, ok := keyIs(t, "mcp_servers")
		if !ok {
			continue
		}
		if y.head >= 0 {
			return nil, refuseYAML("has mcp_servers twice")
		}
		switch rest {
		case "":
		case "{}", "null", "~":
			y.headVal = true
		default:
			return nil, refuseYAML("has mcp_servers in flow style or as a scalar")
		}
		y.head = i
	}
	if y.head < 0 {
		return y, nil
	}
	y.end = len(y.lines)
	for i := y.head + 1; i < len(y.lines); i++ {
		if significant(y.lines[i]) && indentOf(y.lines[i]) == "" {
			y.end = i
			break
		}
	}
	for i := y.head + 1; i < y.end; i++ {
		l := y.lines[i]
		if !significant(l) {
			continue
		}
		if y.headVal {
			return nil, refuseYAML("has a value for mcp_servers and entries under it")
		}
		if y.indent == "" {
			y.indent = indentOf(l)
		}
		ind := indentOf(l)
		if len(ind) < len(y.indent) {
			return nil, refuseYAML("has an mcp_servers block a2p2 cannot follow")
		}
		if ind != y.indent {
			continue
		}
		t := strings.TrimSpace(l)
		if !isKeyLine(t) {
			return nil, refuseYAML("has mcp_servers that is not a mapping")
		}
		if y.entry >= 0 && y.eod < 0 {
			y.eod = i
		}
		if _, ok := keyIs(t, "a2p2"); ok {
			if y.entry >= 0 {
				return nil, refuseYAML("has two a2p2 entries")
			}
			y.entry, y.eod = i, -1
		}
	}
	if y.entry >= 0 && y.eod < 0 {
		y.eod = y.end
	}
	if y.entry >= 0 {
		// Blank lines and comments after the entry belong to what follows.
		for y.eod > y.entry+1 && !significant(y.lines[y.eod-1]) {
			y.eod--
		}
	}
	return y, nil
}

func (y *yamlDoc) text(lines []string, finalNL bool) []byte {
	if len(lines) == 0 {
		return nil
	}
	s := strings.Join(lines, y.eol)
	if finalNL {
		s += y.eol
	}
	return []byte(s)
}

func yamlString(s string) string {
	b, _ := json.Marshal(s) // a JSON string is a valid YAML double-quoted scalar
	return string(b)
}

func hermesEntry(indent string, o Options) []string {
	args := []string{}
	for _, a := range []string{"mcp", "--harness", "hermes"} {
		args = append(args, yamlString(a))
	}
	e := []string{
		indent + "a2p2:",
		indent + indent + "command: " + yamlString(o.Binary),
		indent + indent + "args: [" + strings.Join(args, ", ") + "]",
	}
	if h := os.Getenv("A2P2_HOME"); h != "" && o.HomeDir == "" {
		e = append(e, indent+indent+"env:", indent+indent+indent+"A2P2_HOME: "+yamlString(h))
	}
	return e
}

// withEntry returns the lines with entry put where a2p2's entry is, or
// first under mcp_servers (made a block, or added at the end).
func (y *yamlDoc) withEntry(entry []string) (lines []string, finalNL bool) {
	finalNL = y.finalNL
	switch {
	case y.head < 0:
		lines = append(slices.Clone(y.lines), "mcp_servers:")
		return append(lines, entry...), true
	case y.entry >= 0:
		lines = slices.Concat(y.lines[:y.entry], entry, y.lines[y.eod:])
	default:
		h := y.lines[y.head]
		if y.headVal {
			h = "mcp_servers:"
			if i := strings.Index(y.lines[y.head], " #"); i >= 0 {
				h += y.lines[y.head][i:]
			}
		}
		lines = slices.Concat(y.lines[:y.head], []string{h}, entry, y.lines[y.head+1:])
	}
	if y.head == len(y.lines)-1 || (y.entry >= 0 && y.eod == len(y.lines)) {
		finalNL = true // the file now goes on past what was its last line
	}
	return lines, finalNL
}

// checkHermes parses the new text back and checks that it holds an a2p2
// entry exactly when want is not nil, equal to want, and every line of
// keep in order.
func checkHermes(b []byte, want []string, keep []string) error {
	y, err := parseHermes(b)
	if err != nil {
		return fmt.Errorf("a2p2 computed a config.yaml it cannot read back (%v); nothing was written", err)
	}
	switch {
	case want == nil && y.entry >= 0:
		return errors.New("a2p2's entry is still in the computed config.yaml; nothing was written")
	case want != nil && (y.entry < 0 || !slices.Equal(y.lines[y.entry:y.eod], want)):
		return errors.New("the computed config.yaml does not hold the intended a2p2 entry; nothing was written")
	}
	rest := y.lines
	if y.entry >= 0 {
		rest = slices.Concat(y.lines[:y.entry], y.lines[y.eod:])
	}
	i := 0
	for _, l := range rest {
		if i < len(keep) && l == keep[i] {
			i++
		}
	}
	if i != len(keep) {
		return errors.New("the computed config.yaml lost a line of the original; nothing was written")
	}
	return nil
}

// outside is every line not in a2p2's entry, and not the mcp_servers line
// when that line holds a value (it is rewritten as a block).
func (y *yamlDoc) outside() []string {
	var out []string
	for i, l := range y.lines {
		if (y.entry >= 0 && i >= y.entry && i < y.eod) || (y.headVal && i == y.head) {
			continue
		}
		out = append(out, l)
	}
	return out
}

func hermesAdd(b []byte, o Options) ([]byte, error) {
	y, err := parseHermes(b)
	if err != nil {
		return nil, err
	}
	ind := y.indent
	if ind == "" {
		ind = "  "
	}
	e := hermesEntry(ind, o)
	if y.entry >= 0 && slices.Equal(y.lines[y.entry:y.eod], e) {
		return b, nil
	}
	out := y.text(y.withEntry(e))
	return out, checkHermes(out, e, y.outside())
}

func hermesDel(b []byte) ([]byte, bool, error) {
	y, err := parseHermes(b)
	if err != nil {
		return nil, false, err
	}
	if y.entry < 0 {
		return b, false, nil
	}
	lines := slices.Concat(y.lines[:y.entry], y.lines[y.eod:])
	keep := y.outside()
	// No servers left and nothing else in the block: drop the bare key, as
	// hermes mcp remove does.
	left := false
	for _, l := range lines[y.head+1 : y.end-(y.eod-y.entry)] {
		left = left || strings.TrimSpace(l) != ""
	}
	if !left && strings.TrimSpace(lines[y.head]) == "mcp_servers:" {
		lines = slices.Delete(lines, y.head, y.head+1)
		keep = slices.Delete(slices.Clone(keep), y.head, y.head+1)
	}
	// The line before an entry that ended the file was followed by a line
	// ending; it keeps it.
	out := y.text(lines, y.finalNL || y.eod == len(y.lines))
	return out, true, checkHermes(out, nil, keep)
}

// hermesPrior is the text of an existing a2p2 entry, lines joined by "\n".
func hermesPrior(b []byte) (bool, string, error) {
	y, err := parseHermes(b)
	if err != nil || y.entry < 0 {
		return false, "", err
	}
	return true, strings.Join(y.lines[y.entry:y.eod], "\n"), nil
}

// hermesPut puts a recorded entry back after a2p2's own was removed.
func hermesPut(b []byte, prior string) ([]byte, error) {
	y, err := parseHermes(b)
	if err != nil {
		return nil, err
	}
	if y.entry >= 0 {
		return nil, refuseYAML("already has an a2p2 entry again")
	}
	e := strings.Split(prior, "\n")
	out := y.text(y.withEntry(e))
	return out, checkHermes(out, e, y.outside())
}

// hermesSame compares two config.yaml texts as uninstall leaves them: an
// empty mcp_servers key and a missing final newline do not count.
func hermesSame(a, b []byte) bool {
	norm := func(x []byte) string {
		y, err := parseHermes(x)
		if err != nil {
			return string(x)
		}
		lines := y.lines
		if y.head >= 0 && y.indent == "" { // an mcp_servers key with no servers
			lines = slices.Delete(slices.Clone(lines), y.head, y.head+1)
		}
		return strings.Join(lines, "\n")
	}
	return norm(a) == norm(b)
}

func hermesFind(o Options) (bool, string, error) {
	path, err := hermesPath(o)
	if err != nil {
		return false, "", err
	}
	b, ok, err := readMaybe(path)
	if err != nil || !ok {
		return false, "", err
	}
	y, err := parseHermes(b)
	if err != nil || y.entry < 0 {
		return false, "", err
	}
	for _, l := range y.lines[y.entry+1 : y.eod] {
		if v, ok := keyIs(strings.TrimSpace(l), "command"); ok {
			var s string
			if json.Unmarshal([]byte(v), &s) == nil {
				return true, s, nil
			}
			return true, strings.Trim(v, `'`), nil
		}
	}
	return true, "", nil
}
